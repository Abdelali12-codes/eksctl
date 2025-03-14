package cluster

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/request"
	awseks "github.com/aws/aws-sdk-go/service/eks"
	"github.com/kris-nova/logger"
	"github.com/pkg/errors"

	"github.com/weaveworks/eksctl/pkg/actions/nodegroup"
	api "github.com/weaveworks/eksctl/pkg/apis/eksctl.io/v1alpha5"
	"github.com/weaveworks/eksctl/pkg/cfn/manager"
	"github.com/weaveworks/eksctl/pkg/ctl/cmdutils"
	"github.com/weaveworks/eksctl/pkg/eks"
	iamoidc "github.com/weaveworks/eksctl/pkg/iam/oidc"
	"github.com/weaveworks/eksctl/pkg/kubernetes"
	"github.com/weaveworks/eksctl/pkg/utils/tasks"
	"github.com/weaveworks/eksctl/pkg/utils/waiters"
)

type UnownedCluster struct {
	cfg                 *api.ClusterConfig
	ctl                 *eks.ClusterProvider
	stackManager        manager.StackManager
	newClientSet        func() (kubernetes.Interface, error)
	newNodeGroupManager func(cfg *api.ClusterConfig, ctl *eks.ClusterProvider, clientSet kubernetes.Interface) NodeGroupDrainer
}

func NewUnownedCluster(cfg *api.ClusterConfig, ctl *eks.ClusterProvider, stackManager manager.StackManager) *UnownedCluster {
	return &UnownedCluster{
		cfg:          cfg,
		ctl:          ctl,
		stackManager: stackManager,
		newClientSet: func() (kubernetes.Interface, error) {
			return ctl.NewStdClientSet(cfg)
		},
		newNodeGroupManager: func(cfg *api.ClusterConfig, ctl *eks.ClusterProvider, clientSet kubernetes.Interface) NodeGroupDrainer {
			return nodegroup.New(cfg, ctl, clientSet)
		},
	}
}

func (c *UnownedCluster) Upgrade(_ context.Context, dryRun bool) error {
	versionUpdateRequired, err := upgrade(c.cfg, c.ctl, dryRun)
	if err != nil {
		return err
	}

	// if no version update is required, don't log asking them to rerun with --approve
	cmdutils.LogPlanModeWarning(dryRun && versionUpdateRequired)
	return nil
}

func (c *UnownedCluster) Delete(ctx context.Context, waitInterval time.Duration, wait, force, disableNodegroupEviction bool, parallel int) error {
	clusterName := c.cfg.Metadata.Name

	if err := c.checkClusterExists(clusterName); err != nil {
		return err
	}

	clusterOperable, err := c.ctl.CanOperate(c.cfg)
	if err != nil {
		logger.Debug("failed to check if cluster is operable: %v", err)
	}

	allStacks, err := c.stackManager.ListNodeGroupStacks()
	if err != nil {
		return err
	}

	var clientSet kubernetes.Interface
	if clusterOperable {
		clientSet, err = c.newClientSet()
		if err != nil {
			return err
		}

		nodeGroupManager := c.newNodeGroupManager(c.cfg, c.ctl, clientSet)
		if err := drainAllNodeGroups(c.cfg, c.ctl, clientSet, allStacks, disableNodegroupEviction, parallel, nodeGroupManager, attemptVpcCniDeletion); err != nil {
			if !force {
				return err
			}

			logger.Warning("an error occurred during nodegroups draining, force=true so proceeding with deletion: %q", err.Error())
		}
	}

	if err := deleteSharedResources(ctx, c.cfg, c.ctl, c.stackManager, clusterOperable, clientSet); err != nil {
		if err != nil {
			if force {
				logger.Warning("error occurred during deletion: %v", err)
			} else {
				return err
			}
		}
	}

	if err := c.deleteFargateRoleIfExists(); err != nil {
		return err
	}

	// we have to wait for nodegroups to delete before deleting the cluster
	// so the `wait` value is ignored here
	if err := c.deleteAndWaitForNodegroupsDeletion(waitInterval, allStacks); err != nil {
		return err
	}

	if err := c.deleteIAMAndOIDC(ctx, wait, clusterOperable, clientSet); err != nil {
		if err != nil {
			if force {
				logger.Warning("error occurred during deletion: %v", err)
			} else {
				return err
			}
		}
	}

	if err := c.deleteCluster(wait); err != nil {
		return err
	}

	if err := checkForUndeletedStacks(c.stackManager); err != nil {
		return err
	}

	logger.Success("all cluster resources were deleted")
	return nil
}

func (c *UnownedCluster) deleteFargateRoleIfExists() error {
	stack, err := c.stackManager.GetFargateStack()
	if err != nil {
		return err
	}

	if stack != nil {
		logger.Info("deleting fargate role")
		_, err = c.stackManager.DeleteStackBySpec(stack)
		return err
	}

	logger.Debug("no fargate role found")
	return nil
}

func (c *UnownedCluster) checkClusterExists(clusterName string) error {
	_, err := c.ctl.Provider.EKS().DescribeCluster(&awseks.DescribeClusterInput{
		Name: &c.cfg.Metadata.Name,
	})
	if err != nil {
		if isNotFound(err) {
			return errors.Errorf("cluster %q not found", clusterName)
		}
		return errors.Wrapf(err, "error describing cluster %q", clusterName)
	}
	return nil
}

func (c *UnownedCluster) deleteIAMAndOIDC(ctx context.Context, wait bool, clusterOperable bool, clientSet kubernetes.Interface) error {
	var oidc *iamoidc.OpenIDConnectManager
	oidcSupported := true

	if clusterOperable {
		var err error
		oidc, err = c.ctl.NewOpenIDConnectManager(c.cfg)
		if err != nil {
			if _, ok := err.(*eks.UnsupportedOIDCError); !ok {
				return err
			}
			oidcSupported = false
		}
	}

	tasksTree := &tasks.TaskTree{Parallel: false}

	if clusterOperable && oidcSupported {
		clientSetGetter := kubernetes.NewCachedClientSet(clientSet)
		serviceAccountAndOIDCTasks, err := c.stackManager.NewTasksToDeleteOIDCProviderWithIAMServiceAccounts(ctx, oidc, clientSetGetter)
		if err != nil {
			return err
		}

		if serviceAccountAndOIDCTasks.Len() > 0 {
			serviceAccountAndOIDCTasks.IsSubTask = true
			tasksTree.Append(serviceAccountAndOIDCTasks)
		}
	}

	deleteAddonIAMtasks, err := c.stackManager.NewTaskToDeleteAddonIAM(wait)
	if err != nil {
		return err
	}

	if deleteAddonIAMtasks.Len() > 0 {
		deleteAddonIAMtasks.IsSubTask = true
		tasksTree.Append(deleteAddonIAMtasks)
	}

	if tasksTree.Len() == 0 {
		logger.Warning("no IAM and OIDC resources were found for %q", c.cfg.Metadata.Name)
		return nil
	}

	logger.Info(tasksTree.Describe())
	if errs := tasksTree.DoAllSync(); len(errs) > 0 {
		return handleErrors(errs, "cluster IAM and OIDC")
	}

	logger.Info("all IAM and OIDC resources were deleted")
	return nil
}

func (c *UnownedCluster) deleteCluster(wait bool) error {
	clusterName := c.cfg.Metadata.Name

	out, err := c.ctl.Provider.EKS().DeleteCluster(&awseks.DeleteClusterInput{
		Name: &clusterName,
	})

	if err != nil {
		return err
	}

	logger.Info("initiated deletion of cluster %q", clusterName)
	if out != nil {
		logger.Debug("delete cluster response: %s", out.String())
	}

	if !wait {
		logger.Info("to see the status of the deletion run `eksctl get cluster --name %s --region %s`", clusterName, c.cfg.Metadata.Region)
		return nil
	}
	newRequest := func() *request.Request {
		input := &awseks.DescribeClusterInput{
			Name: &clusterName,
		}
		req, _ := c.ctl.Provider.EKS().DescribeClusterRequest(input)
		return req
	}

	acceptors := waiters.MakeErrorCodeAcceptors(awseks.ErrCodeResourceNotFoundException)

	msg := fmt.Sprintf("waiting for cluster %q to be deleted", clusterName)

	return waiters.Wait(clusterName, msg, acceptors, newRequest, c.ctl.Provider.WaitTimeout(), nil)
}

func (c *UnownedCluster) deleteAndWaitForNodegroupsDeletion(waitInterval time.Duration, allStacks []manager.NodeGroupStack) error {
	clusterName := c.cfg.Metadata.Name
	eksAPI := c.ctl.Provider.EKS()

	// get all managed nodegroups for this cluster
	nodeGroups, err := eksAPI.ListNodegroups(&awseks.ListNodegroupsInput{
		ClusterName: &clusterName,
	})
	if err != nil {
		return err
	}

	if len(allStacks) == 0 && len(nodeGroups.Nodegroups) == 0 {
		logger.Warning("no nodegroups found for %s", clusterName)
		return nil
	}

	// we kill every nodegroup with a stack the standard way. wait is always true
	tasks, err := c.stackManager.NewTasksToDeleteNodeGroups(allStacks, func(_ string) bool { return true }, true, nil)
	if err != nil {
		return err
	}

	for _, n := range nodeGroups.Nodegroups {
		isUnowned := func() bool {
			for _, stack := range allStacks {
				if stack.NodeGroupName == *n {
					return false
				}
			}
			return true
		}

		if isUnowned() {
			// if a managed ng does not have a stack, we queue if for deletion via api
			tasks.Append(c.stackManager.NewTaskToDeleteUnownedNodeGroup(clusterName, *n, eksAPI, c.waitForUnownedNgsDeletion(waitInterval)))
		}
	}

	// TODO what dis?
	tasks.PlanMode = false
	logger.Info(tasks.Describe())
	if errs := tasks.DoAllSync(); len(errs) > 0 {
		return handleErrors(errs, "nodegroup(s)")
	}
	return nil
}

func isNotFound(err error) bool {
	awsError, ok := err.(awserr.Error)
	return ok && awsError.Code() == awseks.ErrCodeResourceNotFoundException
}

func (c *UnownedCluster) waitForUnownedNgsDeletion(interval time.Duration) *manager.DeleteWaitCondition {
	condition := func() (bool, error) {
		nodeGroups, err := c.ctl.Provider.EKS().ListNodegroups(&awseks.ListNodegroupsInput{
			ClusterName: &c.cfg.Metadata.Name,
		})
		if err != nil {
			return false, err
		}
		if len(nodeGroups.Nodegroups) == 0 {
			return true, nil
		}

		logger.Info("waiting for all non eksctl-owned nodegroups to be deleted")
		return false, nil
	}

	return &manager.DeleteWaitCondition{
		Condition: condition,
		Timeout:   c.ctl.Provider.WaitTimeout(),
		Interval:  interval,
	}
}

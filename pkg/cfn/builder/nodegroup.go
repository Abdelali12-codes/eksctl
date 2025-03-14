package builder

import (
	"context"
	"fmt"
	"strings"

	cfn "github.com/aws/aws-sdk-go/service/cloudformation"

	"github.com/pkg/errors"
	gfn "github.com/weaveworks/goformation/v4/cloudformation"
	gfncfn "github.com/weaveworks/goformation/v4/cloudformation/cloudformation"
	gfnec2 "github.com/weaveworks/goformation/v4/cloudformation/ec2"
	gfnt "github.com/weaveworks/goformation/v4/cloudformation/types"

	"github.com/kris-nova/logger"

	api "github.com/weaveworks/eksctl/pkg/apis/eksctl.io/v1alpha5"
	"github.com/weaveworks/eksctl/pkg/awsapi"
	"github.com/weaveworks/eksctl/pkg/cfn/outputs"
	"github.com/weaveworks/eksctl/pkg/nodebootstrap"
	"github.com/weaveworks/eksctl/pkg/vpc"
)

// MaximumTagNumber for ASGs as described here https://docs.aws.amazon.com/autoscaling/ec2/userguide/autoscaling-tagging.html
const MaximumTagNumber = 50

// NodeGroupResourceSet stores the resource information of the nodegroup
type NodeGroupResourceSet struct {
	rs                *resourceSet
	clusterSpec       *api.ClusterConfig
	spec              *api.NodeGroup
	forceAddCNIPolicy bool
	ec2API            awsapi.EC2

	iamAPI             awsapi.IAM
	instanceProfileARN *gfnt.Value
	securityGroups     []*gfnt.Value
	vpc                *gfnt.Value
	vpcImporter        vpc.Importer
	bootstrapper       nodebootstrap.Bootstrapper
}

// NewNodeGroupResourceSet returns a resource set for a nodegroup embedded in a cluster config
func NewNodeGroupResourceSet(ec2API awsapi.EC2, iamAPI awsapi.IAM, spec *api.ClusterConfig, ng *api.NodeGroup, bootstrapper nodebootstrap.Bootstrapper, forceAddCNIPolicy bool, vpcImporter vpc.Importer) *NodeGroupResourceSet {
	return &NodeGroupResourceSet{
		rs:                newResourceSet(),
		forceAddCNIPolicy: forceAddCNIPolicy,
		clusterSpec:       spec,
		spec:              ng,
		ec2API:            ec2API,
		iamAPI:            iamAPI,
		vpcImporter:       vpcImporter,
		bootstrapper:      bootstrapper,
	}
}

// AddAllResources adds all the information about the nodegroup to the resource set
func (n *NodeGroupResourceSet) AddAllResources(ctx context.Context) error {

	if n.clusterSpec.IPv6Enabled() {
		return errors.New("unmanaged nodegroups are not supported with IPv6 clusters")
	}

	n.rs.template.Description = fmt.Sprintf(
		"%s (AMI family: %s, SSH access: %v, private networking: %v) %s",
		nodeGroupTemplateDescription,
		n.spec.AMIFamily, api.IsEnabled(n.spec.SSH.Allow), n.spec.PrivateNetworking,
		templateDescriptionSuffix)

	n.Template().Mappings[servicePrincipalPartitionMapName] = servicePrincipalPartitionMappings

	n.rs.defineOutputWithoutCollector(outputs.NodeGroupFeaturePrivateNetworking, n.spec.PrivateNetworking, false)
	n.rs.defineOutputWithoutCollector(outputs.NodeGroupFeatureSharedSecurityGroup, n.spec.SecurityGroups.WithShared, false)
	n.rs.defineOutputWithoutCollector(outputs.NodeGroupFeatureLocalSecurityGroup, n.spec.SecurityGroups.WithLocal, false)

	n.vpc = n.vpcImporter.VPC()

	if n.spec.Tags == nil {
		n.spec.Tags = map[string]string{}
	}

	for k, v := range n.clusterSpec.Metadata.Tags {
		if _, exists := n.spec.Tags[k]; !exists {
			n.spec.Tags[k] = v
		}
	}

	// Ensure MinSize is set, as it is required by the ASG cfn resource
	// TODO this validation and default setting should happen way earlier than this
	if n.spec.MinSize == nil {
		if n.spec.DesiredCapacity == nil {
			defaultNodeCount := api.DefaultNodeCount
			n.spec.MinSize = &defaultNodeCount
		} else {
			n.spec.MinSize = n.spec.DesiredCapacity
		}
		logger.Info("--nodes-min=%d was set automatically for nodegroup %s", *n.spec.MinSize, n.spec.Name)
	} else if n.spec.DesiredCapacity != nil && *n.spec.DesiredCapacity < *n.spec.MinSize {
		return fmt.Errorf("--nodes value (%d) cannot be lower than --nodes-min value (%d)", *n.spec.DesiredCapacity, *n.spec.MinSize)
	}

	// Ensure MaxSize is set, as it is required by the ASG cfn resource
	if n.spec.MaxSize == nil {
		if n.spec.DesiredCapacity == nil {
			n.spec.MaxSize = n.spec.MinSize
		} else {
			n.spec.MaxSize = n.spec.DesiredCapacity
		}
		logger.Info("--nodes-max=%d was set automatically for nodegroup %s", *n.spec.MaxSize, n.spec.Name)
	} else if n.spec.DesiredCapacity != nil && *n.spec.DesiredCapacity > *n.spec.MaxSize {
		return fmt.Errorf("--nodes value (%d) cannot be greater than --nodes-max value (%d)", *n.spec.DesiredCapacity, *n.spec.MaxSize)
	} else if *n.spec.MaxSize < *n.spec.MinSize {
		return fmt.Errorf("--nodes-min value (%d) cannot be greater than --nodes-max value (%d)", *n.spec.MinSize, *n.spec.MaxSize)
	}

	if err := n.addResourcesForIAM(ctx); err != nil {
		return err
	}
	n.addResourcesForSecurityGroups()

	return n.addResourcesForNodeGroup(ctx)
}

func (n *NodeGroupResourceSet) addResourcesForSecurityGroups() {
	for _, id := range n.spec.SecurityGroups.AttachIDs {
		n.securityGroups = append(n.securityGroups, gfnt.NewString(id))
	}

	if api.IsEnabled(n.spec.SecurityGroups.WithShared) {
		n.securityGroups = append(n.securityGroups, n.vpcImporter.SharedNodeSecurityGroup())
	}

	if api.IsDisabled(n.spec.SecurityGroups.WithLocal) {
		return
	}

	desc := "worker nodes in group " + n.spec.Name
	vpcID := n.vpcImporter.VPC()
	refControlPlaneSG := n.vpcImporter.ControlPlaneSecurityGroup()

	refNodeGroupLocalSG := n.newResource("SG", &gfnec2.SecurityGroup{
		VpcId:            vpcID,
		GroupDescription: gfnt.NewString("Communication between the control plane and " + desc),
		Tags: []gfncfn.Tag{{
			Key:   gfnt.NewString("kubernetes.io/cluster/" + n.clusterSpec.Metadata.Name),
			Value: gfnt.NewString("owned"),
		}},
		SecurityGroupIngress: makeNodeIngressRules(n.spec.NodeGroupBase, refControlPlaneSG, n.clusterSpec.VPC.CIDR.String(), desc),
	})

	n.securityGroups = append(n.securityGroups, refNodeGroupLocalSG)

	if api.IsEnabled(n.spec.EFAEnabled) {
		efaSG := n.rs.addEFASecurityGroup(vpcID, n.clusterSpec.Metadata.Name, desc)
		n.securityGroups = append(n.securityGroups, efaSG)
	}

	n.newResource("EgressInterCluster", &gfnec2.SecurityGroupEgress{
		GroupId:                    refControlPlaneSG,
		DestinationSecurityGroupId: refNodeGroupLocalSG,
		Description:                gfnt.NewString("Allow control plane to communicate with " + desc + " (kubelet and workload TCP ports)"),
		IpProtocol:                 sgProtoTCP,
		FromPort:                   sgMinNodePort,
		ToPort:                     sgMaxNodePort,
	})
	n.newResource("EgressInterClusterAPI", &gfnec2.SecurityGroupEgress{
		GroupId:                    refControlPlaneSG,
		DestinationSecurityGroupId: refNodeGroupLocalSG,
		Description:                gfnt.NewString("Allow control plane to communicate with " + desc + " (workloads using HTTPS port, commonly used with extension API servers)"),
		IpProtocol:                 sgProtoTCP,
		FromPort:                   sgPortHTTPS,
		ToPort:                     sgPortHTTPS,
	})
	n.newResource("IngressInterClusterCP", &gfnec2.SecurityGroupIngress{
		GroupId:               refControlPlaneSG,
		SourceSecurityGroupId: refNodeGroupLocalSG,
		Description:           gfnt.NewString("Allow control plane to receive API requests from " + desc),
		IpProtocol:            sgProtoTCP,
		FromPort:              sgPortHTTPS,
		ToPort:                sgPortHTTPS,
	})
}

func makeNodeIngressRules(ng *api.NodeGroupBase, controlPlaneSG *gfnt.Value, vpcCIDR, description string) []gfnec2.SecurityGroup_Ingress {
	ingressRules := []gfnec2.SecurityGroup_Ingress{
		{
			SourceSecurityGroupId: controlPlaneSG,
			Description:           gfnt.NewString(fmt.Sprintf("[IngressInterCluster] Allow %s to communicate with control plane (kubelet and workload TCP ports)", description)),
			IpProtocol:            sgProtoTCP,
			FromPort:              sgMinNodePort,
			ToPort:                sgMaxNodePort,
		},
		{
			SourceSecurityGroupId: controlPlaneSG,
			Description:           gfnt.NewString(fmt.Sprintf("[IngressInterClusterAPI] Allow %s to communicate with control plane (workloads using HTTPS port, commonly used with extension API servers)", description)),
			IpProtocol:            sgProtoTCP,
			FromPort:              sgPortHTTPS,
			ToPort:                sgPortHTTPS,
		},
	}

	return append(ingressRules, makeSSHIngressRules(ng, vpcCIDR, description)...)
}

// RenderJSON returns the rendered JSON
func (n *NodeGroupResourceSet) RenderJSON() ([]byte, error) {
	return n.rs.renderJSON()
}

// Template returns the CloudFormation template
func (n *NodeGroupResourceSet) Template() gfn.Template {
	return *n.rs.template
}

func (n *NodeGroupResourceSet) newResource(name string, resource gfn.Resource) *gfnt.Value {
	return n.rs.newResource(name, resource)
}

func (n *NodeGroupResourceSet) addResourcesForNodeGroup(ctx context.Context) error {
	launchTemplateName := gfnt.MakeFnSubString(fmt.Sprintf("${%s}", gfnt.StackName))
	launchTemplateData, err := newLaunchTemplateData(ctx, n)
	if err != nil {
		return errors.Wrap(err, "could not add resources for nodegroup")
	}

	if n.spec.SSH != nil && api.IsSetAndNonEmptyString(n.spec.SSH.PublicKeyName) {
		launchTemplateData.KeyName = gfnt.NewString(*n.spec.SSH.PublicKeyName)
	}

	launchTemplateData.BlockDeviceMappings = makeBlockDeviceMappings(n.spec.NodeGroupBase)

	n.newResource("NodeGroupLaunchTemplate", &gfnec2.LaunchTemplate{
		LaunchTemplateName: launchTemplateName,
		LaunchTemplateData: launchTemplateData,
	})

	vpcZoneIdentifier, err := AssignSubnets(ctx, n.spec.NodeGroupBase, n.vpcImporter, n.clusterSpec, n.ec2API)
	if err != nil {
		return err
	}

	tags := []map[string]interface{}{
		{
			"Key":               "Name",
			"Value":             generateNodeName(n.spec.NodeGroupBase, n.clusterSpec.Metadata),
			"PropagateAtLaunch": "true",
		},
		{
			"Key":               "kubernetes.io/cluster/" + n.clusterSpec.Metadata.Name,
			"Value":             "owned",
			"PropagateAtLaunch": "true",
		},
	}
	if api.IsEnabled(n.spec.IAM.WithAddonPolicies.AutoScaler) {
		tags = append(tags,
			map[string]interface{}{
				"Key":               "k8s.io/cluster-autoscaler/enabled",
				"Value":             "true",
				"PropagateAtLaunch": "true",
			},
			map[string]interface{}{
				"Key":               "k8s.io/cluster-autoscaler/" + n.clusterSpec.Metadata.Name,
				"Value":             "owned",
				"PropagateAtLaunch": "true",
			},
		)
	}

	if api.IsEnabled(n.spec.PropagateASGTags) {
		clusterTags, err := generateClusterAutoscalerTags(n.spec)
		if err != nil {
			return err
		}
		tags = append(tags, clusterTags...)
		if len(tags) > MaximumTagNumber {
			return fmt.Errorf("number of tags is exceeding the configured amount %d, was: %d. Due to desiredCapacity==0 we added an extra %d number of tags to ensure the nodegroup is scaled correctly", MaximumTagNumber, len(tags), len(clusterTags))
		}
	}

	asg := nodeGroupResource(launchTemplateName, vpcZoneIdentifier, tags, n.spec)
	n.newResource("NodeGroup", asg)

	return nil
}

func generateClusterAutoscalerTags(spec *api.NodeGroup) ([]map[string]interface{}, error) {
	result := make([]map[string]interface{}, 0)
	duplicates := make(map[string]string)

	// labels
	for k, v := range spec.Labels {
		duplicates[k] = v
		result = append(result, map[string]interface{}{
			"Key":               "k8s.io/cluster-autoscaler/node-template/label/" + k,
			"Value":             v,
			"PropagateAtLaunch": "true",
		})
	}

	// taints
	for _, taint := range spec.Taints {
		if _, ok := duplicates[taint.Key]; ok {
			return nil, fmt.Errorf("duplicate key found for taints and labels with taint key=value: %s=%s, and label: %s=%s", taint.Key, taint.Value, taint.Key, duplicates[taint.Key])
		}
		duplicates[taint.Key] = taint.Value
		result = append(result, map[string]interface{}{
			"Key":               "k8s.io/cluster-autoscaler/node-template/taints/" + taint.Key,
			"Value":             taint.Value,
			"PropagateAtLaunch": "true",
		})
	}
	return result, nil
}

// generateNodeName formulates the name based on the configuration in input
func generateNodeName(ng *api.NodeGroupBase, meta *api.ClusterMeta) string {
	var nameParts []string
	if ng.InstancePrefix != "" {
		nameParts = append(nameParts, ng.InstancePrefix, "-")
	}
	// this overrides the default naming convention
	if ng.InstanceName != "" {
		nameParts = append(nameParts, ng.InstanceName)
	} else {
		nameParts = append(nameParts, fmt.Sprintf("%s-%s-Node", meta.Name, ng.Name))
	}
	return strings.Join(nameParts, "")
}

// AssignSubnets subnets based on the specified availability zones
func AssignSubnets(ctx context.Context, spec *api.NodeGroupBase, vpcImporter vpc.Importer, clusterSpec *api.ClusterConfig, ec2API awsapi.EC2) (*gfnt.Value, error) {
	// Currently, goformation type system doesn't allow specifying `VPCZoneIdentifier: { "Fn::ImportValue": ... }`,
	// and tags don't have `PropagateAtLaunch` field, so we have a custom method here until this gets resolved

	if len(spec.AvailabilityZones) > 0 || len(spec.Subnets) > 0 || api.IsEnabled(spec.EFAEnabled) {
		subnets := clusterSpec.VPC.Subnets.Public
		typ := "public"
		if spec.PrivateNetworking {
			subnets = clusterSpec.VPC.Subnets.Private
			typ = "private"
		}
		subnetIDs, err := vpc.SelectNodeGroupSubnets(ctx, spec.AvailabilityZones, spec.Subnets, subnets, ec2API, clusterSpec.VPC.ID)
		if api.IsEnabled(spec.EFAEnabled) && len(subnetIDs) > 1 {
			subnetIDs = []string{subnetIDs[0]}
			logger.Info("EFA requires all nodes be in a single subnet, arbitrarily choosing one: %s", subnetIDs)
		}
		return gfnt.NewStringSlice(subnetIDs...), errors.Wrapf(err, "couldn't find %s subnets", typ)
	}

	var subnets *gfnt.Value
	if spec.PrivateNetworking {
		subnets = vpcImporter.SubnetsPrivate()
	} else {
		subnets = vpcImporter.SubnetsPublic()
	}

	return subnets, nil
}

// GetAllOutputs collects all outputs of the nodegroup
func (n *NodeGroupResourceSet) GetAllOutputs(stack cfn.Stack) error {
	return n.rs.GetAllOutputs(stack)
}

func newLaunchTemplateData(ctx context.Context, n *NodeGroupResourceSet) (*gfnec2.LaunchTemplate_LaunchTemplateData, error) {
	userData, err := n.bootstrapper.UserData()
	if err != nil {
		return nil, err
	}

	launchTemplateData := &gfnec2.LaunchTemplate_LaunchTemplateData{
		IamInstanceProfile: &gfnec2.LaunchTemplate_IamInstanceProfile{
			Arn: n.instanceProfileARN,
		},
		ImageId:           gfnt.NewString(n.spec.AMI),
		UserData:          gfnt.NewString(userData),
		MetadataOptions:   makeMetadataOptions(n.spec.NodeGroupBase),
		TagSpecifications: makeTags(n.spec.NodeGroupBase, n.clusterSpec.Metadata),
	}

	if err := buildNetworkInterfaces(ctx, launchTemplateData, n.spec.InstanceTypeList(), api.IsEnabled(n.spec.EFAEnabled), n.securityGroups, n.ec2API); err != nil {
		return nil, errors.Wrap(err, "couldn't build network interfaces for launch template data")
	}

	if api.IsEnabled(n.spec.EFAEnabled) && n.spec.Placement == nil {
		groupName := n.newResource("NodeGroupPlacementGroup", &gfnec2.PlacementGroup{
			Strategy: gfnt.NewString("cluster"),
		})
		launchTemplateData.Placement = &gfnec2.LaunchTemplate_Placement{
			GroupName: groupName,
		}
	}

	if !api.HasMixedInstances(n.spec) {
		launchTemplateData.InstanceType = gfnt.NewString(n.spec.InstanceType)
	} else {
		launchTemplateData.InstanceType = gfnt.NewString(n.spec.InstancesDistribution.InstanceTypes[0])
	}
	if n.spec.EBSOptimized != nil {
		launchTemplateData.EbsOptimized = gfnt.NewBoolean(*n.spec.EBSOptimized)
	}

	if n.spec.CPUCredits != nil {
		launchTemplateData.CreditSpecification = &gfnec2.LaunchTemplate_CreditSpecification{
			CpuCredits: gfnt.NewString(strings.ToLower(*n.spec.CPUCredits)),
		}
	}

	if n.spec.Placement != nil {
		launchTemplateData.Placement = &gfnec2.LaunchTemplate_Placement{
			GroupName: gfnt.NewString(n.spec.Placement.GroupName),
		}
	}

	if n.spec.EnableDetailedMonitoring != nil {
		launchTemplateData.Monitoring = &gfnec2.LaunchTemplate_Monitoring{
			Enabled: gfnt.NewBoolean(*n.spec.EnableDetailedMonitoring),
		}
	}

	return launchTemplateData, nil
}

func makeMetadataOptions(ng *api.NodeGroupBase) *gfnec2.LaunchTemplate_MetadataOptions {
	imdsv2TokensRequired := "optional"
	if api.IsEnabled(ng.DisableIMDSv1) || api.IsEnabled(ng.DisablePodIMDS) {
		imdsv2TokensRequired = "required"
	}
	hopLimit := 2
	if api.IsEnabled(ng.DisablePodIMDS) {
		hopLimit = 1
	}
	return &gfnec2.LaunchTemplate_MetadataOptions{
		HttpPutResponseHopLimit: gfnt.NewInteger(hopLimit),
		HttpTokens:              gfnt.NewString(imdsv2TokensRequired),
	}
}

func nodeGroupResource(launchTemplateName *gfnt.Value, vpcZoneIdentifier interface{}, tags []map[string]interface{}, ng *api.NodeGroup) *awsCloudFormationResource {
	ngProps := map[string]interface{}{
		"VPCZoneIdentifier": vpcZoneIdentifier,
		"Tags":              tags,
	}

	if ng.InstancesDistribution != nil && ng.InstancesDistribution.CapacityRebalance {
		ngProps["CapacityRebalance"] = ng.InstancesDistribution.CapacityRebalance
	}

	if ng.DesiredCapacity != nil {
		ngProps["DesiredCapacity"] = fmt.Sprintf("%d", *ng.DesiredCapacity)
	}
	if ng.MinSize != nil {
		ngProps["MinSize"] = fmt.Sprintf("%d", *ng.MinSize)
	}
	if ng.MaxSize != nil {
		ngProps["MaxSize"] = fmt.Sprintf("%d", *ng.MaxSize)
	}
	if len(ng.ASGMetricsCollection) > 0 {
		ngProps["MetricsCollection"] = metricsCollectionResource(ng.ASGMetricsCollection)
	}
	if len(ng.ClassicLoadBalancerNames) > 0 {
		ngProps["LoadBalancerNames"] = ng.ClassicLoadBalancerNames
	}
	if len(ng.TargetGroupARNs) > 0 {
		ngProps["TargetGroupARNs"] = ng.TargetGroupARNs
	}
	if api.HasMixedInstances(ng) {
		ngProps["MixedInstancesPolicy"] = *mixedInstancesPolicy(launchTemplateName, ng)
	} else {
		ngProps["LaunchTemplate"] = map[string]interface{}{
			"LaunchTemplateName": launchTemplateName,
			"Version":            gfnt.MakeFnGetAttString("NodeGroupLaunchTemplate", "LatestVersionNumber"),
		}
	}

	if ng.MaxInstanceLifetime != nil {
		ngProps["MaxInstanceLifetime"] = *ng.MaxInstanceLifetime
	}

	rollingUpdate := map[string]interface{}{}
	if len(ng.ASGSuspendProcesses) > 0 {
		rollingUpdate["SuspendProcesses"] = ng.ASGSuspendProcesses
	}

	return &awsCloudFormationResource{
		Type:       "AWS::AutoScaling::AutoScalingGroup",
		Properties: ngProps,
		UpdatePolicy: map[string]map[string]interface{}{
			"AutoScalingRollingUpdate": rollingUpdate,
		},
	}
}

func mixedInstancesPolicy(launchTemplateName *gfnt.Value, ng *api.NodeGroup) *map[string]interface{} {
	instanceTypes := ng.InstancesDistribution.InstanceTypes
	overrides := make([]map[string]string, len(instanceTypes))

	for i, instanceType := range instanceTypes {
		overrides[i] = map[string]string{
			"InstanceType": instanceType,
		}
	}
	policy := map[string]interface{}{
		"LaunchTemplate": map[string]interface{}{
			"LaunchTemplateSpecification": map[string]interface{}{
				"LaunchTemplateName": launchTemplateName,
				"Version":            gfnt.MakeFnGetAttString("NodeGroupLaunchTemplate", "LatestVersionNumber"),
			},

			"Overrides": overrides,
		},
	}

	instancesDistribution := map[string]string{}

	// Only set the price if it was specified so otherwise AWS picks "on-demand price" as the default
	if ng.InstancesDistribution.MaxPrice != nil {
		instancesDistribution["SpotMaxPrice"] = fmt.Sprintf("%f", *ng.InstancesDistribution.MaxPrice)
	}
	if ng.InstancesDistribution.OnDemandBaseCapacity != nil {
		instancesDistribution["OnDemandBaseCapacity"] = fmt.Sprintf("%d", *ng.InstancesDistribution.OnDemandBaseCapacity)
	}
	if ng.InstancesDistribution.OnDemandPercentageAboveBaseCapacity != nil {
		instancesDistribution["OnDemandPercentageAboveBaseCapacity"] = fmt.Sprintf("%d", *ng.InstancesDistribution.OnDemandPercentageAboveBaseCapacity)
	}
	if ng.InstancesDistribution.SpotInstancePools != nil {
		instancesDistribution["SpotInstancePools"] = fmt.Sprintf("%d", *ng.InstancesDistribution.SpotInstancePools)
	}

	if ng.InstancesDistribution.SpotAllocationStrategy != nil {
		instancesDistribution["SpotAllocationStrategy"] = *ng.InstancesDistribution.SpotAllocationStrategy
	}

	policy["InstancesDistribution"] = instancesDistribution

	return &policy
}

func metricsCollectionResource(asgMetricsCollection []api.MetricsCollection) []map[string]interface{} {
	var metricsCollections []map[string]interface{}
	for _, m := range asgMetricsCollection {
		newCollection := make(map[string]interface{})

		if len(m.Metrics) > 0 {
			newCollection["Metrics"] = m.Metrics
		}
		newCollection["Granularity"] = m.Granularity

		metricsCollections = append(metricsCollections, newCollection)
	}
	return metricsCollections
}

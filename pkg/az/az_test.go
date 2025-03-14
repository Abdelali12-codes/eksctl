package az_test

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/mock"

	api "github.com/weaveworks/eksctl/pkg/apis/eksctl.io/v1alpha5"
	"github.com/weaveworks/eksctl/pkg/az"
	"github.com/weaveworks/eksctl/pkg/testutils/mockprovider"
)

var _ = Describe("AZ", func() {
	var (
		region string
		p      *mockprovider.MockProvider
	)

	BeforeEach(func() {
		region = "us-west-1"
		p = mockprovider.NewMockProvider()
	})

	When("1 AZ is available", func() {
		BeforeEach(func() {
			p.MockEC2().On("DescribeAvailabilityZones", mock.Anything, &ec2.DescribeAvailabilityZonesInput{
				Filters: []ec2types.Filter{
					{
						Name:   aws.String("region-name"),
						Values: []string{region},
					},
					{
						Name:   aws.String("state"),
						Values: []string{string(ec2types.AvailabilityZoneStateAvailable)},
					},
					{
						Name:   aws.String("zone-type"),
						Values: []string{string(ec2types.LocationTypeAvailabilityZone)},
					},
				},
			}).Return(&ec2.DescribeAvailabilityZonesOutput{
				AvailabilityZones: []ec2types.AvailabilityZone{
					createAvailabilityZone(region, ec2types.AvailabilityZoneStateAvailable, "zone1"),
				},
			}, nil)
		})

		It("errors", func() {
			_, err := az.GetAvailabilityZones(context.Background(), p.MockEC2(), region)
			Expect(err).To(MatchError("only 1 zones discovered [zone1], at least 2 are required"))
		})
	})

	When("2 AZs are available", func() {
		BeforeEach(func() {
			p.MockEC2().On("DescribeAvailabilityZones", mock.Anything, &ec2.DescribeAvailabilityZonesInput{
				Filters: []ec2types.Filter{
					{
						Name:   aws.String("region-name"),
						Values: []string{region},
					},
					{
						Name:   aws.String("state"),
						Values: []string{string(ec2types.AvailabilityZoneStateAvailable)},
					},
					{
						Name:   aws.String("zone-type"),
						Values: []string{string(ec2types.LocationTypeAvailabilityZone)},
					},
				},
			}).Return(&ec2.DescribeAvailabilityZonesOutput{
				AvailabilityZones: []ec2types.AvailabilityZone{
					createAvailabilityZone(region, ec2types.AvailabilityZoneStateAvailable, "zone1"),
					createAvailabilityZone(region, ec2types.AvailabilityZoneStateAvailable, "zone2"),
				},
			}, nil)
		})

		It("should return the 2 available AZs", func() {
			zones, err := az.GetAvailabilityZones(context.Background(), p.MockEC2(), region)
			Expect(err).NotTo(HaveOccurred())
			Expect(zones).To(HaveLen(2))
			Expect(zones).To(ConsistOf("zone1", "zone2"))
		})
	})

	When("3 AZs are available", func() {
		BeforeEach(func() {
			p.MockEC2().On("DescribeAvailabilityZones", mock.Anything, &ec2.DescribeAvailabilityZonesInput{
				Filters: []ec2types.Filter{
					{
						Name:   aws.String("region-name"),
						Values: []string{region},
					},
					{
						Name:   aws.String("state"),
						Values: []string{string(ec2types.AvailabilityZoneStateAvailable)},
					},
					{
						Name:   aws.String("zone-type"),
						Values: []string{string(ec2types.LocationTypeAvailabilityZone)},
					},
				},
			}).Return(&ec2.DescribeAvailabilityZonesOutput{
				AvailabilityZones: []ec2types.AvailabilityZone{
					createAvailabilityZone(region, ec2types.AvailabilityZoneStateAvailable, "zone1"),
					createAvailabilityZone(region, ec2types.AvailabilityZoneStateAvailable, "zone2"),
					createAvailabilityZone(region, ec2types.AvailabilityZoneStateAvailable, "zone3"),
				},
			}, nil)
		})

		It("should return the 3 available AZs", func() {
			zones, err := az.GetAvailabilityZones(context.Background(), p.MockEC2(), region)
			Expect(err).NotTo(HaveOccurred())
			Expect(zones).To(HaveLen(3))
			Expect(zones).To(ConsistOf("zone1", "zone2", "zone3"))
		})
	})

	When("more than 3 AZs are available", func() {
		BeforeEach(func() {
			p.MockEC2().On("DescribeAvailabilityZones", mock.Anything, &ec2.DescribeAvailabilityZonesInput{
				Filters: []ec2types.Filter{
					{
						Name:   aws.String("region-name"),
						Values: []string{region},
					},
					{
						Name:   aws.String("state"),
						Values: []string{string(ec2types.AvailabilityZoneStateAvailable)},
					},
					{
						Name:   aws.String("zone-type"),
						Values: []string{string(ec2types.LocationTypeAvailabilityZone)},
					},
				},
			}).Return(&ec2.DescribeAvailabilityZonesOutput{
				AvailabilityZones: []ec2types.AvailabilityZone{
					createAvailabilityZone(region, ec2types.AvailabilityZoneStateAvailable, "zone1"),
					createAvailabilityZone(region, ec2types.AvailabilityZoneStateAvailable, "zone2"),
					createAvailabilityZone(region, ec2types.AvailabilityZoneStateAvailable, "zone3"),
					createAvailabilityZone(region, ec2types.AvailabilityZoneStateAvailable, "zone4"),
				},
			}, nil)
		})

		It("should return a random set of 3 available AZs", func() {
			zones, err := az.GetAvailabilityZones(context.Background(), p.MockEC2(), region)
			Expect(err).NotTo(HaveOccurred())
			Expect(zones).To(HaveLen(3))
			Expect(zonesAreUnique(zones)).To(BeTrue())
		})
	})

	When("fetching the AZs errors", func() {
		BeforeEach(func() {
			p.MockEC2().On("DescribeAvailabilityZones", mock.Anything, &ec2.DescribeAvailabilityZonesInput{
				Filters: []ec2types.Filter{
					{
						Name:   aws.String("region-name"),
						Values: []string{region},
					},
					{
						Name:   aws.String("state"),
						Values: []string{string(ec2types.AvailabilityZoneStateAvailable)},
					},
					{
						Name:   aws.String("zone-type"),
						Values: []string{string(ec2types.LocationTypeAvailabilityZone)},
					},
				},
			}).Return(&ec2.DescribeAvailabilityZonesOutput{}, fmt.Errorf("foo"))
		})

		It("errors", func() {
			_, err := az.GetAvailabilityZones(context.Background(), p.MockEC2(), region)
			Expect(err).To(MatchError(fmt.Sprintf("error getting availability zones for region %s: foo", region)))
		})
	})

	When("the region contains zones that are denylisted", func() {
		BeforeEach(func() {
			region = api.RegionCNNorth1

			p.MockEC2().On("DescribeAvailabilityZones", mock.Anything, &ec2.DescribeAvailabilityZonesInput{
				Filters: []ec2types.Filter{
					{
						Name:   aws.String("region-name"),
						Values: []string{region},
					},
					{
						Name:   aws.String("state"),
						Values: []string{string(ec2types.AvailabilityZoneStateAvailable)},
					},
					{
						Name:   aws.String("zone-type"),
						Values: []string{string(ec2types.LocationTypeAvailabilityZone)},
					},
				},
			}).Return(&ec2.DescribeAvailabilityZonesOutput{
				AvailabilityZones: []ec2types.AvailabilityZone{
					createAvailabilityZone(region, ec2types.AvailabilityZoneStateAvailable, "zone1"),
					createAvailabilityZone(region, ec2types.AvailabilityZoneStateAvailable, "zone2"),
					createAvailabilityZoneWithID(region, ec2types.AvailabilityZoneStateAvailable, "zone3", "cnn1-az4"),
				},
			}, nil)
		})

		It("should not use the denylisted zones", func() {
			zones, err := az.GetAvailabilityZones(context.Background(), p.MockEC2(), region)
			Expect(err).NotTo(HaveOccurred())
			Expect(zones).To(HaveLen(2))
			Expect(zones).To(ConsistOf("zone1", "zone2"))
		})
	})

	When("using us-east-1", func() {
		BeforeEach(func() {
			region = "us-east-1"

			p.MockEC2().On("DescribeAvailabilityZones", mock.Anything, &ec2.DescribeAvailabilityZonesInput{
				Filters: []ec2types.Filter{
					{
						Name:   aws.String("region-name"),
						Values: []string{region},
					},
					{
						Name:   aws.String("state"),
						Values: []string{string(ec2types.AvailabilityZoneStateAvailable)},
					},
					{
						Name:   aws.String("zone-type"),
						Values: []string{string(ec2types.LocationTypeAvailabilityZone)},
					},
				},
			}).Return(&ec2.DescribeAvailabilityZonesOutput{
				AvailabilityZones: []ec2types.AvailabilityZone{
					createAvailabilityZone(region, ec2types.AvailabilityZoneStateAvailable, "zone1"),
					createAvailabilityZone(region, ec2types.AvailabilityZoneStateAvailable, "zone2"),
					createAvailabilityZone(region, ec2types.AvailabilityZoneStateAvailable, "zone3"),
					createAvailabilityZone(region, ec2types.AvailabilityZoneStateAvailable, "zone4"),
				},
			}, nil)
		})

		It("should only use 2 AZs, rather than the default 3", func() {
			zones, err := az.GetAvailabilityZones(context.Background(), p.MockEC2(), region)
			Expect(err).NotTo(HaveOccurred())
			Expect(zones).To(HaveLen(2))
			Expect(zonesAreUnique(zones)).To(BeTrue())
		})
	})
})

func zonesAreUnique(zones []string) bool {
	mapZones := make(map[string]interface{})
	for _, z := range zones {
		mapZones[z] = nil
	}
	return len(mapZones) == len(zones)
}

func createAvailabilityZone(region string, state ec2types.AvailabilityZoneState, zone string) ec2types.AvailabilityZone {
	return createAvailabilityZoneWithID(region, state, zone, "id-"+zone)
}

func createAvailabilityZoneWithID(region string, state ec2types.AvailabilityZoneState, zone, zoneID string) ec2types.AvailabilityZone {
	return ec2types.AvailabilityZone{
		RegionName: aws.String(region),
		State:      state,
		ZoneName:   aws.String(zone),
		ZoneId:     aws.String(zoneID),
	}
}

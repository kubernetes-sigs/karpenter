/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
	ptypes "github.com/aws/aws-sdk-go-v2/service/pricing/types"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	kwok "sigs.k8s.io/karpenter/kwok/cloudprovider"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

type InstanceTypeInfo struct {
	Name         string                   `yaml:"name" json:"name"`
	Offerings    []cloudprovider.Offering `yaml:"offerings" json:"offerings"`
	Architecture string                   `yaml:"architecture" json:"architecture"`
}

func main() {
	ctx := context.Background()
	cfg := lo.Must(config.LoadDefaultConfig(ctx))

	// Create EC2 client
	ec2Client := ec2.NewFromConfig(cfg)

	// Get all availability zones in the region
	azOutput, err := ec2Client.DescribeAvailabilityZones(context.Background(), &ec2.DescribeAvailabilityZonesInput{})
	if err != nil {
		log.Fatalf("unable to describe availability zones, %v", err)
	}
	if len(azOutput.AvailabilityZones) == 0 {
		log.Fatalf("no availability zones found")
	}

	// Create Pricing client (must be in us-east-1)
	pricingCfg := cfg.Copy()
	pricingCfg.Region = "us-east-1"
	pricingClient := pricing.NewFromConfig(pricingCfg)
	priceList, err := getAllPricing(ctx, pricingClient)
	if err != nil {
		log.Fatalf("failed to get pricing: %v", err)
	}
	// bail early if priceList is empty
	if len(priceList) == 0 {
		log.Fatalf("no pricing found")
	}
	priceIndex, err := createPriceIndex(priceList)
	if err != nil {
		log.Fatalf("failed to create price index: %v", err)
	}
	instanceTypes, err := getAllInstanceTypes(ctx, ec2Client)
	if err != nil {
		log.Fatalf("unable to describe instance types, %v", err)
	}
	// bail early if instanceTypes is empty
	if len(instanceTypes) == 0 {
		log.Fatalf("no instance types found")
	}
	var instanceTypesOptions []kwok.InstanceTypeOptions
	/*
		The Price List Query API and Price List Bulk API provides the following endpoints:
			https://api.pricing.us-east-1.amazonaws.com
			https://api.pricing.eu-central-1.amazonaws.com
			https://api.pricing.ap-south-1.amazonaws.com
	*/
	region := cfg.Region
	switch {
	case strings.HasPrefix(region, "us"):
		region = "us-east-1"
	case strings.HasPrefix(region, "eu"):
		region = "eu-central-1"
	case strings.HasPrefix(region, "ap"):
		region = "ap-south-1"
	default:
		log.Fatalf("no pricing endpoint available for region: %s", region)
	}

	// this is how the instance type is created in aws provider
	/*
		func NewInstanceType(ctx context.Context, info ec2types.InstanceTypeInfo, region string,
			blockDeviceMappings []*v1.BlockDeviceMapping, instanceStorePolicy *v1.InstanceStorePolicy, maxPods *int32, podsPerCore *int32,
			kubeReserved map[string]string, systemReserved map[string]string, evictionHard map[string]string, evictionSoft map[string]string,
			amiFamilyType string, offerings cloudprovider.Offerings) *cloudprovider.InstanceType {

			amiFamily := amifamily.GetAMIFamily(amiFamilyType, &amifamily.Options{})
			it := &cloudprovider.InstanceType{
				Name:         string(info.InstanceType),
				Requirements: computeRequirements(info, offerings, region, amiFamily),
				Offerings:    offerings,
				Capacity:     computeCapacity(ctx, info, amiFamily, blockDeviceMappings, instanceStorePolicy, maxPods, podsPerCore),
				Overhead: &cloudprovider.InstanceTypeOverhead{
					KubeReserved:      kubeReservedResources(cpu(info), pods(ctx, info, amiFamily, maxPods, podsPerCore), ENILimitedPods(ctx, info), amiFamily, kubeReserved),
					SystemReserved:    systemReservedResources(systemReserved),
					EvictionThreshold: evictionThreshold(memory(ctx, info), ephemeralStorage(info, amiFamily, blockDeviceMappings, instanceStorePolicy), amiFamily, evictionHard, evictionSoft),
				},
			}
			if it.Requirements.Compatible(scheduling.NewRequirements(scheduling.NewRequirement(corev1.LabelOSStable, corev1.NodeSelectorOpIn, string(corev1.Windows)))) == nil {
				it.Capacity[v1.ResourcePrivateIPv4Address] = *privateIPv4Address(string(info.InstanceType))
			}
			return it
		}
	*/

	// Process each instance type
	for _, instance := range instanceTypes {
		instanceTypeName := string(instance.InstanceType)
		key := fmt.Sprintf("%s:%s", instanceTypeName, region)
		// skip if key isn't present in priceIndex which means a price was not found
		if _, ok := priceIndex[key]; !ok {
			continue
		}
		price := priceIndex[key]
		// loop through architectures and os
		for _, arch := range instance.ProcessorInfo.SupportedArchitectures {
			for _, os := range []corev1.OSName{corev1.Windows, corev1.Linux} {
				cpu := int(*instance.VCpuInfo.DefaultVCpus)
				mem := int(*instance.MemoryInfo.SizeInMiB / 1024)
				pods := lo.Clamp(cpu*16, 0, 1024)
				opts := kwok.InstanceTypeOptions{
					Name:             instanceTypeName,
					Architecture:     string(arch),
					OperatingSystems: []corev1.OSName{os},
					Resources: corev1.ResourceList{
						corev1.ResourceCPU:              resource.MustParse(fmt.Sprintf("%d", cpu)),
						corev1.ResourceMemory:           resource.MustParse(fmt.Sprintf("%dGi", mem)),
						corev1.ResourcePods:             resource.MustParse(fmt.Sprintf("%d", pods)),
						corev1.ResourceEphemeralStorage: resource.MustParse("20Gi"),
					},
				}
				// Create KWOKOffering here by az and capacity type
				for _, zone := range azOutput.AvailabilityZones {
					for _, ct := range []string{v1.CapacityTypeSpot, v1.CapacityTypeOnDemand} {
						opts.Offerings = append(opts.Offerings, kwok.KWOKOffering{
							Offering: cloudprovider.Offering{
								Price:     lo.Ternary(ct == v1.CapacityTypeSpot, price*.7, price),
								Available: true,
							},
							Requirements: []corev1.NodeSelectorRequirement{
								{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{ct}},
								{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{*zone.ZoneName}},
							},
						})
					}
				}
				if len(opts.Offerings) == 0 {
					log.Printf("no offerings found for %s", instanceTypeName)
					continue
				}
				instanceTypesOptions = append(instanceTypesOptions, opts)
			}
		}
	}

	// Convert to JSON
	jsonData, err := json.MarshalIndent(instanceTypesOptions, "", "    ")
	if err != nil {
		log.Fatalf("unable to marshal to JSON, %v", err)
	}

	// Write to file
	err = os.WriteFile("kwok/examples/aws_instance_types.json", jsonData, 0644)
	if err != nil {
		log.Fatalf("unable to write JSON file, %v", err)
	}
}

func getAllInstanceTypes(ctx context.Context, client *ec2.Client) ([]types.InstanceTypeInfo, error) {
	var instanceTypes []types.InstanceTypeInfo
	paginator := ec2.NewDescribeInstanceTypesPaginator(client, &ec2.DescribeInstanceTypesInput{})

	for paginator.HasMorePages() {
		output, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to get instance types page: %w", err)
		}
		instanceTypes = append(instanceTypes, output.InstanceTypes...)
	}

	return instanceTypes, nil
}

func getAllPricing(ctx context.Context, client *pricing.Client) ([]string, error) {
	var priceList []string

	// for now just get pricing for m5 instance types
	input := &pricing.GetProductsInput{
		ServiceCode: lo.ToPtr("AmazonEC2"),
		Filters: []ptypes.Filter{
			{
				Field: aws.String("regionCode"),
				Type:  "TERM_MATCH",
				Value: aws.String("us-east-1"),
			},
			{
				Field: aws.String("serviceCode"),
				Type:  "TERM_MATCH",
				Value: aws.String("AmazonEC2"),
			},
			{
				Field: aws.String("preInstalledSw"),
				Type:  "TERM_MATCH",
				Value: aws.String("NA"),
			},
			{
				Field: aws.String("operatingSystem"),
				Type:  "TERM_MATCH",
				Value: aws.String("Linux"),
			},
			{
				Field: aws.String("capacitystatus"),
				Type:  "TERM_MATCH",
				Value: aws.String("Used"),
			},
			{
				Field: aws.String("marketoption"),
				Type:  "TERM_MATCH",
				Value: aws.String("OnDemand"),
			},
		},
	}

	paginator := pricing.NewGetProductsPaginator(client, input)

	for paginator.HasMorePages() {
		output, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to get pricing page: %w", err)
		}
		priceList = append(priceList, output.PriceList...)
	}

	return priceList, nil
}

func createPriceIndex(priceList []string) (map[string]float64, error) {
	priceIndex := make(map[string]float64)
	for _, priceItem := range priceList {
		var priceData struct {
			Product struct {
				Attributes struct {
					InstanceType string `json:"instanceType"`
					RegionCode   string `json:"regionCode"`
				} `json:"attributes"`
			} `json:"product"`
			Terms struct {
				OnDemand map[string]struct {
					PriceDimensions map[string]struct {
						PricePerUnit struct {
							USD string `json:"USD"`
						} `json:"pricePerUnit"`
					} `json:"priceDimensions"`
				} `json:"OnDemand"`
			} `json:"terms"`
		}

		if err := json.Unmarshal([]byte(priceItem), &priceData); err != nil {
			continue
		}

		// Create a key combining instance type and region
		key := fmt.Sprintf("%s:%s",
			priceData.Product.Attributes.InstanceType,
			priceData.Product.Attributes.RegionCode,
		)

		// Get the first price dimension
		for _, onDemand := range priceData.Terms.OnDemand {
			for _, dimension := range onDemand.PriceDimensions {
				price, err := strconv.ParseFloat(dimension.PricePerUnit.USD, 64)
				if err != nil {
					continue
				}
				if price == 0 {
					continue
				}
				priceIndex[key] = price
				break
			}
			break
		}
	}

	return priceIndex, nil
}

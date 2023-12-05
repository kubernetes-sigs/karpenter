/*
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
package kwok

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/docker/docker/pkg/namesgenerator"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	kubeErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/cloudprovider/fake"
	"github.com/aws/karpenter-core/pkg/scheduling"
	"github.com/aws/karpenter/pkg/utils/project"
)

var instanceTypes []*cloudprovider.InstanceType

func init() {
	instanceTypes = fake.InstanceTypesAssorted()
	fake.AddFakeLabels()
}

func NewCloudProvider(ctx context.Context, client kubernetes.Interface) *CloudProvider {
	return &CloudProvider{
		kubeClient: client,
	}
}

type CloudProvider struct {
	kubeClient    kubernetes.Interface
	populateTypes sync.Once
	instanceTypes []*cloudprovider.InstanceTypes
}

func (c CloudProvider) Create(ctx context.Context, nodeClaim *v1beta1.NodeClaim) (*v1beta1.NodeClaim, error) {
	// Create the Node because KwoK nodes don't run kubelet, which is what Karpenter relies on to create the node.
	node, err := c.toNode(nodeClaim)
	if err != nil {
		return nil, fmt.Errorf("translating nodeclaim to node, %w", err)
	}
	_, err = c.kubeClient.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("creating node, %w", err)
	}
	// convert the node back into a node claim to get the chosen resolved requirement values.
	return c.toNodeClaim(node)
}

func (c CloudProvider) Delete(ctx context.Context, nodeClaim *v1beta1.NodeClaim) error {
	err := c.kubeClient.CoreV1().Nodes().Delete(ctx, nodeClaim.Name, metav1.DeleteOptions{})
	if err != nil {
		if kubeErrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("deleting node, %w", err)
	}
	return nil
}

func (c CloudProvider) Get(ctx context.Context, providerID string) (*v1beta1.NodeClaim, error) {
	nodeName := strings.Replace(providerID, kwokProviderPrefix, "", -1)
	node, err := c.kubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("finding node, %w", err)
	}
	return c.toNodeClaim(node)
}

func (c CloudProvider) List(ctx context.Context) ([]*v1beta1.NodeClaim, error) {
	nodes, err := c.kubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing nodes, %w", err)
	}
	var nodeClaims []*v1beta1.NodeClaim
	for i, node := range nodes.Items {
		if !strings.HasPrefix(node.Spec.ProviderID, kwokProviderPrefix) {
			continue
		}
		nc, err := c.toNodeClaim(&nodes.Items[i])
		if err != nil {
			return nil, fmt.Errorf("converting machine, %w", err)
		}
		nodeClaims = append(nodeClaims, nc)
	}

	return nodeClaims, nil
}

func (c CloudProvider) GetInstanceTypes(ctx context.Context, nodePool *v1beta1.NodePool) ([]*cloudprovider.InstanceType, error) {
	var ret []*cloudprovider.InstanceType
	for _, it := range instanceTypes {
		offerings := c.offerings(ctx, it)
		ret = append(ret, &cloudprovider.InstanceType{
			Name:         it.Name,
			Requirements: requirements(offerings, it),
			Offerings:    offerings,
			Capacity:     computeCapacity(ctx, it),
			Overhead: &cloudprovider.InstanceTypeOverhead{
				KubeReserved:      nil,
				SystemReserved:    nil,
				EvictionThreshold: nil,
			},
		})
	}
	return ret, nil
}

func (c CloudProvider) offerings(ctx context.Context, it *cloudprovider.InstanceType) cloudprovider.Offerings {
	var ret cloudprovider.Offerings
	for _, zone := range kwokZones {
		ret = append(ret, cloudprovider.Offering{
			CapacityType: v1beta1.CapacityTypeOnDemand,
			Zone:         zone,
			Price:        fake.PriceFromResources(it.Capacity),
			Available:    true,
		})
	if spotPrice, ok := c.Pricing.SpotPrice(*it.InstanceType, zone); ok {
			ret = append(ret, cloudprovider.Offering{
				CapacityType: v1beta1.CapacityTypeSpot,
				Zone:         zone,
				Price:        spotPrice,
				Available:    true,
			})
		}
	}
	return ret
}

func (c CloudProvider) IsDrifted(ctx context.Context, nodeClaim *v1beta1.NodeClaim) (cloudprovider.DriftReason, error) {
	return "", nil
}

func (c CloudProvider) Name() string {
	return "kwok-provider"
}

func (c CloudProvider) toNode(nodeClaim *v1beta1.NodeClaim) (*v1.Node, error) {
	newName := strings.Replace(namesgenerator.GetRandomName(0), "_", "-", -1)
	newName = fmt.Sprintf("%s-%d", newName, rand.Uint32())

	var instanceTypeName string
	var instanceTypePrice float64
	capacityType := v1beta1.CapacityTypeOnDemand
	requirements := scheduling.NewNodeSelectorRequirements(nodeClaim.
		Spec.Requirements...)
	if requirements.Get(v1beta1.CapacityTypeLabelKey).Has(v1beta1.CapacityTypeSpot) {
		capacityType = v1beta1.CapacityTypeSpot
	}
	zone := randomChoice(kwokZones)
	req, found := lo.Find(nodeClaim.Spec.Requirements, func(req v1.NodeSelectorRequirement) bool {
		return req.Key == v1.LabelInstanceTypeStable
	})
	if found {
		for _, val := range req.Values {
			// pick the cheapest OD instance type
			instanceTypeName = val
			if capacityType == "spot" {
				instanceTypePrice, _ = c.Pricing.SpotPrice(instanceTypeName, zone)
			} else {
				// Default to OD
				instanceTypePrice, _ = c.Pricing.OnDemandPrice(instanceTypeName)
			}
			for _, it := range req.Values {
				var price float64
				var ok bool
				if capacityType == "spot" {
					price, ok = c.Pricing.SpotPrice(it, zone)
				} else {
					price, ok = c.Pricing.OnDemandPrice(it)
				}
				if ok && price < instanceTypePrice {
					instanceTypePrice = price
					instanceTypeName = it
				}
			}
		}
	}

	its, err := c.GetInstanceTypes(context.Background(), nil)
	if err != nil {
		return nil, fmt.Errorf("listing instance types, %w", err)
	}
	var instanceType *cloudprovider.InstanceType
	for _, it := range its {
		if it.Name == instanceTypeName {
			instanceType = it
			break
		}
	}

	if instanceType == nil {
		return nil, fmt.Errorf("unable to find instance type %q", instanceTypeName)
	}

	return &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        newName,
			Labels:      addInstanceLabels(nodeClaim.Labels, instanceType, nodeClaim, capacityType),
			Annotations: addKwokAnnotation(nodeClaim.Annotations),
		},
		Spec: v1.NodeSpec{
			ProviderID: kwokProviderPrefix + newName,
		},
		Status: v1.NodeStatus{
			Capacity:    instanceType.Capacity,
			Allocatable: instanceType.Allocatable(),
			Phase:       v1.NodePending,
		},
	}, nil
}

func addInstanceLabels(labels map[string]string, instanceType *cloudprovider.InstanceType, nodeClaim *v1beta1.NodeClaim, capacityType string) map[string]string {
	ret := make(map[string]string, len(labels))
	// start with labels on the machine
	for k, v := range labels {
		ret[k] = v
	}

	// add the derived machine requirement labels
	for _, r := range nodeClaim.Spec.Requirements {
		if len(r.Values) == 1 && r.Operator == v1.NodeSelectorOpIn {
			ret[r.Key] = r.Values[0]
		}
	}

	// ensure we have an instance type and then any instance type requiremnets
	ret[v1.LabelInstanceTypeStable] = instanceType.Name
	for _, r := range instanceType.Requirements {
		if r.Len() == 1 && r.Operator() == v1.NodeSelectorOpIn {
			ret[r.Key] = r.Values()[0]
		}
	}
	// Kwok has some scalability limitations.
	// Randomly add each new node to one of the pre-created kwokPartitions.
	ret["kwok-partition"] = randomPartition(10)
	ret[v1beta1.CapacityTypeLabelKey] = capacityType
	// no zone set by requirements, so just pick one
	if _, ok := ret[v1.LabelTopologyZone]; !ok {
		ret[v1.LabelTopologyZone] = randomChoice(kwokZones)
	}
	ret[v1.LabelHostname] = nodeClaim.Name

	ret["kwok.x-k8s.io/node"] = "fake"
	return ret
}

// pick one of the first n letters
func randomPartition(n int) string {
	i := rand.Intn(len(kwokPartitions))
	return kwokPartitions[i]
}

func randomChoice(zones []string) string {
	i := rand.Intn(len(zones))
	return zones[i]
}

func addKwokAnnotation(annotations map[string]string) map[string]string {
	ret := make(map[string]string, len(annotations)+1)
	for k, v := range annotations {
		ret[k] = v
	}
	ret["kwok.x-k8s.io/node"] = "fake"
	return ret
}

func (c CloudProvider) toNodeClaim(node *v1.Node) (*v1beta1.NodeClaim, error) {
	return &v1beta1.NodeClaim{
		IsMachine: true,
		ObjectMeta: metav1.ObjectMeta{
			Name:        node.Name,
			Labels:      node.Labels,
			Annotations: addKwokAnnotation(node.Annotations),
		},
		Spec: v1beta1.NodeClaimSpec{
			Taints:             nil,
			StartupTaints:      nil,
			Requirements:       nil,
			Resources:          v1beta1.ResourceRequirements{},
			Kubelet:            nil,
			NodeClassRef: nil,
		},
		Status: v1beta1.NodeClaimStatus{
			NodeName:    node.Name,
			ProviderID:  node.Spec.ProviderID,
			Capacity:    node.Status.Capacity,
			Allocatable: node.Status.Allocatable,
		},
	}, nil
}


// withUserAgent adds a karpenter specific user-agent string to AWS session
func withUserAgent(sess *session.Session) *session.Session {
	userAgent := fmt.Sprintf("karpenter.sh-%s", project.Version)
	sess.Handlers.Build.PushBack(request.MakeAddToUserAgentFreeFormHandler(userAgent))
	return sess
}

// checkEC2Connectivity makes a dry-run call to DescribeInstanceTypes.  If it fails, we provide an early indicator that we
// are having issues connecting to the EC2 API.
func checkEC2Connectivity(ctx context.Context, api *ec2.EC2) error {
	_, err := api.DescribeInstanceTypesWithContext(ctx, &ec2.DescribeInstanceTypesInput{DryRun: aws.Bool(true)})
	var aerr awserr.Error
	if errors.As(err, &aerr) && aerr.Code() == "DryRunOperation" {
		return nil
	}
	return err
}

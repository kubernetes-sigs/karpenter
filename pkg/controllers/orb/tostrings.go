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

package orb

import (
	"bytes"
	"fmt"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

/* The following functions are testing toString functions that will mirror what the serialization
   deserialization functions will do in protobuf. These are inefficient, but human-readable */

// TODO: This eventually will be "as simple" as reconstructing the data structures from
// the log data and using K8S and/or Karpenter representation to present as JSON or YAML or something

// This function as a human readable test function for serializing desired pod data
// It takes in a v1.Pod and gets the string representations of all the fields we care about.
func PodToString(pod *v1.Pod) string {
	if pod == nil {
		return "<nil>"
	}
	return fmt.Sprintf("{Name: %s, Namespace: %s, Phase: %s}", pod.Name, pod.Namespace, pod.Status.Phase)
}

func PodsToString(pods []*v1.Pod) string {
	if pods == nil {
		return "<nil>"
	}
	var buf bytes.Buffer
	for _, pod := range pods {
		buf.WriteString(PodToString(pod) + "\n") // TODO: Can replace with pod.String() if I want/need
	}
	return buf.String()
}

func StateNodeWithPodsToString(node *StateNodeWithPods) string {
	if node == nil {
		return "<nil>"
	}
	return fmt.Sprintf("{Node: %s, NodeClaim: %s, {Pods: %s}}",
		NodeToString(node.Node), NodeClaimToString(node.NodeClaim), PodsToString(node.Pods))
}

func StateNodesWithPodsToString(nodes []*StateNodeWithPods) string {
	if nodes == nil {
		return "<nil>"
	}
	var buf bytes.Buffer
	for _, node := range nodes {
		buf.WriteString(StateNodeWithPodsToString(node) + "\n")
	}
	return buf.String()
}

// Similar function for human-readable string serialization of a v1.Node
func NodeToString(node *v1.Node) string {
	if node == nil {
		return "<nil>"
	}
	return fmt.Sprintf("{Name: %s, Status: %s}", node.Name, node.Status.Phase)
}

// Similar function for NodeClaim
func NodeClaimToString(nodeClaim *v1beta1.NodeClaim) string {
	if nodeClaim == nil {
		return "<nil>"
	}
	return fmt.Sprintf("{NodeClaimName: %s}", nodeClaim.Name)
}

// Similar for instanceTypes (name, requirements, offerings, capacity, overhead
func InstanceTypeToString(instanceType *cloudprovider.InstanceType) string {
	if instanceType == nil {
		return "<nil>"
	}
	// TODO: String print the sub-types, like Offerings, too, all of them
	return fmt.Sprintf("Name: %s,\nRequirements: %s,\nOffering: %s", instanceType.Name,
		RequirementsToString(instanceType.Requirements), OfferingsToString(instanceType.Offerings))
}

func InstanceTypesToString(instanceTypes []*cloudprovider.InstanceType) string {
	if instanceTypes == nil {
		return "<nil>"
	}
	var buf bytes.Buffer
	for _, instanceType := range instanceTypes {
		buf.WriteString(InstanceTypeToString(instanceType) + "\n")
	}
	return buf.String()
}

// Similar for IT Requirements
// karpenter.sh/capacity-type In [on-demand spot]
// topology.k8s.aws/zone-id In [usw2-az1 usw2-az2 usw2-az3],
// topology.kubernetes.io/zone In [us-west-2a us-west-2b us-west-2c]
func RequirementsToString(requirements scheduling.Requirements) string {
	if requirements == nil {
		return "<nil>"
	}
	capacityType := requirements.Get("karpenter.sh/capacity-type")
	zoneID := requirements.Get("topology.k8s.aws/zone-id")
	zone := requirements.Get("topology.kubernetes.io/zone")
	return fmt.Sprintf("{%s, %s, %s}", RequirementToString(capacityType), RequirementToString(zoneID), RequirementToString(zone))
}

func RequirementToString(requirement *scheduling.Requirement) string {
	if requirement == nil {
		return "<nil>"
	}
	return fmt.Sprintf("{Key: %s, Operator: %s, Values: %v}", requirement.Key, requirement.Operator(), requirement.Values())
}

// Similar for IT Offerings (Price, Availability)
func OfferingToString(offering *cloudprovider.Offering) string {
	if offering == nil {
		return "<nil>"
	}
	return fmt.Sprintf("{Requirements: %v, Price: %f, Available: %t}", RequirementsToString(offering.Requirements), offering.Price, offering.Available)
}

func OfferingsToString(offerings cloudprovider.Offerings) string {
	if offerings == nil {
		return "<nil>"
	}
	var buf bytes.Buffer
	for _, offering := range offerings {
		buf.WriteString(OfferingToString(&offering) + "\n")
	}
	return buf.String()
}

func BindingsToString(bindings map[types.NamespacedName]string) string {
	var buf bytes.Buffer
	for podNamespacedName, nodeName := range bindings {
		buf.WriteString(fmt.Sprintf("{Namespace: %s, Name: %s}: %s,", podNamespacedName.Namespace, podNamespacedName.Name, nodeName))
	}

	if buf.Len() > 0 {
		buf.Truncate(buf.Len() - 1) // Remove the trailing ","
	}

	return buf.String()
}

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

package scheduling

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"sigs.k8s.io/controller-runtime/pkg/client"
)
func TopologyListOptions(namespace string, labelSelector *metav1.LabelSelector) *client.ListOptions {
	selector := labels.Everything()
	if labelSelector == nil {
		return &client.ListOptions{Namespace: namespace, LabelSelector: selector}
	}
	for key, value := range labelSelector.MatchLabels {
		requirement, err := labels.NewRequirement(key, selection.Equals, []string{value})
		if err != nil {
			return &client.ListOptions{Namespace: namespace, LabelSelector: labels.Nothing()}
		}
		selector = selector.Add(*requirement)
	}
	for _, expression := range labelSelector.MatchExpressions {
		requirement, err := labels.NewRequirement(expression.Key, mapOperator(expression.Operator), expression.Values)
		if err != nil {
			return &client.ListOptions{Namespace: namespace, LabelSelector: labels.Nothing()}
		}
		selector = selector.Add(*requirement)
	}
	return &client.ListOptions{Namespace: namespace, LabelSelector: selector}
}

func mapOperator(operator metav1.LabelSelectorOperator) selection.Operator {
	switch operator {
	case metav1.LabelSelectorOpIn:
		return selection.In
	case metav1.LabelSelectorOpNotIn:
		return selection.NotIn
	case metav1.LabelSelectorOpExists:
		return selection.Exists
	case metav1.LabelSelectorOpDoesNotExist:
		return selection.DoesNotExist
	}
	// this shouldn't occur as we cover all valid cases of LabelSelectorOperator that the API allows.  If it still
	// does occur somehow we'll panic just later when the requirement throws an error.,
	return ""
}

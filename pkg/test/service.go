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

package test

import (
	"fmt"

	"github.com/imdario/mergo"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ServiceOptions customizes a Service.
type ServiceOptions struct {
	metav1.ObjectMeta
	// Selector is the Service's pod selector. A nil selector produces a Service that selects no pods, which is
	// meaningfully different from an empty one that selects every pod.
	Selector map[string]string
	Ports    []corev1.ServicePort
}

// Service creates a test Service with defaults that can be overridden by ServiceOptions.
// Overrides are applied in order, with a last write wins semantic.
func Service(overrides ...ServiceOptions) *corev1.Service {
	options := ServiceOptions{}
	for _, opts := range overrides {
		if err := mergo.Merge(&options, opts, mergo.WithOverride); err != nil {
			panic(fmt.Sprintf("Failed to merge service options: %s", err))
		}
	}
	if options.Ports == nil {
		options.Ports = []corev1.ServicePort{{Port: 80}}
	}
	return &corev1.Service{
		ObjectMeta: NamespacedObjectMeta(options.ObjectMeta),
		Spec: corev1.ServiceSpec{
			Selector: options.Selector,
			Ports:    options.Ports,
		},
	}
}

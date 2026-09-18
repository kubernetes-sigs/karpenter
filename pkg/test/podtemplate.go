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
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type PodTemplateOptions struct {
	metav1.ObjectMeta
	PodOptions PodOptions
}

func PodTemplate(overrides ...PodTemplateOptions) *v1.PodTemplate {
	options := PodTemplateOptions{}
	for _, opts := range overrides {
		if err := mergo.Merge(&options, opts, mergo.WithOverride); err != nil {
			panic(fmt.Sprintf("Failed to merge pod template options: %s", err))
		}
	}
	if options.PodOptions.Image == "" {
		options.PodOptions.Image = DefaultImage
	}
	pod := Pod(options.PodOptions)
	return &v1.PodTemplate{
		ObjectMeta: NamespacedObjectMeta(options.ObjectMeta),
		Template: v1.PodTemplateSpec{
			ObjectMeta: pod.ObjectMeta,
			Spec:       pod.Spec,
		},
	}
}

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

package config

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestConfig(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Config Suite")
}

var _ = Describe("Config", func() {
	Describe("Validation", func() {
		It("should return error for empty driver", func() {
			config := &Config{
				Driver: "",
				Mappings: []Mapping{
					{
						Name: "test-mapping",
						NodeSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{
								"test": "value",
							},
						},
						ResourceSlice: resourcev1.ResourceSliceSpec{
							Driver: "test-driver",
							Pool: resourcev1.ResourcePool{
								Name:               "test-pool",
								ResourceSliceCount: 1,
							},
						},
					},
				},
			}
			err := config.Validate()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("driver name cannot be empty"))
		})
	})
})

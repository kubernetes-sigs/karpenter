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

package dra_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("DRA ConfigMap Integration", func() {
	var draConfigMap *corev1.ConfigMap

	BeforeEach(func() {
		// Create a basic ConfigMap for DRA configuration
		draConfigMap = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "dra-kwok-configmap",
				Namespace: "karpenter",
			},
			Data: map[string]string{
				"config.yaml": `
deviceClasses:
  - name: nvidia-gpu
    resources:
      - name: nvidia.com/gpu
        count: 8
  - name: intel-gpu
    resources:
      - name: intel.com/gpu
        count: 4
`,
			},
		}
	})

	AfterEach(func() {
		// Clean up ConfigMap if it exists
		if draConfigMap != nil {
			_ = env.Client.Delete(env, draConfigMap)
		}
	})

	It("should create and read ConfigMap successfully", func() {
		// Create ConfigMap
		Expect(env.Client.Create(env, draConfigMap)).To(Succeed())

		// Verify ConfigMap exists
		fetchedCM := &corev1.ConfigMap{}
		Eventually(func() error {
			return env.Client.Get(env,
				client.ObjectKey{Name: "dra-kwok-configmap", Namespace: "karpenter"},
				fetchedCM)
		}).WithTimeout(10 * time.Second).Should(Succeed())

		// Verify content
		Expect(fetchedCM.Data).To(HaveKey("config.yaml"))
		Expect(fetchedCM.Data["config.yaml"]).To(ContainSubstring("nvidia-gpu"))
	})

	It("should update ConfigMap configuration", func() {
		// Create initial ConfigMap
		Expect(env.Client.Create(env, draConfigMap)).To(Succeed())

		// Update the configuration
		Eventually(func() error {
			fetchedCM := &corev1.ConfigMap{}
			if err := env.Client.Get(env,
				client.ObjectKey{Name: "dra-kwok-configmap", Namespace: "karpenter"},
				fetchedCM); err != nil {
				return err
			}

			fetchedCM.Data["config.yaml"] = `
deviceClasses:
  - name: amd-gpu
    resources:
      - name: amd.com/gpu
        count: 2
`
			return env.Client.Update(env, fetchedCM)
		}).WithTimeout(10 * time.Second).Should(Succeed())

		// Verify update
		fetchedCM := &corev1.ConfigMap{}
		Eventually(func() string {
			_ = env.Client.Get(env,
				client.ObjectKey{Name: "dra-kwok-configmap", Namespace: "karpenter"},
				fetchedCM)
			return fetchedCM.Data["config.yaml"]
		}).WithTimeout(10 * time.Second).Should(ContainSubstring("amd-gpu"))
	})

	It("should handle ConfigMap deletion", func() {
		// Create ConfigMap
		Expect(env.Client.Create(env, draConfigMap)).To(Succeed())

		// Delete ConfigMap
		Expect(env.Client.Delete(env, draConfigMap)).To(Succeed())

		// Verify deletion
		fetchedCM := &corev1.ConfigMap{}
		Eventually(func() error {
			return env.Client.Get(env,
				client.ObjectKey{Name: "dra-kwok-configmap", Namespace: "karpenter"},
				fetchedCM)
		}).WithTimeout(10 * time.Second).Should(Not(Succeed()))

		// Clear reference so AfterEach doesn't try to delete again
		draConfigMap = nil
	})
})

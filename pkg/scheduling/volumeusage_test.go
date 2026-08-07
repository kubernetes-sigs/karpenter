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

package scheduling

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakecr "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var _ = Describe("VolumeUsage", func() {
	var (
		ctx    context.Context
		scheme *runtime.Scheme
	)

	BeforeEach(func() {
		ctx = context.Background()
		scheme = runtime.NewScheme()
		Expect(v1.AddToScheme(scheme)).To(Succeed())
		Expect(storagev1.AddToScheme(scheme)).To(Succeed())
	})

	Describe("GetVolumes", func() {
		newPod := func(claimName string) *v1.Pod {
			return &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
				Spec: v1.PodSpec{
					Volumes: []v1.Volume{{
						Name: "data",
						VolumeSource: v1.VolumeSource{
							PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{ClaimName: claimName},
						},
					}},
				},
			}
		}

		It("should tolerate a missing PersistentVolume and not return an error", func() {
			// PVC references a PV that has been deleted from the cluster (e.g. manually).
			// Without this tolerance, one missing PV would fail node-state reconciliation
			// and could block cluster sync after a controller restart.
			pvc := &v1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pvc", Namespace: "default"},
				Spec: v1.PersistentVolumeClaimSpec{
					VolumeName:       "test-pv",
					StorageClassName: strPtr("test-storage-class"),
				},
			}
			kubeClient := fakecr.NewClientBuilder().WithScheme(scheme).WithObjects(pvc).Build()

			volumes, err := GetVolumes(ctx, kubeClient, newPod("test-pvc"))
			Expect(err).ToNot(HaveOccurred())
			Expect(volumes).To(BeEmpty())
		})

		It("should return volumes correctly when the PersistentVolume exists", func() {
			pv := &v1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pv"},
				Spec: v1.PersistentVolumeSpec{
					PersistentVolumeSource: v1.PersistentVolumeSource{
						CSI: &v1.CSIPersistentVolumeSource{Driver: "ebs.csi.aws.com", VolumeHandle: "vol-test"},
					},
				},
			}
			pvc := &v1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pvc", Namespace: "default"},
				Spec: v1.PersistentVolumeClaimSpec{
					VolumeName:       "test-pv",
					StorageClassName: strPtr("test-storage-class"),
				},
			}
			kubeClient := fakecr.NewClientBuilder().WithScheme(scheme).WithObjects(pv, pvc).Build()

			volumes, err := GetVolumes(ctx, kubeClient, newPod("test-pvc"))
			Expect(err).ToNot(HaveOccurred())
			Expect(volumes).To(HaveKey("ebs.csi.aws.com"))
		})

		It("should tolerate a missing PersistentVolumeClaim and not return an error", func() {
			// Existing behavior: a missing PVC is already tolerated. This test guards it.
			kubeClient := fakecr.NewClientBuilder().WithScheme(scheme).Build()

			volumes, err := GetVolumes(ctx, kubeClient, newPod("nonexistent-pvc"))
			Expect(err).ToNot(HaveOccurred())
			Expect(volumes).To(BeEmpty())
		})
	})
})

func strPtr(s string) *string {
	return &s
}

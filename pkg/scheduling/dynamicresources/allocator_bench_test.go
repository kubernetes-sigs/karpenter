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

package dynamicresources_test

import (
	"context"
	"fmt"
	"testing"
	"unique"

	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/scheduling/dynamicresources"
)

// templateNodeClaim mirrors the scheduler's in-flight NodeClaim adapter: every ResourceSlices call wraps the templates
// of every candidate instance type.
type templateNodeClaim struct {
	instanceTypes []dynamicresources.InstanceTypeID
	templates     map[dynamicresources.InstanceTypeID][]*cloudprovider.ResourceSliceTemplate
	calls         int
}

func (t *templateNodeClaim) ID() dynamicresources.NodeClaimID        { return unique.Make("bench-nc") }
func (t *templateNodeClaim) NodeName() string                        { return "" }
func (t *templateNodeClaim) NodePoolID() dynamicresources.NodePoolID { return unique.Make("bench-np") }
func (t *templateNodeClaim) Requirements() scheduling.Requirements {
	return scheduling.NewRequirements()
}
func (t *templateNodeClaim) InstanceTypes() []dynamicresources.InstanceTypeID { return t.instanceTypes }
func (t *templateNodeClaim) ResourceSlices() map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice {
	t.calls++
	slices := make(map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice, len(t.templates))
	for itID, templates := range t.templates {
		for _, template := range templates {
			slices[itID] = append(slices[itID], dynamicresources.NewTemplateSlice(template))
		}
	}
	return slices
}

// BenchmarkAllocate measures a single-device allocation against an in-flight NodeClaim whose candidate instance types
// all publish template devices.
func BenchmarkAllocate(b *testing.B) {
	scheme := runtime.NewScheme()
	if err := resourcev1.AddToScheme(scheme); err != nil {
		b.Fatal(err)
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&resourcev1.DeviceClass{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu"},
		Spec: resourcev1.DeviceClassSpec{
			Selectors: []resourcev1.DeviceSelector{{CEL: &resourcev1.CELDeviceSelector{Expression: `device.driver == "gpu.example.com"`}}},
		},
	}).Build()
	claims := []*resourcev1.ResourceClaim{makeClaim("c1", exactRequest("req-1", "gpu", 1))}

	for _, n := range []int{100, 400, 800} {
		nc := &templateNodeClaim{templates: map[dynamicresources.InstanceTypeID][]*cloudprovider.ResourceSliceTemplate{}}
		for i := range n {
			itID := unique.Make(fmt.Sprintf("it-%d", i))
			nc.instanceTypes = append(nc.instanceTypes, itID)
			nc.templates[itID] = []*cloudprovider.ResourceSliceTemplate{
				makeTemplate("gpu.example.com", "gpus", "gpu-0", "gpu-1", "gpu-2", "gpu-3", "gpu-4", "gpu-5", "gpu-6", "gpu-7"),
			}
		}
		b.Run(fmt.Sprintf("instanceTypes=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			nc.calls = 0
			b.ResetTimer()
			for b.Loop() {
				alloc := dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, kubeClient, nil)
				if _, err := alloc.Allocate(context.Background(), nc, claims); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(nc.calls)/float64(b.N), "ResourceSlices/op")
		})
	}
}

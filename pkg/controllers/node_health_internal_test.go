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

package controllers

import (
	"context"
	"time"

	"github.com/awslabs/operatorpkg/controller"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/test"
)

type stubController struct{}

func (*stubController) Register(context.Context, manager.Manager) error {
	return nil
}

var _ = Describe("Node Health Controller Wiring", func() {
	var cloudProvider *fake.CloudProvider
	var clock *clocktesting.FakeClock
	var recorder *test.EventRecorder

	BeforeEach(func() {
		cloudProvider = fake.NewCloudProvider()
		clock = clocktesting.NewFakeClock(time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC))
		recorder = test.NewEventRecorder()
	})

	It("appends node repair when policies are valid", func() {
		controllers := appendNodeHealthController(context.Background(), nil, nil, cloudProvider, clock, recorder)
		Expect(controllers).To(HaveLen(1))
	})

	It("disables only node repair when policies are invalid", func() {
		existingController := &stubController{}
		controllers := []controller.Controller{existingController}
		cloudProvider.RepairPolicy = []cloudprovider.RepairPolicy{
			{
				ConditionType:   "BadNode",
				ConditionStatus: corev1.ConditionFalse,
				ReasonRegex:     "[",
				Action:          cloudprovider.ReplaceNode,
			},
			{
				ConditionType:   "BadNode",
				ConditionStatus: corev1.ConditionFalse,
				Action:          cloudprovider.ReplaceNode,
			},
		}

		controllers = appendNodeHealthController(context.Background(), controllers, nil, cloudProvider, clock, recorder)
		Expect(controllers).To(ConsistOf(existingController))
	})
})

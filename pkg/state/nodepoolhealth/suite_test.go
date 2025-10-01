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

package nodepoolhealth_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"

	"sigs.k8s.io/karpenter/pkg/state/nodepoolhealth"
)

var (
	npState nodepoolhealth.State
	npUUID  types.UID
)

var _ = BeforeSuite(func() {
	npUUID = uuid.NewUUID()
	npState = nodepoolhealth.State{}
})

var _ = AfterEach(func() {
	npState.SetStatus(npUUID, nodepoolhealth.StatusUnknown)
})

func TestAPIs(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "NodePoolHealthState")
}

var _ = Describe("NodePoolHealthState", func() {
	It("should expect status unknown for a new nodePool with empty buffer", func() {
		Expect(npState.Status(npUUID)).To(Equal(nodepoolhealth.StatusUnknown))
	})
	It("should expect status healthy for a nodePool with one true entry", func() {
		npState.Update(npUUID, true)
		Expect(npState.Status(npUUID)).To(Equal(nodepoolhealth.StatusHealthy))
	})
	It("should expect status unhealthy for a nodePool with two false entries", func() {
		npState.Update(npUUID, false)
		npState.Update(npUUID, false)
		Expect(npState.Status(npUUID)).To(Equal(nodepoolhealth.StatusUnhealthy))
	})
	It("should expect status unhealthy for a nodePool with two false entries and one true entry", func() {
		npState.SetStatus(npUUID, nodepoolhealth.StatusUnhealthy)
		npState.Update(npUUID, true)
		Expect(npState.Status(npUUID)).To(Equal(nodepoolhealth.StatusUnhealthy))
	})
	It("should return the correct status in case of multiple nodepools stored in the state", func() {
		npState.Update(npUUID, true)
		npUUID2 := uuid.NewUUID()
		npState.Update(npUUID2, false)
		npState.Update(npUUID2, false)
		Expect(npState.Status(npUUID)).To(Equal(nodepoolhealth.StatusHealthy))
		Expect(npState.Status(npUUID2)).To(Equal(nodepoolhealth.StatusUnhealthy))
		npState.SetStatus(npUUID2, nodepoolhealth.StatusUnknown)
		Expect(npState.Status(npUUID2)).To(Equal(nodepoolhealth.StatusUnknown))
	})
})

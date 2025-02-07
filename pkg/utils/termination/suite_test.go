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

package termination_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"sigs.k8s.io/karpenter/pkg/apis"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	"sigs.k8s.io/karpenter/pkg/test/v1alpha1"
	"sigs.k8s.io/karpenter/pkg/utils/termination"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"
)

var (
	ctx           context.Context
	env           *test.Environment
	cloudProvider *fake.CloudProvider
)

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "TerminationUtils")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(test.WithCRDs(apis.CRDs...), test.WithCRDs(v1alpha1.CRDs...))
	cloudProvider = fake.NewCloudProvider()
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = AfterEach(func() {
	cloudProvider.Reset()
	ExpectCleanedUp(ctx, env.Client)
})

var _ = Describe("TerminationUtils", func() {
	var nodeClaim *v1.NodeClaim
	BeforeEach(func() {
		nodeClaim = test.NodeClaim()
		cloudProvider.CreatedNodeClaims[nodeClaim.Status.ProviderID] = nodeClaim
	})
	It("should return false if cloudProvider Delete does not return any error", func() {
		ExpectApplied(ctx, env.Client, nodeClaim)
		instanceTerminated, err := termination.EnsureTerminated(ctx, env.Client, nodeClaim, cloudProvider)
		Expect(len(cloudProvider.DeleteCalls)).To(BeEquivalentTo(1))
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeInstanceTerminating).IsTrue()).To(BeTrue())
		Expect(instanceTerminated).To(BeFalse())
		Expect(err).NotTo(HaveOccurred())
	})
	It("should return false if cloudProvider Delete does not return a not found error", func() {
		ExpectApplied(ctx, env.Client, nodeClaim)
		cloudProvider.NextDeleteErr = errors.New("fake error")
		// This will call cloudProvider.Delete()
		instanceTerminated, err := termination.EnsureTerminated(ctx, env.Client, nodeClaim, cloudProvider)
		Expect(instanceTerminated).To(BeFalse())
		Expect(err).To(HaveOccurred())
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeInstanceTerminating).IsTrue()).To(BeFalse())
	})
	It("should call cloudProvider Delete and return true if cloudProvider Delete returns not found error", func() {
		ExpectApplied(ctx, env.Client, nodeClaim)

		cloudProvider.NextDeleteErr = cloudprovider.NewNodeClaimNotFoundError(fmt.Errorf("no nodeclaim exists"))
		instanceTerminated, err := termination.EnsureTerminated(ctx, env.Client, nodeClaim, cloudProvider)

		Expect(instanceTerminated).To(BeTrue())
		Expect(err).NotTo(HaveOccurred())
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeInstanceTerminating).IsTrue()).To(BeTrue())
	})
	It("shouldn't mark the root condition of the NodeClaim as unknown when setting the Termination condition", func() {
		for _, cond := range []string{
			v1.ConditionTypeLaunched,
			v1.ConditionTypeRegistered,
			v1.ConditionTypeInitialized,
		} {
			nodeClaim.StatusConditions().SetTrue(cond)
		}
		ExpectApplied(ctx, env.Client, nodeClaim)
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Root().IsTrue())
		ExpectApplied(ctx, env.Client, nodeClaim)
		cloudProvider.NextDeleteErr = cloudprovider.NewNodeClaimNotFoundError(fmt.Errorf("no nodeclaim exists"))
		instanceTerminated, err := termination.EnsureTerminated(ctx, env.Client, nodeClaim, cloudProvider)
		Expect(instanceTerminated).To(BeTrue())
		Expect(err).NotTo(HaveOccurred())
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeInstanceTerminating).IsTrue()).To(BeTrue())
		Expect(nodeClaim.StatusConditions().Root().IsTrue())
	})
})

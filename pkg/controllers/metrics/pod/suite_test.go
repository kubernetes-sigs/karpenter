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

package pod_test

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	. "sigs.k8s.io/karpenter/pkg/utils/testing"

	"sigs.k8s.io/karpenter/pkg/controllers/metrics/pod"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var podController *pod.Controller
var ctx context.Context
var env *test.Environment

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "PodMetrics")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment()
	podController = pod.NewController(env.Client)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = Describe("Pod Metrics", func() {
	It("should update the pod state metrics", func() {
		p := test.Pod()
		ExpectApplied(ctx, env.Client, p)
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(p))

		_, found := FindMetricWithLabelValues("karpenter_pods_state", map[string]string{
			"name":      p.GetName(),
			"namespace": p.GetNamespace(),
		})
		Expect(found).To(BeTrue())
	})
	It("should update the pod state metrics with pod phase", func() {
		p := test.Pod()
		ExpectApplied(ctx, env.Client, p)
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(p))

		_, found := FindMetricWithLabelValues("karpenter_pods_state", map[string]string{
			"name":      p.GetName(),
			"namespace": p.GetNamespace(),
		})
		Expect(found).To(BeTrue())

		p.Status.Phase = v1.PodRunning
		ExpectApplied(ctx, env.Client, p)
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(p))

		_, found = FindMetricWithLabelValues("karpenter_pods_state", map[string]string{
			"name":      p.GetName(),
			"namespace": p.GetNamespace(),
			"phase":     string(p.Status.Phase),
		})
		Expect(found).To(BeTrue())
	})
	It("should delete the pod state metric on pod delete", func() {
		p := test.Pod()
		ExpectApplied(ctx, env.Client, p)
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(p))

		ExpectDeleted(ctx, env.Client, p)
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(p))

		_, found := FindMetricWithLabelValues("karpenter_pods_state", map[string]string{
			"name":      p.GetName(),
			"namespace": p.GetNamespace(),
		})
		Expect(found).To(BeFalse())
	})
})

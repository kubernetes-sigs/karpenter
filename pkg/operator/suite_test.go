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

package operator_test

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	prometheusmodel "github.com/prometheus/client_model/go"
	"github.com/samber/lo"
	"k8s.io/client-go/rest"

	"sigs.k8s.io/karpenter/pkg/apis"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	"sigs.k8s.io/karpenter/pkg/test/v1alpha1"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"
)

var ctx context.Context
var env *test.Environment

func TestOperator(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Operator")
}

var _ = BeforeSuite(func() {
	// A low QPS with no burst headroom so that back-to-back requests are throttled by client-go's
	// client-side rate limiter: the first request takes the only token and each following one waits ~200ms.
	env = test.NewEnvironment(test.WithCRDs(apis.CRDs...), test.WithCRDs(v1alpha1.CRDs...), test.WithConfigOptions(func(config *rest.Config) {
		config.QPS = 5
		config.Burst = 1
	}))
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = Describe("Operator", func() {
	It("should fire a metric with the build_info", func() {
		m, found := FindMetricWithLabelValues("karpenter_build_info", map[string]string{})
		Expect(found).To(BeTrue())

		for _, label := range []string{"version", "goversion", "goarch", "commit"} {
			_, ok := lo.Find(m.GetLabel(), func(l *prometheusmodel.LabelPair) bool { return lo.FromPtr(l.Name) == label })
			Expect(ok).To(BeTrue())
		}
	})
	It("should fire a metric for client-go rate limiter latency when API calls are throttled", func() {
		for range 3 {
			Expect(env.Client.Create(ctx, test.NodePool())).To(Succeed())
		}
		m, found := FindMetricWithLabelValues("client_go_rate_limiter_duration_seconds", map[string]string{
			"verb":        "CREATE",
			"group":       "karpenter.sh",
			"version":     "v1",
			"kind":        "nodepools",
			"subresource": "",
		})
		Expect(found).To(BeTrue())
		Expect(m.GetHistogram().GetSampleCount()).To(BeNumerically("==", 3))
		// At least one throttled create waited for a token, which only the rate limiter can account for
		bucket, ok := lo.Find(m.GetHistogram().GetBucket(), func(b *prometheusmodel.Bucket) bool { return b.GetUpperBound() == 0.1 })
		Expect(ok).To(BeTrue())
		Expect(bucket.GetCumulativeCount()).To(BeNumerically("<", 3))
	})
})

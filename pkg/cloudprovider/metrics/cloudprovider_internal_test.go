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

package metrics

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	dto "github.com/prometheus/client_model/go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
)

var _ = Describe("CloudProvider error metric labels", func() {
	BeforeEach(func() {
		ErrorsTotal.Reset()
	})
	AfterEach(func() {
		ErrorsTotal.Reset()
	})

	It("should expose the NodePool label", func() {
		ctx := injection.WithControllerName(context.Background(), "provisioner")
		providerErr := cloudprovider.NewInsufficientCapacityError(errors.New("not enough capacity"))
		decorated := Decorate(&erroringCloudProvider{createErr: providerErr})
		_, err := decorated.Create(ctx, &v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{v1.NodePoolLabelKey: "default"},
		}})
		Expect(err).To(MatchError(providerErr))

		metricFamilies, err := crmetrics.Registry.Gather()
		Expect(err).ToNot(HaveOccurred())
		metricFamily := findMetricFamily(metricFamilies, "karpenter_cloudprovider_errors_total")
		Expect(metricFamily).ToNot(BeNil())
		Expect(metricFamily.Metric).To(HaveLen(1))
		Expect(metricFamily.Metric[0].GetCounter().GetValue()).To(Equal(float64(1)))
		Expect(metricLabels(metricFamily.Metric[0])).To(Equal(map[string]string{
			"controller": "provisioner",
			"error":      "InsufficientCapacityError",
			"method":     "Create",
			"nodepool":   "default",
			"provider":   "fake",
		}))
	})

	It("should expose the NodePool returned with a Get error", func() {
		ctx := injection.WithControllerName(context.Background(), "nodeclaim-lifecycle")
		providerErr := errors.New("get failed")
		decorated := Decorate(&erroringCloudProvider{
			getNodeClaim: &v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{v1.NodePoolLabelKey: "default"},
			}},
			getErr: providerErr,
		})
		_, err := decorated.Get(ctx, "nodeclaim-id")
		Expect(err).To(MatchError(providerErr))

		metricFamilies, err := crmetrics.Registry.Gather()
		Expect(err).ToNot(HaveOccurred())
		metricFamily := findMetricFamily(metricFamilies, "karpenter_cloudprovider_errors_total")
		Expect(metricFamily).ToNot(BeNil())
		Expect(metricFamily.Metric).To(HaveLen(1))
		Expect(metricLabels(metricFamily.Metric[0])).To(Equal(map[string]string{
			"controller": "nodeclaim-lifecycle",
			"error":      "",
			"method":     "Get",
			"nodepool":   "default",
			"provider":   "fake",
		}))
	})

	It("should derive the NodePool from available CloudProvider arguments", func() {
		Expect(nodePoolNameForNodeClaim(&v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{v1.NodePoolLabelKey: "nodeclaim-pool"},
		}})).To(Equal("nodeclaim-pool"))
		Expect(nodePoolNameForNodePool(&v1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "nodepool-argument"}})).To(Equal("nodepool-argument"))
		Expect(nodePoolNameForNodeClaim(nil)).To(BeEmpty())
		Expect(nodePoolNameForNodePool(nil)).To(BeEmpty())
	})
})

type erroringCloudProvider struct {
	cloudprovider.CloudProvider
	createErr    error
	getNodeClaim *v1.NodeClaim
	getErr       error
}

func (erroringCloudProvider) Name() string {
	return "fake"
}

func (e erroringCloudProvider) Create(context.Context, *v1.NodeClaim) (*v1.NodeClaim, error) {
	return nil, e.createErr
}

func (e erroringCloudProvider) Get(context.Context, string) (*v1.NodeClaim, error) {
	return e.getNodeClaim, e.getErr
}

func findMetricFamily(metricFamilies []*dto.MetricFamily, name string) *dto.MetricFamily {
	for _, metricFamily := range metricFamilies {
		if metricFamily.GetName() == name {
			return metricFamily
		}
	}
	return nil
}

func metricLabels(metric *dto.Metric) map[string]string {
	labels := map[string]string{}
	for _, label := range metric.Label {
		labels[label.GetName()] = label.GetValue()
	}
	return labels
}

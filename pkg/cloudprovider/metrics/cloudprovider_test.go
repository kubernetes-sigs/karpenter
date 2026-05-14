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

package metrics_test

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/metrics"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

const (
	durationMetricName = "karpenter_cloudprovider_duration_seconds"
	errorsMetricName   = "karpenter_cloudprovider_errors_total"
	providerName       = "fake"
)

var _ = Describe("Cloudprovider", func() {
	var nodeClaimNotFoundErr = cloudprovider.NewNodeClaimNotFoundError(errors.New("not found"))
	var insufficientCapacityErr = cloudprovider.NewInsufficientCapacityError(errors.New("not enough capacity"))
	var nodeClassNotReadyErr = cloudprovider.NewNodeClassNotReadyError(errors.New("not ready"))
	var unknownErr = errors.New("this is an error we don't know about")

	Describe("CloudProvider nodeclaim errors via GetErrorTypeLabelValue()", func() {
		Context("when the error is known", func() {
			It("nodeclaim not found should be recognized", func() {
				Expect(metrics.GetErrorTypeLabelValue(nodeClaimNotFoundErr)).To(Equal(metrics.NodeClaimNotFoundError))
			})
			It("insufficient capacity should be recognized", func() {
				Expect(metrics.GetErrorTypeLabelValue(insufficientCapacityErr)).To(Equal(metrics.InsufficientCapacityError))
			})
			It("nodeclass not ready should be recognized", func() {
				Expect(metrics.GetErrorTypeLabelValue(nodeClassNotReadyErr)).To(Equal(metrics.NodeClassNotReadyError))
			})
		})
		Context("when the error is unknown", func() {
			It("should always return empty string", func() {
				Expect(metrics.GetErrorTypeLabelValue(unknownErr)).To(Equal(metrics.MetricLabelErrorDefaultVal))
			})
		})
	})

	Describe("decorator emits metrics for CloudProvider methods", func() {
		var fakeProvider *fake.CloudProvider
		var decorated cloudprovider.CloudProvider

		BeforeEach(func() {
			fakeProvider = fake.NewCloudProvider()
			fakeProvider.Reset()
			decorated = metrics.Decorate(fakeProvider)
		})

		// Use a unique controller name per test so counter values don't collide
		// with other test runs that share the global prometheus registry.
		ctxFor := func(controller string) context.Context {
			return injection.WithControllerName(context.Background(), controller)
		}

		Context("Create", func() {
			It("should record duration and increment errors_total when the call fails", func() {
				ctx := ctxFor("create-error")
				fakeProvider.NextCreateErr = insufficientCapacityErr
				_, err := decorated.Create(ctx, &v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "test"}})
				Expect(err).To(HaveOccurred())

				ExpectMetricHistogramSampleCountValue(durationMetricName, 1, map[string]string{
					"controller": "create-error",
					"method":     "Create",
					"provider":   providerName,
				})
				ExpectMetricCounterValue(metrics.ErrorsTotal, 1, map[string]string{
					"controller": "create-error",
					"method":     "Create",
					"provider":   providerName,
					"error":      metrics.InsufficientCapacityError,
				})
			})
		})

		Context("Delete", func() {
			It("should record duration and increment errors_total with NodeClaimNotFoundError label", func() {
				ctx := ctxFor("delete-error")
				_, err := decorated.Get(ctx, "missing-id")
				Expect(err).To(HaveOccurred())

				err = decorated.Delete(ctx, &v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "test"}})
				Expect(err).To(HaveOccurred())

				ExpectMetricHistogramSampleCountValue(durationMetricName, 1, map[string]string{
					"controller": "delete-error",
					"method":     "Delete",
					"provider":   providerName,
				})
				ExpectMetricCounterValue(metrics.ErrorsTotal, 1, map[string]string{
					"controller": "delete-error",
					"method":     "Delete",
					"provider":   providerName,
					"error":      metrics.NodeClaimNotFoundError,
				})
			})
		})

		Context("Get", func() {
			It("should record duration and increment errors_total when the call fails", func() {
				ctx := ctxFor("get-error")
				fakeProvider.NextGetErr = nodeClassNotReadyErr
				_, err := decorated.Get(ctx, "any-id")
				Expect(err).To(HaveOccurred())

				ExpectMetricHistogramSampleCountValue(durationMetricName, 1, map[string]string{
					"controller": "get-error",
					"method":     "Get",
					"provider":   providerName,
				})
				ExpectMetricCounterValue(metrics.ErrorsTotal, 1, map[string]string{
					"controller": "get-error",
					"method":     "Get",
					"provider":   providerName,
					"error":      metrics.NodeClassNotReadyError,
				})
			})
		})

		Context("List", func() {
			It("should record duration on success without incrementing errors_total", func() {
				ctx := ctxFor("list-success")
				_, err := decorated.List(ctx)
				Expect(err).ToNot(HaveOccurred())

				ExpectMetricHistogramSampleCountValue(durationMetricName, 1, map[string]string{
					"controller": "list-success",
					"method":     "List",
					"provider":   providerName,
				})
				_, ok := FindMetricWithLabelValues(errorsMetricName, map[string]string{
					"controller": "list-success",
					"method":     "List",
					"provider":   providerName,
				})
				Expect(ok).To(BeFalse())
			})
		})

		Context("GetInstanceTypes", func() {
			It("should record duration and increment errors_total when the call fails", func() {
				ctx := ctxFor("getinstancetypes-error")
				fakeProvider.ErrorsForNodePool["pool"] = unknownErr
				_, err := decorated.GetInstanceTypes(ctx, &v1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "pool"}})
				Expect(err).To(HaveOccurred())

				ExpectMetricHistogramSampleCountValue(durationMetricName, 1, map[string]string{
					"controller": "getinstancetypes-error",
					"method":     "GetInstanceTypes",
					"provider":   providerName,
				})
				ExpectMetricCounterValue(metrics.ErrorsTotal, 1, map[string]string{
					"controller": "getinstancetypes-error",
					"method":     "GetInstanceTypes",
					"provider":   providerName,
					"error":      metrics.MetricLabelErrorDefaultVal,
				})
			})
		})

		Context("IsDrifted", func() {
			It("should record duration on success without incrementing errors_total", func() {
				ctx := ctxFor("isdrifted-success")
				_, err := decorated.IsDrifted(ctx, &v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "test"}})
				Expect(err).ToNot(HaveOccurred())

				ExpectMetricHistogramSampleCountValue(durationMetricName, 1, map[string]string{
					"controller": "isdrifted-success",
					"method":     "IsDrifted",
					"provider":   providerName,
				})
				_, ok := FindMetricWithLabelValues(errorsMetricName, map[string]string{
					"controller": "isdrifted-success",
					"method":     "IsDrifted",
					"provider":   providerName,
				})
				Expect(ok).To(BeFalse())
			})
		})
	})
})

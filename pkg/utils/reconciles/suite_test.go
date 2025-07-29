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

package reconciles_test

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/utils/reconciles"
)

func TestReconciles(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Reconciles")
}

func contextWithCPURequests(cpuRequests int64) context.Context {
	opts := &options.Options{
		CPURequests: cpuRequests,
	}
	return opts.ToContext(context.Background())
}

var _ = Describe("Reconciles", func() {
	minReconciles := 10
	maxReconciles := 1000
	Context("LinearScaleReconciles Calculations", func() {
		It("should calculate minReconciles for 0.5 CPU core", func() {
			ctx := contextWithCPURequests(0.5 * 1000.0)
			result := reconciles.LinearScaleReconciles(ctx, minReconciles, maxReconciles)
			Expect(result).To(Equal(minReconciles))
		})
		It("should calculate minReconciles for 1 CPU core", func() {
			ctx := contextWithCPURequests(1 * 1000)
			result := reconciles.LinearScaleReconciles(ctx, minReconciles, maxReconciles)
			Expect(result).To(Equal(minReconciles))
		})
		It("should calculate maxReconciles for 60 CPU cores", func() {
			ctx := contextWithCPURequests(60 * 1000)
			result := reconciles.LinearScaleReconciles(ctx, minReconciles, maxReconciles)
			Expect(result).To(Equal(maxReconciles))
		})
		It("should calculate maxReconciles for 100 CPU cores", func() {
			ctx := contextWithCPURequests(100 * 1000)
			result := reconciles.LinearScaleReconciles(ctx, minReconciles, maxReconciles)
			Expect(result).To(Equal(maxReconciles))
		})
		It("should follow the linear scaling formula", func() {
			ctx := contextWithCPURequests(15 * 1000) // 15 cores
			result := reconciles.LinearScaleReconciles(ctx, minReconciles, maxReconciles)
			// At 15 cores
			// slope = (maxReconciles - minReconciles)/59 = (1000-10)/59 = 990/59 = ~16.78
			// result = int(slope * (cores - 1)) + minReconciles ~= 16.78 * (15-1) + 10 = 234 + 10 = 244
			expected := 244
			Expect(result).To(Equal(expected))
		})
		It("should handle fractional CPU cores correctly", func() {
			ctx := contextWithCPURequests(1.5 * 1000.0) // 1.5 cores
			result := reconciles.LinearScaleReconciles(ctx, minReconciles, maxReconciles)
			// At 2 cores (ceil(1.5))
			// slope = (maxReconciles - minReconciles)/59 = (1000-10)/59 = 990/59 = ~16.78
			// result = int(slope * (cores - 1)) + minReconciles ~= 16.78 * (2-1) + 10 = 16 + 10 = 26
			expected := 26
			Expect(result).To(Equal(expected))
		})
	})
})

/*
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
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/cloudprovider/metrics"
)

var _ = Describe("Cloudprovider", func() {
	var machineNotFoundErr = cloudprovider.NewMachineNotFoundError(errors.New("not found"))
	var insufficientCapacityErr = cloudprovider.NewInsufficientCapacityError(errors.New("not enough capacity"))
	var unknownErr = errors.New("this is an error we don't know about")

	Describe("CloudProvider machine errors via GetErrorTypeLabelValue()", func() {
		Context("when the error is known", func() {
			It("machine not found should be recognized", func() {
				Expect(metrics.GetErrorTypeLabelValue(machineNotFoundErr)).To(Equal(metrics.MachineNotFoundError))
			})
			It("insufficient capacity should be recognized", func() {
				Expect(metrics.GetErrorTypeLabelValue(insufficientCapacityErr)).To(Equal(metrics.InsufficientCapacityError))
			})
		})
		Context("when the error is unknown", func() {
			It("should always return empty string", func() {
				Expect(metrics.GetErrorTypeLabelValue(unknownErr)).To(Equal(metrics.MetricLabelErrorDefaultVal))
			})
		})
	})
})

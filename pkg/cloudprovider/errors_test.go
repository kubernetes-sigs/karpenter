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

package cloudprovider_test

import (
	"errors"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

var errCause = errors.New("cause")

var _ = Describe("Cloud provider errors", func() {
	// Each row builds the error a provider returns and the predicate controllers use to recognize it.
	type errorKind struct {
		name    string
		build   func(error) error
		matches func(error) bool
	}
	kinds := []errorKind{
		{"NodeClaimNotFound", func(e error) error { return cloudprovider.NewNodeClaimNotFoundError(e) }, cloudprovider.IsNodeClaimNotFoundError},
		{"InsufficientCapacity", func(e error) error { return cloudprovider.NewInsufficientCapacityError(e) }, cloudprovider.IsInsufficientCapacityError},
		{"NodeClassNotReady", func(e error) error { return cloudprovider.NewNodeClassNotReadyError(e) }, cloudprovider.IsNodeClassNotReadyError},
		{"NodeRebootFailed", func(e error) error { return cloudprovider.NewNodeRebootFailedError(e) }, cloudprovider.IsNodeRebootFailedError},
		{"NodeRebootNotImplemented", func(error) error { return cloudprovider.NewNodeRebootNotImplementedError() }, cloudprovider.IsNodeRebootNotImplementedError},
		{"UnevaluatedNodePool", func(error) error { return cloudprovider.NewUnevaluatedNodePoolError("default") }, cloudprovider.IsUnevaluatedNodePoolError},
	}

	for _, k := range kinds {
		Context(k.name, func() {
			It("should be recognized directly and when wrapped", func() {
				err := k.build(errCause)
				Expect(k.matches(err)).To(BeTrue())
				Expect(k.matches(fmt.Errorf("launching: %w", err))).To(BeTrue())
			})
			It("should not match nil or an unrelated error", func() {
				Expect(k.matches(nil)).To(BeFalse())
				Expect(k.matches(errCause)).To(BeFalse())
			})
			It("should not match any other kind of cloud provider error", func() {
				for _, other := range kinds {
					if other.name == k.name {
						continue
					}
					Expect(k.matches(other.build(errCause))).To(BeFalse(), "%s matched a %s error", k.name, other.name)
				}
			})
		})
	}

	It("should expose the underlying cause through errors.Is", func() {
		for _, err := range []error{
			cloudprovider.NewNodeClaimNotFoundError(errCause),
			cloudprovider.NewInsufficientCapacityError(errCause),
			cloudprovider.NewNodeClassNotReadyError(errCause),
			cloudprovider.NewNodeRebootFailedError(errCause),
			cloudprovider.NewCreateError(errCause, "reason", "message"),
		} {
			Expect(errors.Is(err, errCause)).To(BeTrue(), "%T did not unwrap to its cause", err)
		}
	})
	It("should keep the cause in the error message", func() {
		Expect(cloudprovider.NewNodeClaimNotFoundError(errCause).Error()).To(ContainSubstring("cause"))
		Expect(cloudprovider.NewInsufficientCapacityError(errCause).Error()).To(ContainSubstring("cause"))
		Expect(cloudprovider.NewNodeClassNotReadyError(errCause).Error()).To(ContainSubstring("cause"))
		Expect(cloudprovider.NewCreateError(errCause, "reason", "message").Error()).To(ContainSubstring("cause"))
		Expect(cloudprovider.NewUnevaluatedNodePoolError("default").Error()).To(ContainSubstring(`"default"`))
	})
	It("should keep the condition reason and message on a CreateError", func() {
		err := cloudprovider.NewCreateError(errCause, "LaunchFailed", "no capacity")
		Expect(err.ConditionReason).To(Equal("LaunchFailed"))
		Expect(err.ConditionMessage).To(Equal("no capacity"))
	})

	Context("IgnoreNodeClaimNotFoundError", func() {
		It("should drop a NodeClaimNotFoundError, wrapped or not", func() {
			Expect(cloudprovider.IgnoreNodeClaimNotFoundError(cloudprovider.NewNodeClaimNotFoundError(errCause))).To(Succeed())
			Expect(cloudprovider.IgnoreNodeClaimNotFoundError(fmt.Errorf("deleting: %w", cloudprovider.NewNodeClaimNotFoundError(errCause)))).To(Succeed())
		})
		It("should pass any other error through unchanged", func() {
			Expect(cloudprovider.IgnoreNodeClaimNotFoundError(errCause)).To(MatchError(errCause))
			Expect(cloudprovider.IgnoreNodeClaimNotFoundError(nil)).To(Succeed())
		})
	})
})

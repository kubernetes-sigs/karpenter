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

// Package labelstest registers label policy in tests and restores the previous registry and bound vars afterwards.
package labelstest

import (
	"testing"

	"sigs.k8s.io/karpenter/pkg/labels"
	"sigs.k8s.io/karpenter/pkg/labels/internal/registry"
)

// Register resets the registry, registers opts, and on t's cleanup restores the registry and bound vars to what
// they were before the call. Registration errors fail t. With Ginkgo, pass GinkgoTB() so cleanup runs as
// DeferCleanup.
//
// Ginkgo container bodies (Describe, Context, DescribeTable and Entry arguments) run while the spec tree is built,
// before any BeforeEach, so they see bound vars and the registry as they were before Register. Read them inside
// setup and subject nodes instead; the labelsbind analyzer doesn't flag reads in container bodies.
func Register(t testing.TB, opts ...labels.Option) {
	t.Helper()
	t.Cleanup(registry.Save().Restore)
	registry.Reset()
	if err := labels.Register(opts...); err != nil {
		t.Fatalf("registering labels: %v", err)
	}
}

// Reset clears the registry and restores every bound var to the value it held before it was bound, for suites
// without a testing.TB.
func Reset() {
	registry.Reset()
}

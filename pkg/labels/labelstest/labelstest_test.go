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

package labelstest_test

import (
	"fmt"
	"slices"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"sigs.k8s.io/karpenter/pkg/labels"
	"sigs.k8s.io/karpenter/pkg/labels/labelstest"
)

// recordingTB collects cleanups so a spec can run them and observe the restored state.
type recordingTB struct {
	testing.TB
	cleanups []func()
	fatal    string
}

func (t *recordingTB) Helper()                      {}
func (t *recordingTB) Cleanup(f func())             { t.cleanups = append(t.cleanups, f) }
func (t *recordingTB) Fatalf(f string, args ...any) { t.fatal = fmt.Sprintf(f, args...) }
func (t *recordingTB) runCleanups() {
	for _, cleanup := range slices.Backward(t.cleanups) {
		cleanup()
	}
	t.cleanups = nil
}

// Bound vars must be package-level and marked, like every var in Binds.
// +labels:bound
var bound string

var _ = Describe("labelstest", func() {
	BeforeEach(func() {
		labelstest.Reset()
		bound = "original"
		DeferCleanup(labelstest.Reset)
	})

	It("should register the options and restore the previous registry and binds on cleanup", func() {
		outer, inner := &recordingTB{}, &recordingTB{}
		labelstest.Register(outer, labels.Option{Key: "example.com/outer", Binds: []*string{&bound}})
		labelstest.Register(inner, labels.Option{Key: "example.com/inner", Binds: []*string{&bound}})
		Expect(bound).To(Equal("example.com/inner"))
		Expect(labels.IsWellKnown("example.com/inner")).To(BeTrue())
		Expect(labels.IsWellKnown("example.com/outer")).To(BeFalse())

		inner.runCleanups()
		Expect(bound).To(Equal("example.com/outer"))
		Expect(labels.IsWellKnown("example.com/outer")).To(BeTrue())
		Expect(labels.IsWellKnown("example.com/inner")).To(BeFalse())

		outer.runCleanups()
		Expect(bound).To(Equal("original"))
		Expect(func() { labels.WellKnown() }).To(Panic())
	})
	It("should fail the test on a registration error and still restore on cleanup", func() {
		outer, inner := &recordingTB{}, &recordingTB{}
		labelstest.Register(outer, labels.Option{Key: "example.com/outer", Binds: []*string{&bound}})
		labelstest.Register(inner, labels.Option{Key: "example.com/inner"}, labels.Option{Key: "example.com/inner"})
		Expect(inner.fatal).To(ContainSubstring("already registered"))

		inner.runCleanups()
		Expect(bound).To(Equal("example.com/outer"))
		Expect(labels.IsWellKnown("example.com/outer")).To(BeTrue())
	})
	It("should reset the registry and restore binds", func() {
		Expect(labels.Register(labels.Option{Key: "example.com/key", Binds: []*string{&bound}})).To(Succeed())
		labelstest.Reset()
		Expect(bound).To(Equal("original"))
		Expect(func() { labels.WellKnown() }).To(Panic())
		Expect(labels.Register(labels.Option{Key: "example.com/key"})).To(Succeed())
	})
})

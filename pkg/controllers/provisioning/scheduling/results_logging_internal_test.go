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

package scheduling

import (
	"context"
	"encoding/json"

	"github.com/go-logr/logr/funcr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

var _ = Describe("Results Logging", func() {
	It("should log pod counts under pod-count so pods is never logged as a number", func() {
		var lines []map[string]any
		logger := funcr.NewJSON(func(obj string) {
			line := map[string]any{}
			Expect(json.Unmarshal([]byte(obj), &line)).To(Succeed())
			lines = append(lines, line)
		}, funcr.Options{})
		ctx := log.IntoContext(context.Background(), logger)

		Results{NewNodeClaims: []*NodeClaim{{Pods: []*corev1.Pod{{}, {}}}}}.Record(ctx, nil, nil)

		Expect(lines).To(HaveLen(1))
		Expect(lines[0]).To(HaveKeyWithValue("msg", "computed new nodeclaim(s) to fit pod(s)"))
		Expect(lines[0]).To(HaveKeyWithValue("pod-count", BeNumerically("==", 2)))
		Expect(lines[0]).ToNot(HaveKey("pods"))
	})
})

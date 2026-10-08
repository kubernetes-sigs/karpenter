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

package reboot

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clock "k8s.io/utils/clock/testing"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

var _ = Describe("Reboot Issuance Timer", func() {
	It("should not leak the in-memory issuance timer for a NodeClaim deleted mid-reboot", func() {
		clk := clock.NewFakeClock(time.Now())
		c := &Controller{clock: clk, issuanceStarted: map[types.UID]time.Time{}}
		nodeClaim := &v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "rebooting", UID: "uid-1", DeletionTimestamp: &metav1.Time{Time: clk.Now()}, Finalizers: []string{v1.TerminationFinalizer}}}
		nodeClaim.StatusConditions().SetTrueWithReason(v1.ConditionTypeRebooting, v1.RebootReasonRequested, "rebooting")
		c.ensureIssuanceStarted(nodeClaim.UID)

		_, err := c.Reconcile(context.Background(), nodeClaim)
		Expect(err).ToNot(HaveOccurred())
		Expect(c.issuanceStarted).ToNot(HaveKey(nodeClaim.UID))
	})
})

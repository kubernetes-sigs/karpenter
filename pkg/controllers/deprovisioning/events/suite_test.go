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

package events_test

import (
	"sync"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/util/flowcontrol"

	deprovisioningevents "github.com/aws/karpenter-core/pkg/controllers/deprovisioning/events"
	"github.com/aws/karpenter-core/pkg/events"
	"github.com/aws/karpenter-core/pkg/test"
)

var eventRecorder events.Recorder
var internalRecorder *InternalRecorder

type InternalRecorder struct {
	mu    sync.RWMutex
	calls map[string]int
}

func NewInternalRecorder() *InternalRecorder {
	return &InternalRecorder{
		calls: map[string]int{},
	}
}

func (i *InternalRecorder) Event(_ runtime.Object, _, reason, _ string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.calls[reason]++
}

func (i *InternalRecorder) Eventf(object runtime.Object, eventtype, reason, messageFmt string, _ ...interface{}) {
	i.Event(object, eventtype, reason, messageFmt)
}

func (i *InternalRecorder) AnnotatedEventf(object runtime.Object, _ map[string]string, eventtype, reason, messageFmt string, _ ...interface{}) {
	i.Event(object, eventtype, reason, messageFmt)
}

func (i *InternalRecorder) Calls(reason string) int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.calls[reason]
}

func TestRecorder(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "EventRecorder")
}

var _ = BeforeEach(func() {
	internalRecorder = NewInternalRecorder()
	eventRecorder = events.NewRecorder(internalRecorder)
	events.PodNominationRateLimiter = flowcontrol.NewTokenBucketRateLimiter(5, 10)
})

var _ = Describe("Event Creation", func() {
	It("should create a BlockedDeprovisioning event", func() {
		eventRecorder.Publish(deprovisioningevents.BlockedDeprovisioning(NodeWithUID(), ""))
		Expect(internalRecorder.Calls(deprovisioningevents.BlockedDeprovisioning(NodeWithUID(), "").Reason)).To(Equal(1))
	})
	It("should create a TerminatingNode event", func() {
		eventRecorder.Publish(deprovisioningevents.TerminatingNode(NodeWithUID(), ""))
		Expect(internalRecorder.Calls(deprovisioningevents.TerminatingNode(NodeWithUID(), "").Reason)).To(Equal(1))
	})
	It("should create a LaunchingNode event", func() {
		eventRecorder.Publish(deprovisioningevents.LaunchingNode(NodeWithUID(), ""))
		Expect(internalRecorder.Calls(deprovisioningevents.LaunchingNode(NodeWithUID(), "").Reason)).To(Equal(1))
	})
	It("should create a WaitingOnReadiness event", func() {
		eventRecorder.Publish(deprovisioningevents.WaitingOnReadiness(NodeWithUID()))
		Expect(internalRecorder.Calls(deprovisioningevents.WaitingOnReadiness(NodeWithUID()).Reason)).To(Equal(1))
	})
	It("should create a WaitingOnDeletion event", func() {
		eventRecorder.Publish(deprovisioningevents.WaitingOnDeletion(NodeWithUID()))
		Expect(internalRecorder.Calls(deprovisioningevents.WaitingOnDeletion(NodeWithUID()).Reason)).To(Equal(1))
	})
	It("should create a UnconsolidatableReason event", func() {
		eventRecorder.Publish(deprovisioningevents.UnconsolidatableReason(NodeWithUID(), ""))
		Expect(internalRecorder.Calls(deprovisioningevents.UnconsolidatableReason(NodeWithUID(), "").Reason)).To(Equal(1))
	})
})

func NodeWithUID() *v1.Node {
	n := test.Node()
	n.UID = uuid.NewUUID()
	return n
}

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

package test

import (
	"regexp"
	"sync"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"

	"github.com/aws/karpenter-core/pkg/events"
)

// Binding is a potential binding that was reported through event recording.
type Binding struct {
	Pod  *v1.Pod
	Node *v1.Node
}

var _ events.Recorder = (*EventRecorder)(nil)

// EventRecorder is a mock event recorder that is used to facilitate testing.
type EventRecorder struct {
	mu       sync.RWMutex
	bindings []Binding
	calls    map[string]int
}

func NewEventRecorder() *EventRecorder {
	return &EventRecorder{
		calls: map[string]int{},
	}
}

func (e *EventRecorder) Publish(evt events.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()

	fakeNode := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "fake"}}
	fakePod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "fake"}}
	switch evt.Reason {
	case events.NominatePod(fakePod, fakeNode).Reason:
		var nodeName string
		r := regexp.MustCompile(`Pod should schedule on (?P<NodeName>.*)`)
		matches := r.FindStringSubmatch(evt.Message)
		if len(matches) == 0 {
			return
		}
		for i, name := range r.SubexpNames() {
			if name == "NodeName" {
				nodeName = matches[i]
				break
			}
		}

		pod := evt.InvolvedObject.(*v1.Pod)
		node := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}} // This is all we need for the binding
		e.bindings = append(e.bindings, Binding{pod, node})
	}
	e.calls[evt.Reason]++
}

func (e *EventRecorder) Calls(reason string) int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.calls[reason]
}

func (e *EventRecorder) Reset() {
	e.ResetBindings()
}

func (e *EventRecorder) ResetBindings() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.bindings = nil
}
func (e *EventRecorder) ForEachBinding(f func(pod *v1.Pod, node *v1.Node)) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, b := range e.bindings {
		f(b.Pod, b.Node)
	}
}

var _ record.EventRecorder = (*InternalRecorder)(nil)

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

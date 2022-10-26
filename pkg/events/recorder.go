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

package events

import (
	"fmt"
	"strings"
	"time"

	"github.com/patrickmn/go-cache"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/flowcontrol"
)

type Event struct {
	InvolvedObject runtime.Object
	Type           string
	Reason         string
	Message        string
	DedupeValues   []string
	RateLimiter    flowcontrol.RateLimiter
}

func (e Event) dedupeKey() string {
	return fmt.Sprintf("%s-%s",
		strings.ToLower(e.Reason),
		strings.Join(e.DedupeValues, "-"),
	)
}

type Recorder interface {
	Publish(Event)
}

type recorder struct {
	rec   record.EventRecorder
	cache *cache.Cache
}

func NewRecorder(r record.EventRecorder) Recorder {
	return &recorder{
		rec:   r,
		cache: cache.New(120*time.Second, 10*time.Second),
	}
}

// Publish creates a Kubernetes event using the passed event struct
func (r *recorder) Publish(evt Event) {
	// Dedupe same events that involve the same object and are close together
	if len(evt.DedupeValues) > 0 && !r.shouldCreateEvent(evt.dedupeKey()) {
		return
	}
	// If the event is rate-limited, then validate we should create the event
	if evt.RateLimiter != nil && !evt.RateLimiter.TryAccept() {
		return
	}
	r.rec.Event(evt.InvolvedObject, evt.Type, evt.Reason, evt.Message)
}

func (r *recorder) shouldCreateEvent(key string) bool {
	if _, exists := r.cache.Get(key); exists {
		return false
	}
	r.cache.SetDefault(key, nil)
	return true
}

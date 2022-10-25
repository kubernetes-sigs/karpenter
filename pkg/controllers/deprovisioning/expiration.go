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

package deprovisioning

import (
	"context"
	"sort"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/utils/clock"

	"knative.dev/pkg/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/provisioning/v1alpha5"
	"github.com/aws/karpenter-core/pkg/controllers/state"
)

// Expiration is a subreconciler that terminates nodes after a period of time.
type Expiration struct {
	kubeClient client.Client
	clock      clock.Clock
}

const expirationName = "expiration"

func (e *Expiration) shouldNotBeDeprovisioned(ctx context.Context, n state.Node, provisioner *v1alpha5.Provisioner, pods []*v1.Pod) bool {
	if provisioner == nil || provisioner.Spec.TTLSecondsUntilExpired == nil {
		return true
	}

	expirationTTL := time.Duration(ptr.Int64Value(provisioner.Spec.TTLSecondsUntilExpired)) * time.Second
	expirationTime := n.Node.CreationTimestamp.Add(expirationTTL)
	return e.clock.Now().After(expirationTime)
}

func (e *Expiration) sortCandidates(nodes []candidateNode) []candidateNode {
	sort.Slice(nodes, func(i int, j int) bool {
		return nodes[j].CreationTimestamp.After(nodes[i].CreationTimestamp.Time)
	})
	return nodes
}

func (e *Expiration) string() string {
	return expirationName
}

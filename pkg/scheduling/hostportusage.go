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

package scheduling

import (
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// HostPortUsage tracks HostPort usage within a node. On a node, each <hostIP, hostPort, protocol> used by pods bound
// to the node must be unique. We need to track this to keep an accurate concept of what pods can potentially schedule
// together.
type HostPortUsage struct {
	reserved map[types.NamespacedName][]HostPort
}

func NewHostPortUsage() *HostPortUsage {
	return &HostPortUsage{
		reserved: map[types.NamespacedName][]HostPort{},
	}
}

func (u *HostPortUsage) Conflicts(usedBy *v1.Pod, ports []HostPort) error {
	for _, newEntry := range ports {
		for podKey, entries := range u.reserved {
			for _, existing := range entries {
				if newEntry.Matches(existing) && podKey != client.ObjectKeyFromObject(usedBy) {
					return fmt.Errorf("%s conflicts with existing HostPort configuration %s", newEntry, existing)
				}
			}
		}
	}
	return nil
}

// Add adds a port to the HostPortUsage
func (u *HostPortUsage) Add(usedBy *v1.Pod, ports []HostPort) {
	u.reserved[client.ObjectKeyFromObject(usedBy)] = ports
}

// DeletePod deletes all host port usage from the HostPortUsage that were created by the pod with the given name.
func (u *HostPortUsage) DeletePod(key types.NamespacedName) {
	delete(u.reserved, key)
}

func (u *HostPortUsage) DeepCopy() *HostPortUsage {
	if u == nil {
		return nil
	}
	out := &HostPortUsage{}
	u.DeepCopyInto(out)
	return out
}

func (u *HostPortUsage) DeepCopyInto(out *HostPortUsage) {
	out.reserved = map[types.NamespacedName][]HostPort{}
	for k, v := range u.reserved {
		for _, p := range v {
			out.reserved[k] = append(out.reserved[k], p.DeepCopy())
		}
	}
}

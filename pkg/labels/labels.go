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

// Package labels is the process-wide registry of Karpenter's label policy: restricted domains, registered keys and
// the resources each key is denied on, known values, and key and value aliases.
//
// The policy is a list of plain-data Options. Each binary calls Register exactly once, as the first statement in
// main, with its complete list. Every read before Register panics. The package has no init().
package labels

import (
	"errors"
	"fmt"

	"github.com/awslabs/operatorpkg/wellknown"
	"k8s.io/apimachinery/pkg/util/sets"

	"sigs.k8s.io/karpenter/pkg/labels/internal/registry"
)

// Resource is a kind of object whose labels or requirements are validated against the registry.
type Resource = registry.Resource

const (
	NodePool    = registry.NodePool    // spec.template.metadata.labels and spec.template.spec.requirements
	NodeClaim   = registry.NodeClaim   // spec.requirements
	NodeOverlay = registry.NodeOverlay // spec.requirements
	Pod         = registry.Pod         // runtime-only: nodeSelector / required node affinity validation
)

// All returns every Resource, as a fresh slice.
func All() []Resource { return []Resource{NodePool, NodeClaim, NodeOverlay, Pod} }

// Owned returns the resources that reference a NodeClass, so exactly one binary acts on each object, as a fresh
// slice.
func Owned() []Resource { return []Resource{NodePool, NodeClaim} }

// Option is plain data so downstreams can inspect and edit it before registering.
// An option is either a domain option (Domain set) or a key option (Key set), never both.
type Option struct {
	// Domain options.

	// Domain is a restricted domain, a DNS-1123 subdomain: unregistered keys in it, or in a subdomain of it, are
	// denied.
	Domain string
	// AdmissionUnrestricted marks a co-installed provider's domain: it is restricted in Go on Owned resources only,
	// and generated CEL leaves it open.
	AdmissionUnrestricted bool

	// Key options.

	// Key is the registered key, in any domain.
	Key string
	// Binds are package-level vars that each receive the final Key at Register. Every var in Binds must carry a
	// "// +labels:bound" marker in its doc comment, which the labelsbind analyzer in hack/tools/labelsbind checks.
	// A var may appear in only one option.
	Binds []*string
	// DenyOn are the resources the key may not appear on; empty means it is allowed everywhere. A key denied on
	// every resource is restricted rather than well-known.
	DenyOn []Resource
	// KnownValues is an optional closed value set for In requirements on the key. It is Go-only and never emitted
	// to CEL.
	KnownValues []string
	// Aliases are keys normalized to Key. They inherit Key's DenyOn.
	Aliases []string
	// ValueAliases translate values on every requirement on Key, whether or not it arrived through an alias. Keys
	// and values must be valid label values.
	ValueAliases map[string]string
	// Doc is documentation only and never read at runtime. Doc.Name must be empty: the docs generator (follow-up)
	// derives it from Key.
	Doc wellknown.Label
}

// Register validates opts, writes each option's final Key into the vars in its Binds, and seals the registry. It
// must be called exactly once per process, as the first statement in main; a second call returns an error.
func Register(opts ...Option) error {
	if registry.Current != nil {
		return errors.New("labels.Register was already called")
	}
	r, err := registry.New(convert(opts))
	if err != nil {
		return fmt.Errorf("labels.Register: %w", err)
	}
	for _, o := range opts {
		for _, ptr := range o.Binds {
			registry.Bind(ptr, o.Key)
		}
	}
	registry.Current = r
	return nil
}

func convert(opts []Option) []registry.Option {
	out := make([]registry.Option, len(opts))
	for i, o := range opts {
		out[i] = registry.Option(o)
	}
	return out
}

func current() *registry.Registry {
	if registry.Current == nil {
		panic("labels: the label registry was read before labels.Register; call labels.Register as the first statement in main")
	}
	return registry.Current
}

// Validate returns an error if key may not be used on r. It fails when key, or the key it aliases, is registered
// and denied on r, or when key is unregistered and its domain is a restricted domain or a subdomain of one, unless
// that domain is AdmissionUnrestricted and r is not Owned. A key without a "/" has no domain.
func Validate(key string, r Resource) error {
	return current().Validate(key, r)
}

// IsWellKnown returns whether key is registered and not denied on every resource. Aliases are not well-known.
func IsWellKnown(key string) bool {
	return current().WellKnown().Has(key)
}

// WellKnown returns the registered keys that are not denied on every resource. The set is shared and sealed;
// callers must not mutate it.
func WellKnown() sets.Set[string] {
	return current().WellKnown()
}

// IsRestricted returns whether key is registered and denied on every resource. Restricted keys aren't well-known;
// Requirements.String and NodeClaim label stamping skip them.
func IsRestricted(key string) bool {
	return current().IsRestricted(key)
}

// KnownValues returns the closed value set registered for key, if any. The set is shared and sealed; callers must
// not mutate it.
func KnownValues(key string) (sets.Set[string], bool) {
	return current().KnownValues(key)
}

// Normalize rewrites an alias to its registered key, then translates values through the registered key's
// ValueAliases. values is never modified: it is returned as is unless a value is translated, in which case a new
// slice is returned.
func Normalize(key string, values ...string) (string, []string) {
	return current().Normalize(key, values)
}

// Domains returns the restricted domains, sorted, excluding AdmissionUnrestricted domains.
func Domains() []string {
	return current().Domains()
}

// Denied returns the registered keys denied on r, sorted. Aliases are not included.
func Denied(r Resource) []string {
	return current().Denied(r)
}

// Aliases returns a copy of the alias to registered key mapping.
func Aliases() map[string]string {
	return current().Aliases()
}

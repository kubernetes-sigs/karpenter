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

// Package registry builds and answers label policy for package labels, and holds the process-wide registry that
// labels.Register seals. crdgen builds registries from it without touching the process-wide one, and labelstest
// resets and restores the process-wide one, so labels exports no test hooks.
package registry

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/awslabs/operatorpkg/wellknown"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation"
)

// Resource is labels.Resource.
type Resource string

const (
	NodePool    Resource = "NodePool"
	NodeClaim   Resource = "NodeClaim"
	NodeOverlay Resource = "NodeOverlay"
	Pod         Resource = "Pod"
)

var (
	all   = sets.New(NodePool, NodeClaim, NodeOverlay, Pod)
	owned = sets.New(NodePool, NodeClaim)
)

// Option mirrors labels.Option field for field; labels converts between them with a type conversion, so the two
// can't drift apart. labels.Option documents the fields.
type Option struct {
	Domain                string
	AdmissionUnrestricted bool
	Key                   string
	Binds                 []*string
	DenyOn                []Resource
	KnownValues           []string
	Aliases               []string
	ValueAliases          map[string]string
	Doc                   wellknown.Label
}

// Registry is a validated label policy. It is never modified after New returns.
type Registry struct {
	keys      map[string]*key   // registered key -> policy
	aliases   map[string]string // alias -> registered key
	domains   []domain          // sorted by name
	wellKnown sets.Set[string]
	restrict  sets.Set[string] // registered keys denied on every resource
}

type key struct {
	denyOn       sets.Set[Resource]
	knownValues  sets.Set[string]
	valueAliases map[string]string
}

type domain struct {
	name                  string
	admissionUnrestricted bool
}

// builder accumulates a registry from options, rejecting the first invalid one.
type builder struct {
	registry *Registry
	names    map[string]string // registered key or alias -> "a key" or "an alias"
	domains  sets.Set[string]
	binds    sets.Set[*string]
}

// New validates opts and builds a registry from them. It writes no binds.
func New(opts []Option) (*Registry, error) {
	b := builder{
		registry: &Registry{keys: map[string]*key{}, aliases: map[string]string{}, wellKnown: sets.New[string](), restrict: sets.New[string]()},
		names:    map[string]string{},
		domains:  sets.New[string](),
		binds:    sets.New[*string](),
	}
	for i, o := range opts {
		var err error
		switch {
		case o.Domain != "" && o.Key != "":
			err = fmt.Errorf("option %d sets both Domain %q and Key %q", i, o.Domain, o.Key)
		case o.Domain == "" && o.Key == "":
			err = fmt.Errorf("option %d sets neither Domain nor Key", i)
		case o.Domain != "":
			err = b.addDomain(o)
		default:
			err = b.addKey(o)
		}
		if err != nil {
			return nil, err
		}
	}
	slices.SortFunc(b.registry.domains, func(x, y domain) int { return strings.Compare(x.name, y.name) })
	for _, d := range b.registry.domains {
		for _, parent := range b.registry.domains {
			if strings.HasSuffix(d.name, "."+parent.name) {
				return nil, fmt.Errorf("domain %q is a subdomain of registered domain %q", d.name, parent.name)
			}
		}
	}
	return b.registry, nil
}

// docSet returns whether doc sets any field, treating empty slices as unset like the other option fields.
func docSet(doc wellknown.Label) bool {
	if len(doc.UsedOn) == 0 {
		doc.UsedOn = nil
	}
	if len(doc.Values) == 0 {
		doc.Values = nil
	}
	return !reflect.ValueOf(doc).IsZero()
}

func (b *builder) addDomain(o Option) error {
	if len(o.Binds) > 0 || len(o.DenyOn) > 0 || len(o.KnownValues) > 0 || len(o.Aliases) > 0 || len(o.ValueAliases) > 0 || docSet(o.Doc) {
		return fmt.Errorf("domain option %q sets a key option field (Binds, DenyOn, KnownValues, Aliases, ValueAliases or Doc)", o.Domain)
	}
	if errs := validation.IsDNS1123Subdomain(o.Domain); len(errs) > 0 {
		return fmt.Errorf("domain option %q is not a DNS-1123 subdomain: %s", o.Domain, strings.Join(errs, "; "))
	}
	if b.domains.Has(o.Domain) {
		return fmt.Errorf("domain %q is registered more than once", o.Domain)
	}
	b.domains.Insert(o.Domain)
	b.registry.domains = append(b.registry.domains, domain{name: o.Domain, admissionUnrestricted: o.AdmissionUnrestricted})
	return nil
}

func (b *builder) addKey(o Option) error {
	if o.AdmissionUnrestricted {
		return fmt.Errorf("key option %q sets AdmissionUnrestricted, which is only valid on domain options", o.Key)
	}
	if o.Doc.Name != "" {
		return fmt.Errorf("key option %q sets Doc.Name, which must be empty", o.Key)
	}
	if err := b.addNames(o); err != nil {
		return err
	}
	if err := validateValues(o); err != nil {
		return err
	}
	for _, r := range o.DenyOn {
		if !all.Has(r) {
			return fmt.Errorf("key option %q: DenyOn has unknown resource %q", o.Key, r)
		}
	}
	for _, ptr := range o.Binds {
		if ptr == nil {
			return fmt.Errorf("key option %q: Binds has a nil pointer", o.Key)
		}
		if b.binds.Has(ptr) {
			return fmt.Errorf("key option %q: a var in Binds is already bound by another option", o.Key)
		}
		b.binds.Insert(ptr)
	}
	k := &key{denyOn: sets.New(o.DenyOn...), valueAliases: maps.Clone(o.ValueAliases)}
	if len(o.KnownValues) > 0 {
		k.knownValues = sets.New(o.KnownValues...)
	}
	b.registry.keys[o.Key] = k
	for _, alias := range o.Aliases {
		b.registry.aliases[alias] = o.Key
	}
	if k.denyOn.IsSuperset(all) {
		b.registry.restrict.Insert(o.Key)
	} else {
		b.registry.wellKnown.Insert(o.Key)
	}
	return nil
}

// addNames claims the option's key and aliases; keys and aliases share one namespace.
func (b *builder) addNames(o Option) error {
	for i, name := range append([]string{o.Key}, o.Aliases...) {
		if errs := validation.IsQualifiedName(name); len(errs) > 0 {
			return fmt.Errorf("key option %q: %q is not a qualified name: %s", o.Key, name, strings.Join(errs, "; "))
		}
		if kind, ok := b.names[name]; ok {
			return fmt.Errorf("key option %q: %q is already registered as %s", o.Key, name, kind)
		}
		b.names[name] = "an alias"
		if i == 0 {
			b.names[name] = "a key"
		}
	}
	return nil
}

// validateValues checks that known values and both sides of every value alias are valid label values.
func validateValues(o Option) error {
	check := func(what, v string) error {
		if errs := validation.IsValidLabelValue(v); len(errs) > 0 {
			return fmt.Errorf("key option %q: %s %q is not a valid label value: %s", o.Key, what, v, strings.Join(errs, "; "))
		}
		return nil
	}
	for _, v := range o.KnownValues {
		if err := check("known value", v); err != nil {
			return err
		}
	}
	for _, from := range slices.Sorted(maps.Keys(o.ValueAliases)) {
		if err := check("value alias", from); err != nil {
			return err
		}
		if err := check("value alias target", o.ValueAliases[from]); err != nil {
			return err
		}
	}
	return nil
}

// Validate implements labels.Validate.
func (r *Registry) Validate(k string, res Resource) error {
	registered := k
	if canonical, ok := r.aliases[k]; ok {
		registered = canonical
	}
	if p, ok := r.keys[registered]; ok {
		if p.denyOn.Has(res) {
			return fmt.Errorf("label %q is restricted on %s", k, res)
		}
		return nil
	}
	name, _, ok := strings.Cut(k, "/")
	if !ok {
		return nil
	}
	for _, d := range r.domains {
		if name != d.name && !strings.HasSuffix(name, "."+d.name) {
			continue
		}
		if d.admissionUnrestricted && !owned.Has(res) {
			continue
		}
		return fmt.Errorf("label domain %q is restricted", d.name)
	}
	return nil
}

// WellKnown implements labels.WellKnown.
func (r *Registry) WellKnown() sets.Set[string] { return r.wellKnown }

// IsRestricted implements labels.IsRestricted.
func (r *Registry) IsRestricted(k string) bool { return r.restrict.Has(k) }

// KnownValues implements labels.KnownValues.
func (r *Registry) KnownValues(k string) (sets.Set[string], bool) {
	p, ok := r.keys[k]
	if !ok || p.knownValues == nil {
		return nil, false
	}
	return p.knownValues, true
}

// Normalize implements labels.Normalize.
func (r *Registry) Normalize(k string, values []string) (string, []string) {
	if canonical, ok := r.aliases[k]; ok {
		k = canonical
	}
	p, ok := r.keys[k]
	if !ok || len(p.valueAliases) == 0 {
		return k, values
	}
	var normalized []string
	for i, v := range values {
		alias, ok := p.valueAliases[v]
		if !ok {
			continue
		}
		if normalized == nil {
			normalized = slices.Clone(values)
		}
		normalized[i] = alias
	}
	if normalized == nil {
		return k, values
	}
	return k, normalized
}

// Domains implements labels.Domains.
func (r *Registry) Domains() []string {
	var out []string
	for _, d := range r.domains {
		if !d.admissionUnrestricted {
			out = append(out, d.name)
		}
	}
	return out
}

// Denied implements labels.Denied.
func (r *Registry) Denied(res Resource) []string {
	var out []string
	for name, p := range r.keys {
		if p.denyOn.Has(res) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// Aliases implements labels.Aliases.
func (r *Registry) Aliases() map[string]string { return maps.Clone(r.aliases) }

// Names returns every registered key and alias.
func (r *Registry) Names() sets.Set[string] {
	names := sets.KeySet(r.keys)
	for alias := range r.aliases {
		names.Insert(alias)
	}
	return names
}

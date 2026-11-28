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

// Package crdgen generates the label restriction CEL rules in Karpenter's NodePool, NodeClaim and NodeOverlay CRDs
// from label option lists, so admission and Go validation derive from the same policy.
//
// crdgen owns every rule whose message has one of these formats, whoever wrote it:
//
//	label domain "<domain>" is restricted
//	label "<key>" is restricted on <resource>
//	label "<key>" is restricted
//
// The last is the format of the hand-written rules crdgen replaces; it never emits it. Every run first strips the
// rules it owns from the input CRDs, then re-emits its own, so it is idempotent and converges over CRDs written by
// older scripts or by a run with other options. All other schema content is left as is.
package crdgen

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
	"k8s.io/apimachinery/pkg/util/sets"

	"sigs.k8s.io/karpenter/pkg/apis"
	"sigs.k8s.io/karpenter/pkg/labels"
	"sigs.k8s.io/karpenter/pkg/labels/internal/registry"
)

var ownedMessage = regexp.MustCompile(`^label (domain "[^"]+" is restricted|"[^"]+" is restricted( on [A-Za-z]+)?)$`)

// verdicts answers labels.Validate for one input's options, without touching the process-wide registry.
type verdicts struct {
	registry *registry.Registry
	names    sets.Set[string] // registered keys and aliases
}

func newVerdicts(opts []labels.Option) (verdicts, error) {
	converted := make([]registry.Option, len(opts))
	for i, o := range opts {
		converted[i] = registry.Option(o)
	}
	r, err := registry.New(converted)
	if err != nil {
		return verdicts{}, err
	}
	return verdicts{registry: r, names: r.Names()}, nil
}

// allows returns whether this input admits name on r. A registered name is admitted unless it is denied on r. An
// unregistered name inside a restricted domain of any input is left to the inputs that register it; outside every
// restricted domain it is admitted only if this input's Go verdict passes, so an admission-unrestricted domain's open
// view can't re-admit a key that another input registers and denies there.
func (v verdicts) allows(name string, r labels.Resource, inRestrictedDomain bool) bool {
	if v.names.Has(name) {
		return v.registry.Validate(name, r) == nil
	}
	return !inRestrictedDomain && v.registry.Validate(name, r) == nil
}

type rule struct {
	message string
	// cond renders the rule for one label key held by the CEL variable v.
	cond func(v string) string
}

// rules returns each resource's generated rules, domain rules first, each group sorted. A domain's allowlist holds
// every key and alias in it that some input registers, whatever their DenyOn. A registered key or alias that no input
// allows on a resource gets its own rule there, so CEL rejects it with the same message as labels.Validate.
func rules(inputs []verdicts) map[labels.Resource][]rule {
	domains := sets.New[string]()
	names := sets.New[string]()
	for _, v := range inputs {
		domains.Insert(v.registry.Domains()...)
		names = names.Union(v.names)
	}
	out := map[labels.Resource][]rule{}
	for r := range targets {
		for _, d := range sets.List(domains) {
			out[r] = append(out[r], domainRule(d, slices.DeleteFunc(sets.List(names), func(n string) bool { return !inDomain(n, d) })))
		}
		for _, name := range sets.List(names) {
			restricted := slices.ContainsFunc(sets.List(domains), func(d string) bool { return inDomain(name, d) })
			if !slices.ContainsFunc(inputs, func(v verdicts) bool { return v.allows(name, r, restricted) }) {
				out[r] = append(out[r], keyRule(name, r))
			}
		}
	}
	return out
}

func inDomain(key, domain string) bool {
	name, _, ok := strings.Cut(key, "/")
	return ok && (name == domain || strings.HasSuffix(name, "."+domain))
}

func domainRule(domain string, allowed []string) rule {
	quoted := make([]string, len(allowed))
	for i, a := range allowed {
		quoted[i] = strconv.Quote(a)
	}
	return rule{
		message: fmt.Sprintf("label domain %q is restricted", domain),
		cond: func(v string) string {
			return fmt.Sprintf(`!%[1]s.contains("/") || %[1]s in [%[2]s] || !(%[1]s.find("^([^/]+)") == %[3]s || %[1]s.find("^([^/]+)").endsWith(%[4]s))`,
				v, strings.Join(quoted, ", "), strconv.Quote(domain), strconv.Quote("."+domain))
		},
	}
}

func keyRule(key string, r labels.Resource) rule {
	return rule{
		message: fmt.Sprintf("label %q is restricted on %s", key, r),
		cond:    func(v string) string { return fmt.Sprintf("%s != %s", v, strconv.Quote(key)) },
	}
}

// target is a schema field crdgen writes rules to. A labels target is a map whose keys are validated with
// self.all; otherwise the field is a single key.
type target struct {
	path   []string
	labels bool
}

var targets = map[labels.Resource][]target{
	labels.NodePool: {
		{path: []string{"spec", "template", "metadata", "labels"}, labels: true},
		{path: []string{"spec", "template", "spec", "requirements", "[]", "key"}},
	},
	labels.NodeClaim:   {{path: []string{"spec", "requirements", "[]", "key"}}},
	labels.NodeOverlay: {{path: []string{"spec", "requirements", "[]", "key"}}},
}

// generate rewrites the label rules of the karpenter.sh NodePool, NodeClaim and NodeOverlay CRDs in base from the
// union of policy and coInstalled, and writes them to out, which may be base. Other files are skipped.
func generate(base, out string, policy []labels.Option, coInstalled ...[]labels.Option) error {
	var inputs []verdicts
	for i, opts := range append([][]labels.Option{policy}, coInstalled...) {
		v, err := newVerdicts(opts)
		if err != nil {
			return fmt.Errorf("input %d: %w", i, err)
		}
		inputs = append(inputs, v)
	}
	generated := rules(inputs)

	entries, err := os.ReadDir(base)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(base, e.Name()))
		if err != nil {
			return err
		}
		updated, ok, err := rewrite(data, generated)
		if err != nil {
			return fmt.Errorf("%s: %w", e.Name(), err)
		}
		if !ok {
			continue
		}
		if err := os.WriteFile(filepath.Join(out, e.Name()), updated, 0o644); err != nil { //nolint:gosec // CRDs are checked in and world-readable
			return err
		}
	}
	return nil
}

// rewrite replaces the generated rules in a CRD. It returns false if data isn't a CRD crdgen writes to.
func rewrite(data []byte, generated map[labels.Resource][]rule) ([]byte, bool, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, false, err
	}
	if len(doc.Content) == 0 {
		return nil, false, nil
	}
	crd := doc.Content[0]
	r := labels.Resource(value(lookup(crd, "spec", "names", "kind")))
	if value(lookup(crd, "kind")) != "CustomResourceDefinition" || value(lookup(crd, "spec", "group")) != apis.Group || targets[r] == nil {
		return nil, false, nil
	}
	versions := lookup(crd, "spec", "versions")
	if versions == nil || versions.Kind != yaml.SequenceNode {
		return nil, false, errors.New("spec.versions not found")
	}
	for _, version := range versions.Content {
		schema := lookup(version, "schema", "openAPIV3Schema")
		for _, t := range targets[r] {
			node := schemaField(schema, t.path)
			if node == nil {
				return nil, false, fmt.Errorf("version %s: schema field %s not found", value(lookup(version, "name")), strings.Join(t.path, "."))
			}
			setRules(node, generated[r], t.labels)
		}
	}
	var buf bytes.Buffer
	if bytes.HasPrefix(data, []byte("---\n")) {
		buf.WriteString("---\n")
	}
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, false, err
	}
	if err := enc.Close(); err != nil {
		return nil, false, err
	}
	return buf.Bytes(), true, nil
}

// setRules strips the owned rules from node's x-kubernetes-validations and appends rs.
func setRules(node *yaml.Node, rs []rule, labelsField bool) {
	var kept []*yaml.Node
	i := -1
	for j := 0; j+1 < len(node.Content); j += 2 {
		if node.Content[j].Value == "x-kubernetes-validations" {
			i = j
		}
	}
	if i >= 0 {
		for _, r := range node.Content[i+1].Content {
			if !ownedMessage.MatchString(value(lookup(r, "message"))) {
				kept = append(kept, r)
			}
		}
	}
	for _, r := range rs {
		cond := r.cond("self")
		if labelsField {
			cond = fmt.Sprintf("self.all(x, %s)", r.cond("x"))
		}
		kept = append(kept, &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{str("message"), str(r.message), str("rule"), str(cond)}})
	}
	switch {
	case i >= 0 && len(kept) == 0:
		node.Content = slices.Delete(node.Content, i, i+2)
	case i >= 0:
		node.Content[i+1].Content = kept
	case len(kept) > 0:
		node.Content = append(node.Content, str("x-kubernetes-validations"), &yaml.Node{Kind: yaml.SequenceNode, Content: kept})
	}
}

// schemaField walks an OpenAPI schema along path, where "[]" steps into array items and any other element is a
// property name.
func schemaField(schema *yaml.Node, path []string) *yaml.Node {
	node := schema
	for _, p := range path {
		if p == "[]" {
			node = lookup(node, "items")
		} else {
			node = lookup(node, "properties", p)
		}
	}
	return node
}

// lookup follows keys through nested mappings, returning nil if any is missing.
func lookup(node *yaml.Node, keys ...string) *yaml.Node {
	for _, k := range keys {
		if node == nil || node.Kind != yaml.MappingNode {
			return nil
		}
		var next *yaml.Node
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Value == k {
				next = node.Content[i+1]
				break
			}
		}
		node = next
	}
	return node
}

func value(node *yaml.Node) string {
	if node == nil {
		return ""
	}
	return node.Value
}

func str(s string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s} }

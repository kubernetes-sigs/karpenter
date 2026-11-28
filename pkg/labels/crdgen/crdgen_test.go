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

package crdgen

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"

	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/labels"
	"sigs.k8s.io/karpenter/pkg/labels/labelstest"
)

const (
	restricted  = "restricted.example.com"
	coInstalled = "coinstalled.example.com"
)

// shapes exercises every rule shape from a single list.
func shapes() []labels.Option {
	return []labels.Option{
		{Domain: restricted},
		{Domain: coInstalled, AdmissionUnrestricted: true},
		{Key: restricted + "/allowed", Aliases: []string{restricted + "/alias"}, KnownValues: []string{"never-emitted"}},
		{Key: restricted + "/not-on-nodepool", DenyOn: []labels.Resource{labels.NodePool}},
		{Key: "example.com/not-on-overlay", Aliases: []string{"example.com/overlay-alias"}, DenyOn: []labels.Resource{labels.NodeOverlay}},
		{Key: "example.com/restricted", DenyOn: labels.All()},
		{Key: "example.com/not-on-pod", DenyOn: []labels.Resource{labels.Pod}},
		{Key: "example.com/allowed"},
	}
}

// The union inputs model EKS and OSS sharing the CRDs: each restricts its own domain and leaves the other's
// admission-unrestricted.
func unionPolicy() []labels.Option {
	return []labels.Option{
		{Domain: "eks.example.com"},
		{Domain: "oss.example.com", AdmissionUnrestricted: true},
		{Key: "eks.example.com/nodeclass", DenyOn: []labels.Resource{labels.NodePool}},
		{Key: "eks.example.com/zone-id"},
		{Key: "example.com/denied-by-both", DenyOn: []labels.Resource{labels.NodeOverlay}},
		{Key: "example.com/denied-by-policy-only", DenyOn: []labels.Resource{labels.NodeOverlay}},
		{Key: "example.com/registered-by-policy-only", DenyOn: []labels.Resource{labels.NodeOverlay}},
	}
}

func unionCoInstalled() []labels.Option {
	return []labels.Option{
		{Domain: "oss.example.com"},
		{Domain: "eks.example.com", AdmissionUnrestricted: true},
		{Key: "oss.example.com/nodeclass", DenyOn: []labels.Resource{labels.NodePool}},
		{Key: "oss.example.com/zone-id"},
		{Key: "example.com/denied-by-both", DenyOn: []labels.Resource{labels.NodeOverlay}},
		{Key: "example.com/denied-by-policy-only"},
	}
}

// expectGolden runs crdgen over testdata/base and compares the output with testdata/<name>.
func expectGolden(name string, policy []labels.Option, coInstalled ...[]labels.Option) string {
	GinkgoHelper()
	out := GinkgoT().TempDir()
	Expect(generate(filepath.Join("testdata", "base"), out, policy, coInstalled...)).To(Succeed())
	golden := filepath.Join("testdata", name)
	if *update {
		Expect(os.RemoveAll(golden)).To(Succeed())
		Expect(os.CopyFS(golden, os.DirFS(out))).To(Succeed())
	}
	Expect(fileNames(out)).To(Equal(fileNames(golden)))
	for _, f := range fileNames(golden) {
		Expect(readFile(filepath.Join(out, f))).To(Equal(readFile(filepath.Join(golden, f))), f)
	}
	return out
}

func fileNames(dir string) []string {
	GinkgoHelper()
	entries, err := os.ReadDir(dir)
	Expect(err).ToNot(HaveOccurred())
	return lo.Map(entries, func(e os.DirEntry, _ int) string { return e.Name() })
}

func readFile(path string) string {
	GinkgoHelper()
	data, err := os.ReadFile(path)
	Expect(err).ToNot(HaveOccurred())
	return string(data)
}

func readCRD(path string) *apiextensionsv1.CustomResourceDefinition {
	GinkgoHelper()
	crd := &apiextensionsv1.CustomResourceDefinition{}
	Expect(yaml.Unmarshal([]byte(readFile(path)), crd)).To(Succeed())
	return crd
}

// labelRestriction matches the message of every label restriction rule, generated or hand-written, independently of
// the formats crdgen claims to own.
var labelRestriction = regexp.MustCompile(`^label (domain )?"[^"]+" is restricted`)

// restrictions evaluates the CRD's rules for key, set as a requirement key (and, on NodePools, also as a template
// label), and returns the messages of the failed label restriction rules. Other rules are ignored.
func restrictions(crd *apiextensionsv1.CustomResourceDefinition, key string) []string {
	GinkgoHelper()
	internal := &apiextensions.JSONSchemaProps{}
	Expect(apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(crd.Spec.Versions[0].Schema.OpenAPIV3Schema, internal, nil)).To(Succeed())
	structural, err := schema.NewStructural(internal)
	Expect(err).ToNot(HaveOccurred())
	requirements := []any{map[string]any{"key": key, "operator": "Exists"}}
	spec := map[string]any{"requirements": requirements}
	if crd.Spec.Names.Kind == string(labels.NodePool) {
		spec = map[string]any{"template": map[string]any{
			"metadata": map[string]any{"labels": map[string]any{key: "value"}},
			"spec":     map[string]any{"requirements": requirements},
		}}
	}
	errs, _ := cel.NewValidator(structural, true, celconfig.PerCallLimit).Validate(context.Background(), nil, structural, map[string]any{"spec": spec}, nil, celconfig.RuntimeCELCostBudget)
	return lo.FilterMap(errs, func(e *field.Error, _ int) (string, bool) { return e.Detail, labelRestriction.MatchString(e.Detail) })
}

func admit(crd *apiextensionsv1.CustomResourceDefinition, key string) bool {
	GinkgoHelper()
	return len(restrictions(crd, key)) == 0
}

// expectParity checks that the generated CRDs in out admit a probe on a resource exactly when labels.Validate allows
// it under opts, except that admission-unrestricted domains are open, and reject it with labels.Validate's message.
func expectParity(out string, opts []labels.Option, extraProbes ...string) {
	GinkgoHelper()
	labelstest.Register(GinkgoTB(), opts...)
	probes := sets.New(extraProbes...)
	probes.Insert(labels.WellKnown().UnsortedList()...)
	probes.Insert(lo.Keys(labels.Aliases())...)
	for _, r := range labels.All() {
		probes.Insert(labels.Denied(r)...)
		for _, d := range labels.Domains() {
			probes.Insert(d+"/unregistered", "sub."+d+"/unregistered", "not-"+d+"/unregistered", d)
		}
	}
	for _, f := range fileNames(out) {
		crd := readCRD(filepath.Join(out, f))
		r := labels.Resource(crd.Spec.Names.Kind)
		for key := range probes {
			err := labels.Validate(key, r)
			if err == nil || slices.ContainsFunc(opts, func(o labels.Option) bool { return o.AdmissionUnrestricted && inDomain(key, o.Domain) }) {
				Expect(restrictions(crd, key)).To(BeEmpty(), "%s on %s", key, r)
				continue
			}
			messages := restrictions(crd, key)
			Expect(messages).ToNot(BeEmpty(), "%s on %s", key, r)
			Expect(messages).To(HaveEach(err.Error()), "%s on %s", key, r)
		}
	}
}

var _ = Describe("crdgen", func() {
	BeforeEach(func() {
		labelstest.Reset()
	})

	It("should admit exactly the keys labels.Validate allows, with its messages, treating admission-unrestricted domains as open", func() {
		out := expectGolden("shapes", shapes())
		expectParity(out, shapes(), coInstalled+"/unregistered", "example.com/unregistered")
	})
	It("should admit exactly the keys labels.Validate allows on core's CRDs, with its messages", func() {
		out := GinkgoT().TempDir()
		Expect(generate(filepath.Join("..", "..", "apis", "crds"), out, karpv1.LabelOptions())).To(Succeed())
		Expect(fileNames(out)).To(ConsistOf("karpenter.sh_nodepools.yaml", "karpenter.sh_nodeclaims.yaml", "karpenter.sh_nodeoverlays.yaml"))
		expectParity(out, karpv1.LabelOptions(), "example.com/unregistered")
	})
	It("should admit a key on a resource if any co-installed policy allows it", func() {
		out := expectGolden("union", unionPolicy(), unionCoInstalled())
		overlay := readCRD(filepath.Join(out, "karpenter.sh_nodeoverlays.yaml"))
		pool := readCRD(filepath.Join(out, "karpenter.sh_nodepools.yaml"))
		Expect(admit(overlay, "example.com/denied-by-both")).To(BeFalse())
		Expect(admit(overlay, "example.com/denied-by-policy-only")).To(BeTrue())
		Expect(admit(overlay, "example.com/registered-by-policy-only")).To(BeTrue())
		for _, key := range []string{"eks.example.com/zone-id", "oss.example.com/zone-id"} {
			Expect(admit(pool, key)).To(BeTrue(), key)
		}
		for _, key := range []string{"eks.example.com/unregistered", "oss.example.com/unregistered", "eks.example.com/nodeclass", "oss.example.com/nodeclass"} {
			Expect(admit(pool, key)).To(BeFalse(), key)
		}
	})

	It("should not let an admission-unrestricted domain re-admit a key another list denies", func() {
		out := GinkgoT().TempDir()
		Expect(generate(filepath.Join("testdata", "base"), out,
			[]labels.Option{{Domain: "shared.example.com", AdmissionUnrestricted: true}},
			[]labels.Option{{Key: "shared.example.com/key", DenyOn: []labels.Resource{labels.NodePool}}},
		)).To(Succeed())
		Expect(admit(readCRD(filepath.Join(out, "karpenter.sh_nodepools.yaml")), "shared.example.com/key")).To(BeFalse())
		Expect(admit(readCRD(filepath.Join(out, "karpenter.sh_nodeclaims.yaml")), "shared.example.com/key")).To(BeTrue())
	})
	It("should be idempotent over its own output", func() {
		once := expectGolden("union", unionPolicy(), unionCoInstalled())
		twice := GinkgoT().TempDir()
		Expect(generate(once, twice, unionPolicy(), unionCoInstalled())).To(Succeed())
		for _, f := range fileNames(once) {
			Expect(readFile(filepath.Join(twice, f))).To(Equal(readFile(filepath.Join(once, f))), f)
		}
	})
	It("should replace another policy's generated rules instead of keeping them", func() {
		expected := GinkgoT().TempDir()
		Expect(generate(filepath.Join("testdata", "base"), expected, karpv1.LabelOptions())).To(Succeed())
		out := GinkgoT().TempDir()
		Expect(generate(expectGolden("shapes", shapes()), out, karpv1.LabelOptions())).To(Succeed())
		Expect(fileNames(out)).To(Equal(fileNames(expected)))
		for _, f := range fileNames(expected) {
			Expect(readFile(filepath.Join(out, f))).To(Equal(readFile(filepath.Join(expected, f))), f)
		}
	})
	It("core's input NodePool CRD bounds labels with maxProperties, which bounds the cost of the labels rule", func() {
		pool := readCRD(filepath.Join("..", "..", "apis", "crds", "karpenter.sh_nodepools.yaml"))
		labelsSchema := pool.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["template"].Properties["metadata"].Properties["labels"]
		Expect(labelsSchema.MaxProperties).ToNot(BeNil())
	})

	It("should reject an input that labels.Register rejects", func() {
		Expect(generate(filepath.Join("testdata", "base"), GinkgoT().TempDir(), shapes(), []labels.Option{{}})).To(MatchError(ContainSubstring("input 1: option 0 sets neither Domain nor Key")))
	})
	It("should fail when a CRD lacks a field it writes rules to", func() {
		base := GinkgoT().TempDir()
		crd := readCRD(filepath.Join("testdata", "base", "karpenter.sh_nodeclaims.yaml"))
		delete(crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties, "requirements")
		Expect(os.WriteFile(filepath.Join(base, "karpenter.sh_nodeclaims.yaml"), lo.Must(yaml.Marshal(crd)), 0o600)).To(Succeed())
		Expect(generate(base, GinkgoT().TempDir(), shapes())).To(MatchError(ContainSubstring("spec.requirements.[].key not found")))
	})
})

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

package labels_test

import (
	"github.com/awslabs/operatorpkg/docs"
	"github.com/awslabs/operatorpkg/wellknown"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"

	"sigs.k8s.io/karpenter/pkg/labels"
	"sigs.k8s.io/karpenter/pkg/labels/labelstest"
)

// Bound vars must be package-level and marked, like every var in Binds.
var (
	// +labels:bound
	bound string
	// +labels:bound
	alsoBound string

	// nilBind is built outside the Binds field because the analyzer only admits &X there.
	nilBind = []*string{nil}
)

var _ = Describe("Register", func() {
	BeforeEach(func() {
		labelstest.Reset()
		DeferCleanup(labelstest.Reset)
		bound, alsoBound = "unset", "unset"
	})

	It("should write the final key into every bound var", func() {
		Expect(labels.Register(labels.Option{Key: "example.com/final", Binds: []*string{&bound, &alsoBound}})).To(Succeed())
		Expect(bound).To(Equal("example.com/final"))
		Expect(alsoBound).To(Equal("example.com/final"))
	})
	It("should return an error on a second call and leave the first registration in place", func() {
		Expect(labels.Register(labels.Option{Key: "example.com/first", Binds: []*string{&bound}})).To(Succeed())
		Expect(labels.Register(labels.Option{Key: "example.com/second", Binds: []*string{&bound}})).To(MatchError(ContainSubstring("already called")))
		Expect(bound).To(Equal("example.com/first"))
		Expect(labels.IsWellKnown("example.com/first")).To(BeTrue())
		Expect(labels.IsWellKnown("example.com/second")).To(BeFalse())
	})
	It("should not copy later edits of an option into the sealed registry", func() {
		valueAliases := map[string]string{"a": "b"}
		Expect(labels.Register(labels.Option{Key: "example.com/key", ValueAliases: valueAliases})).To(Succeed())
		valueAliases["a"] = "c"
		_, values := labels.Normalize("example.com/key", "a")
		Expect(values).To(Equal([]string{"b"}))
	})

	DescribeTable("should reject an invalid option without binding or sealing",
		func(invalid labels.Option, message string) {
			valid := labels.Option{Key: "example.com/valid", Binds: []*string{&bound}}
			Expect(labels.Register(valid, invalid)).To(MatchError(ContainSubstring(message)))
			Expect(bound).To(Equal("unset"))
			// The failed call didn't seal the registry, so a valid list still registers.
			Expect(labels.Register(valid)).To(Succeed())
			Expect(bound).To(Equal("example.com/valid"))
		},
		Entry("both Domain and Key", labels.Option{Domain: "example.org", Key: "example.org/key"}, "sets both Domain"),
		Entry("neither Domain nor Key", labels.Option{DenyOn: []labels.Resource{labels.NodePool}}, "sets neither Domain nor Key"),
		Entry("AdmissionUnrestricted on a key option", labels.Option{Key: "example.org/key", AdmissionUnrestricted: true}, "AdmissionUnrestricted"),
		Entry("KnownValues on a domain option", labels.Option{Domain: "example.org", KnownValues: []string{"a"}}, "sets a key option field"),
		Entry("Doc on a domain option", labels.Option{Domain: "example.org", Doc: wellknown.Label{Help: "help"}}, "sets a key option field"),
		Entry("a key that isn't a qualified name", labels.Option{Key: "example.org/not a name"}, "not a qualified name"),
		Entry("an alias that isn't a qualified name", labels.Option{Key: "example.org/key", Aliases: []string{"-alias"}}, "not a qualified name"),
		Entry("a domain that isn't a DNS-1123 subdomain", labels.Option{Domain: "Example.org"}, "not a DNS-1123 subdomain"),
		Entry("a domain that is a key", labels.Option{Domain: "example.org/key"}, "not a DNS-1123 subdomain"),
		Entry("a value alias that isn't a label value", labels.Option{Key: "example.org/key", ValueAliases: map[string]string{"not a value": "a"}}, `value alias "not a value" is not a valid label value`),
		Entry("a value alias target that isn't a label value", labels.Option{Key: "example.org/key", ValueAliases: map[string]string{"a": "not a value"}}, `value alias target "not a value" is not a valid label value`),
		Entry("a known value that isn't a label value", labels.Option{Key: "example.org/key", KnownValues: []string{"a", "not a value"}}, "not a valid label value"),
		Entry("a duplicate key", labels.Option{Key: "example.com/valid"}, `"example.com/valid" is already registered as a key`),
		Entry("an alias that is a key", labels.Option{Key: "example.org/key", Aliases: []string{"example.com/valid"}}, `"example.com/valid" is already registered as a key`),
		Entry("a key that is the option's own alias", labels.Option{Key: "example.org/key", Aliases: []string{"example.org/key"}}, `"example.org/key" is already registered as a key`),
		Entry("a duplicate alias within an option", labels.Option{Key: "example.org/key", Aliases: []string{"example.org/alias", "example.org/alias"}}, `"example.org/alias" is already registered as an alias`),
		Entry("Doc.Name set", labels.Option{Key: "example.org/key", Doc: wellknown.Label{Name: "example.org/key"}}, "Doc.Name"),
		Entry("an unknown DenyOn resource", labels.Option{Key: "example.org/key", DenyOn: []labels.Resource{"Node"}}, "unknown resource"),
		Entry("a nil bind", labels.Option{Key: "example.org/key", Binds: nilBind}, "nil pointer"),
	)
	It("should treat empty fields on a domain option as unset", func() {
		Expect(labels.Register(labels.Option{
			Domain:       "example.org",
			Binds:        []*string{},
			DenyOn:       []labels.Resource{},
			KnownValues:  []string{},
			Aliases:      []string{},
			ValueAliases: map[string]string{},
			Doc:          wellknown.Label{UsedOn: []runtime.Object{}, Values: []docs.Value{}},
		})).To(Succeed())
	})
	It("should treat empty KnownValues as no known values", func() {
		Expect(labels.Register(labels.Option{Key: "example.com/key", KnownValues: []string{}})).To(Succeed())
		_, ok := labels.KnownValues("example.com/key")
		Expect(ok).To(BeFalse())
	})
	It("should reject a key that a later option uses as an alias", func() {
		Expect(labels.Register(
			labels.Option{Key: "example.com/key"},
			labels.Option{Key: "example.com/other", Aliases: []string{"example.com/key"}},
		)).To(MatchError(ContainSubstring(`"example.com/key" is already registered as a key`)))
	})
	It("should reject a key that an earlier option uses as an alias", func() {
		Expect(labels.Register(
			labels.Option{Key: "example.com/other", Aliases: []string{"example.com/key"}},
			labels.Option{Key: "example.com/key"},
		)).To(MatchError(ContainSubstring(`"example.com/key" is already registered as an alias`)))
	})
	It("should reject an alias registered by two options", func() {
		Expect(labels.Register(
			labels.Option{Key: "example.com/a", Aliases: []string{"example.com/alias"}},
			labels.Option{Key: "example.com/b", Aliases: []string{"example.com/alias"}},
		)).To(MatchError(ContainSubstring(`"example.com/alias" is already registered as an alias`)))
	})
	It("should reject a var bound by two options", func() {
		Expect(labels.Register(
			labels.Option{Key: "example.com/a", Binds: []*string{&bound}},
			labels.Option{Key: "example.com/b", Binds: []*string{&bound}},
		)).To(MatchError(ContainSubstring("already bound")))
	})
	It("should reject a var bound twice by one option", func() {
		Expect(labels.Register(labels.Option{Key: "example.com/a", Binds: []*string{&bound, &bound}})).To(MatchError(ContainSubstring("already bound")))
	})
	DescribeTable("should reject a domain nested in another",
		func(first, second labels.Option) {
			Expect(labels.Register(first, second)).To(MatchError(ContainSubstring(`domain "sub.example.com" is a subdomain of registered domain "example.com"`)))
		},
		Entry("subdomain registered second", labels.Option{Domain: "example.com"}, labels.Option{Domain: "sub.example.com"}),
		Entry("subdomain registered first", labels.Option{Domain: "sub.example.com"}, labels.Option{Domain: "example.com"}),
		Entry("admission-unrestricted subdomain", labels.Option{Domain: "example.com"}, labels.Option{Domain: "sub.example.com", AdmissionUnrestricted: true}),
	)
	It("should allow domains that only share a suffix without a dot boundary", func() {
		Expect(labels.Register(labels.Option{Domain: "example.com"}, labels.Option{Domain: "notexample.com"})).To(Succeed())
	})
	DescribeTable("should reject a duplicate domain",
		func(first, second labels.Option) {
			Expect(labels.Register(first, second)).To(MatchError(ContainSubstring(`domain "example.com" is registered more than once`)))
		},
		Entry("restricted twice", labels.Option{Domain: "example.com"}, labels.Option{Domain: "example.com"}),
		Entry("restricted and admission-unrestricted", labels.Option{Domain: "example.com"}, labels.Option{Domain: "example.com", AdmissionUnrestricted: true}),
	)
})

var _ = Describe("Reads before Register", func() {
	BeforeEach(func() {
		labelstest.Reset()
	})
	DescribeTable("should panic naming labels.Register",
		func(read func()) {
			Expect(read).To(PanicWith(ContainSubstring("labels.Register")))
		},
		Entry("Validate", func() { _ = labels.Validate("example.com/key", labels.Pod) }),
		Entry("IsWellKnown", func() { labels.IsWellKnown("example.com/key") }),
		Entry("WellKnown", func() { labels.WellKnown() }),
		Entry("KnownValues", func() { labels.KnownValues("example.com/key") }),
		Entry("Normalize", func() { labels.Normalize("example.com/key") }),
		Entry("Domains", func() { labels.Domains() }),
		Entry("Denied", func() { labels.Denied(labels.Pod) }),
		Entry("Aliases", func() { labels.Aliases() }),
	)
})

var _ = Describe("Semantics", func() {
	const (
		restricted    = "restricted.example.com"
		coInstalled   = "coinstalled.example.com"
		registered    = restricted + "/registered"
		deniedOnPool  = "example.com/denied-on-nodepool"
		restrictedKey = "example.com/restricted"
		canonical     = "example.com/canonical"
		alias         = "example.com/alias"
	)
	BeforeEach(func() {
		labelstest.Register(GinkgoTB(),
			labels.Option{Domain: restricted},
			labels.Option{Domain: coInstalled, AdmissionUnrestricted: true},
			labels.Option{Key: registered, KnownValues: []string{"a", "b"}},
			labels.Option{Key: deniedOnPool, DenyOn: []labels.Resource{labels.NodePool}},
			labels.Option{Key: restrictedKey, DenyOn: labels.All()},
			labels.Option{Key: canonical, Aliases: []string{alias, restricted + "/alias"}, DenyOn: []labels.Resource{labels.NodeOverlay}, ValueAliases: map[string]string{"": "0"}},
		)
	})

	Context("Validate", func() {
		It("should reject a registered key only on the resources it's denied on", func() {
			Expect(labels.Validate(deniedOnPool, labels.NodePool)).To(MatchError(`label "example.com/denied-on-nodepool" is restricted on NodePool`))
			for _, r := range []labels.Resource{labels.NodeClaim, labels.NodeOverlay, labels.Pod} {
				Expect(labels.Validate(deniedOnPool, r)).To(Succeed(), string(r))
			}
		})
		It("should reject an unregistered key in a restricted domain on every resource", func() {
			for _, r := range labels.All() {
				Expect(labels.Validate(restricted+"/unregistered", r)).To(MatchError(`label domain "restricted.example.com" is restricted`), string(r))
				// A registered key in the same domain is allowed, so the domain, not the resource, is the reason.
				Expect(labels.Validate(registered, r)).To(Succeed(), string(r))
			}
		})
		It("should reject an unregistered key in a subdomain of a restricted domain", func() {
			Expect(labels.Validate("sub."+restricted+"/unregistered", labels.Pod)).To(MatchError(`label domain "restricted.example.com" is restricted`))
		})
		It("should match domains on a dot boundary", func() {
			Expect(labels.Validate("not-"+restricted+"/unregistered", labels.NodePool)).To(Succeed())
		})
		It("should never domain-restrict a key without a slash", func() {
			Expect(labels.Validate(restricted, labels.NodePool)).To(Succeed())
		})
		It("should allow an unregistered key outside every restricted domain", func() {
			Expect(labels.Validate("example.com/unregistered", labels.NodePool)).To(Succeed())
		})
		It("should restrict an admission-unrestricted domain on Owned resources only", func() {
			for _, r := range labels.Owned() {
				Expect(labels.Validate(coInstalled+"/key", r)).To(MatchError(`label domain "coinstalled.example.com" is restricted`), string(r))
			}
			for _, r := range []labels.Resource{labels.NodeOverlay, labels.Pod} {
				Expect(labels.Validate(coInstalled+"/key", r)).To(Succeed(), string(r))
			}
		})
		It("should apply the registered key's DenyOn to its aliases", func() {
			Expect(labels.Validate(alias, labels.NodeOverlay)).To(MatchError(`label "example.com/alias" is restricted on NodeOverlay`))
			Expect(labels.Validate(alias, labels.NodePool)).To(Succeed())
		})
		It("should allow an alias inside a restricted domain like its registered key", func() {
			Expect(labels.Validate(restricted+"/alias", labels.NodePool)).To(Succeed())
			Expect(labels.Validate(restricted+"/alias", labels.NodeOverlay)).To(MatchError(ContainSubstring("is restricted on NodeOverlay")))
		})
	})

	Context("well-known and restricted keys", func() {
		It("should treat a key denied on every resource as restricted, not well-known", func() {
			Expect(labels.IsWellKnown(restrictedKey)).To(BeFalse())
			Expect(labels.WellKnown().Has(restrictedKey)).To(BeFalse())
			for _, r := range labels.All() {
				Expect(labels.Validate(restrictedKey, r)).To(HaveOccurred(), string(r))
				Expect(labels.Denied(r)).To(ContainElement(restrictedKey), string(r))
			}
		})
		It("should treat a key denied on some resources as well-known", func() {
			Expect(labels.IsWellKnown(deniedOnPool)).To(BeTrue())
		})
		It("should report every other registered key, and no alias, as well-known", func() {
			Expect(labels.WellKnown()).To(Equal(sets.New(registered, deniedOnPool, canonical)))
			Expect(labels.IsWellKnown(alias)).To(BeFalse())
		})
	})

	Context("Normalize", func() {
		It("should rewrite an alias to its registered key and apply value aliases", func() {
			key, values := labels.Normalize(alias, "", "1")
			Expect(key).To(Equal(canonical))
			Expect(values).To(Equal([]string{"0", "1"}))
		})
		It("should apply value aliases to a requirement on the registered key itself", func() {
			_, values := labels.Normalize(canonical, "")
			Expect(values).To(Equal([]string{"0"}))
		})
		It("should not modify the caller's values", func() {
			values := []string{""}
			labels.Normalize(canonical, values...)
			Expect(values).To(Equal([]string{""}))
		})
		It("should leave keys without aliases or value aliases unchanged", func() {
			key, values := labels.Normalize(registered, "")
			Expect(key).To(Equal(registered))
			Expect(values).To(Equal([]string{""}))
		})
	})

	Context("reads", func() {
		It("should return known values only for keys that register them", func() {
			values, ok := labels.KnownValues(registered)
			Expect(ok).To(BeTrue())
			Expect(values).To(Equal(sets.New("a", "b")))
			_, ok = labels.KnownValues(canonical)
			Expect(ok).To(BeFalse())
		})
		It("should return restricted domains, excluding admission-unrestricted ones", func() {
			Expect(labels.Domains()).To(Equal([]string{restricted}))
		})
		It("should return the registered keys denied on a resource, sorted, without aliases", func() {
			Expect(labels.Denied(labels.NodePool)).To(Equal([]string{deniedOnPool, restrictedKey}))
			Expect(labels.Denied(labels.NodeOverlay)).To(Equal([]string{canonical, restrictedKey}))
		})
		It("should return a copy of the aliases", func() {
			aliases := labels.Aliases()
			Expect(aliases).To(Equal(map[string]string{alias: canonical, restricted + "/alias": canonical}))
			delete(aliases, alias)
			Expect(labels.Aliases()).To(HaveKey(alias))
		})
	})
})

var _ = Describe("Sorted reads", func() {
	It("should sort Domains and Denied regardless of registration order", func() {
		labelstest.Register(GinkgoTB(),
			labels.Option{Domain: "c.example.com"},
			labels.Option{Domain: "b.example.com"},
			labels.Option{Domain: "a.example.com"},
			labels.Option{Key: "example.com/c", DenyOn: []labels.Resource{labels.Pod}},
			labels.Option{Key: "example.com/b", DenyOn: []labels.Resource{labels.Pod}},
			labels.Option{Key: "example.com/a", DenyOn: []labels.Resource{labels.Pod}},
		)
		Expect(labels.Domains()).To(Equal([]string{"a.example.com", "b.example.com", "c.example.com"}))
		Expect(labels.Denied(labels.Pod)).To(Equal([]string{"example.com/a", "example.com/b", "example.com/c"}))
	})
})

var _ = Describe("Resources", func() {
	It("should return a fresh slice from All and Owned", func() {
		labels.All()[0] = "edited"
		labels.Owned()[0] = "edited"
		Expect(labels.All()).To(Equal([]labels.Resource{labels.NodePool, labels.NodeClaim, labels.NodeOverlay, labels.Pod}))
		Expect(labels.Owned()).To(Equal([]labels.Resource{labels.NodePool, labels.NodeClaim}))
	})
})

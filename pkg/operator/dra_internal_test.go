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

package operator

import (
	"context"
	"errors"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/client-go/discovery"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"sigs.k8s.io/karpenter/pkg/controllers/dynamicresources/deviceallocation"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/pkg/utils/env"
)

var _ = Describe("DRA API validation", func() {
	var draCtx context.Context
	BeforeEach(func() {
		draCtx = options.ToContext(context.Background(), test.Options(test.OptionsFields{IgnoreDRARequests: new(false)}))
	})
	It("should skip discovery when DRA requests are ignored", func() {
		dc := &fakediscovery.FakeDiscovery{Fake: &clienttesting.Fake{}}
		Expect(validateDRASupport(options.ToContext(context.Background(), test.Options()), dc)).To(Succeed())
		Expect(dc.Actions()).To(BeEmpty())
	})
	It("should succeed when resource.k8s.io/v1 is served", func() {
		dc := &fakediscovery.FakeDiscovery{Fake: &clienttesting.Fake{
			Resources: []*metav1.APIResourceList{{GroupVersion: "resource.k8s.io/v1"}},
		}}
		Expect(validateDRASupport(draCtx, dc)).To(Succeed())
	})
	It("should name the flag and the required Kubernetes version when resource.k8s.io/v1 isn't served", func() {
		dc := &fakediscovery.FakeDiscovery{Fake: &clienttesting.Fake{
			Resources: []*metav1.APIResourceList{{GroupVersion: "resource.k8s.io/v1beta1"}},
		}}
		Expect(validateDRASupport(draCtx, dc)).To(MatchError(And(
			ContainSubstring("--ignore-dra-requests=false"),
			ContainSubstring("resource.k8s.io/v1"),
			ContainSubstring("Kubernetes 1.34+"),
		)))
	})
	It("should wrap other discovery errors", func() {
		fake := &clienttesting.Fake{}
		fake.AddReactor("get", "resource", func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("connection refused")
		})
		Expect(validateDRASupport(draCtx, &fakediscovery.FakeDiscovery{Fake: fake})).To(MatchError(And(
			ContainSubstring("checking for the resource.k8s.io/v1 API"),
			ContainSubstring("connection refused"),
		)))
	})

	Context("API server without resource.k8s.io/v1", Ordered, func() {
		var testEnv *envtest.Environment
		var cfg *rest.Config
		BeforeAll(func() {
			testEnv = &envtest.Environment{Scheme: scheme.Scheme}
			// Older API servers don't serve resource.k8s.io/v1 at all.
			if version.MustParseSemantic(strings.ReplaceAll(env.WithDefaultString("K8S_VERSION", "1.36.x"), ".x", ".0")).Minor() >= 34 {
				testEnv.ControlPlane.GetAPIServer().Configure().Set("runtime-config", "resource.k8s.io/v1=false")
			}
			var err error
			cfg, err = testEnv.Start()
			Expect(err).ToNot(HaveOccurred())
		})
		AfterAll(func() {
			Expect(testEnv.Stop()).To(Succeed())
		})
		It("should fail validation", func() {
			Expect(validateDRASupport(draCtx, discovery.NewDiscoveryClientForConfigOrDie(cfg))).To(MatchError(ContainSubstring("--ignore-dra-requests=false")))
		})
		It("should return an error instead of panicking when hydrating device allocations", func() {
			kubeClient, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
			Expect(err).ToNot(HaveOccurred())
			hydrateErr := deviceallocation.NewController(kubeClient).Hydrate(draCtx)
			Expect(meta.IsNoMatchError(hydrateErr)).To(BeTrue(), "expected a no-match error, got %v", hydrateErr)
		})
	})
})

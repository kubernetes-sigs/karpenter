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

package kwok_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"sigs.k8s.io/karpenter/kwok/apis/v1alpha1"
	kwok "sigs.k8s.io/karpenter/kwok/cloudprovider"
	"sigs.k8s.io/karpenter/kwok/options"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

func TestKWOKCloudProvider(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "KWOK CloudProvider")
}

var _ = Describe("Delete", func() {
	var ctx context.Context
	var instanceTypes []*cloudprovider.InstanceType
	var nodeClaim *v1.NodeClaim

	BeforeEach(func() {
		ctx = context.Background()
		var err error
		instanceTypes, err = kwok.ConstructInstanceTypes(options.ToContext(ctx, &options.Options{}))
		Expect(err).ToNot(HaveOccurred())
		nodeClaim = &v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "default-abcde"},
			Spec: v1.NodeClaimSpec{
				NodeClassRef: &v1.NodeClassReference{Group: "karpenter.kwok.sh", Kind: "KWOKNodeClass", Name: "default"},
				Requirements: []v1.NodeSelectorRequirementWithMinValues{{
					Key:      corev1.LabelInstanceTypeStable,
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{instanceTypes[0].Name},
				}},
				Resources: v1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}},
			},
		}
	})

	newCloudProvider := func(delay time.Duration, funcs interceptor.Funcs) (*kwok.CloudProvider, client.Client) {
		nodeClass := &v1alpha1.KWOKNodeClass{
			ObjectMeta: metav1.ObjectMeta{Name: "default"},
			Spec:       v1alpha1.KWOKNodeClassSpec{NodeRegistrationDelay: metav1.Duration{Duration: delay}},
		}
		kubeClient := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(nodeClass, nodeClaim).WithInterceptorFuncs(funcs).Build()
		return kwok.NewCloudProvider(ctx, kubeClient, instanceTypes), kubeClient
	}
	launch := func(cp *kwok.CloudProvider) {
		created, err := cp.Create(ctx, nodeClaim)
		Expect(err).ToNot(HaveOccurred())
		nodeClaim.Status.ProviderID = created.Status.ProviderID
	}
	nodes := func(kubeClient client.Client) func(Gomega) []corev1.Node {
		return func(g Gomega) []corev1.Node {
			list := &corev1.NodeList{}
			g.Expect(kubeClient.List(ctx, list)).To(Succeed())
			return list.Items
		}
	}

	It("should delete the Node and stay idempotent", func() {
		cp, kubeClient := newCloudProvider(0, interceptor.Funcs{})
		launch(cp)
		Eventually(nodes(kubeClient)).Should(HaveLen(1))

		Expect(cloudprovider.IsNodeClaimNotFoundError(cp.Delete(ctx, nodeClaim))).To(BeTrue())
		Expect(nodes(kubeClient)(Default)).To(BeEmpty())
		Expect(cloudprovider.IsNodeClaimNotFoundError(cp.Delete(ctx, nodeClaim))).To(BeTrue())
	})
	It("should not delete a Node for a providerID KWOK does not own", func() {
		cp, _ := newCloudProvider(0, interceptor.Funcs{
			// The fake client skips the name check client-go does before sending a request
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if msgs := rest.IsValidPathSegmentName(obj.GetName()); len(msgs) != 0 {
					return fmt.Errorf("invalid resource name %q: %v", obj.GetName(), msgs)
				}
				return c.Delete(ctx, obj, opts...)
			},
		})
		nodeClaim.Status.ProviderID = "Provider:///AZ/i-01234567890123456"

		Expect(cp.Delete(ctx, nodeClaim)).To(Satisfy(cloudprovider.IsNodeClaimNotFoundError))
	})
	It("should return NodeClaimNotFound from Get for a providerID KWOK does not own", func() {
		cp, _ := newCloudProvider(0, interceptor.Funcs{
			// The fake client skips the name check client-go does before sending a request
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if msgs := rest.IsValidPathSegmentName(key.Name); len(msgs) != 0 {
					return fmt.Errorf("invalid resource name %q: %v", key.Name, msgs)
				}
				return c.Get(ctx, key, obj, opts...)
			},
		})

		_, err := cp.Get(ctx, "Provider:///AZ/i-01234567890123456")
		Expect(err).To(Satisfy(cloudprovider.IsNodeClaimNotFoundError))
	})
	It("should not create the Node when Delete runs before the registration delay elapses", func() {
		cp, kubeClient := newCloudProvider(200*time.Millisecond, interceptor.Funcs{})
		launch(cp)

		Expect(cloudprovider.IsNodeClaimNotFoundError(cp.Delete(ctx, nodeClaim))).To(BeTrue())
		Consistently(nodes(kubeClient)).WithTimeout(time.Second).Should(BeEmpty())
	})
	It("should not orphan a Node whose create is in flight when Delete runs", func() {
		entered, release := make(chan struct{}), make(chan struct{})
		cp, kubeClient := newCloudProvider(0, interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*corev1.Node); ok {
					close(entered)
					<-release
				}
				return c.Create(ctx, obj, opts...)
			},
		})
		launch(cp)
		Eventually(entered).Should(BeClosed())

		deleted := make(chan error, 1)
		go func() { deleted <- cp.Delete(ctx, nodeClaim) }()
		// Delete waits for the in-flight create instead of racing it
		Consistently(deleted).WithTimeout(300 * time.Millisecond).ShouldNot(Receive())
		close(release)

		var err error
		Eventually(deleted).Should(Receive(&err))
		Expect(cloudprovider.IsNodeClaimNotFoundError(err)).To(BeTrue())
		Expect(nodes(kubeClient)(Default)).To(BeEmpty())
	})
})

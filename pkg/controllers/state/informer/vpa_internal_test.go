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

package informer

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr/funcr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

var _ = Describe("VPA CRD lookup", func() {
	crdResource := schema.GroupResource{Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions"}
	forbidden := func(context.Context, client.Object) error {
		return apierrors.NewForbidden(crdResource, vpaCRDName, fmt.Errorf("rbac"))
	}

	DescribeTable("should report the CRD's state",
		func(get func(context.Context, client.Object) error, expected crdState) {
			c := &VPAController{apiReader: getReader(get)}
			Expect(c.vpaCRDState(context.Background())).To(Equal(expected))
		},
		Entry("present", func(context.Context, client.Object) error { return nil }, crdPresent),
		Entry("terminating", func(_ context.Context, obj client.Object) error {
			obj.SetDeletionTimestamp(&metav1.Time{Time: time.Now()})
			return nil
		}, crdTerminating),
		Entry("absent", func(context.Context, client.Object) error {
			return apierrors.NewNotFound(crdResource, vpaCRDName)
		}, crdAbsent),
		Entry("unknown when forbidden", forbidden, crdUnknown),
		Entry("unknown when the lookup fails", func(context.Context, client.Object) error {
			return apierrors.NewServiceUnavailable("unavailable")
		}, crdUnknown),
	)

	It("should get the CRD's metadata by name with a bounded timeout", func() {
		start := time.Now()
		c := &VPAController{apiReader: getReader(func(ctx context.Context, obj client.Object) error {
			deadline, ok := ctx.Deadline()
			Expect(ok).To(BeTrue())
			Expect(deadline).To(BeTemporally("~", start.Add(vpaCRDLookupTimeout), time.Second))
			Expect(obj.GetName()).To(Equal(vpaCRDName))
			Expect(obj).To(BeAssignableToTypeOf(&metav1.PartialObjectMetadata{}))
			Expect(obj.GetObjectKind().GroupVersionKind()).To(Equal(apiextensionsv1.SchemeGroupVersion.WithKind("CustomResourceDefinition")))
			return nil
		})}
		Expect(c.vpaCRDState(context.Background())).To(Equal(crdPresent))
	})

	It("should log a forbidden lookup once", func() {
		var logs []string
		logCtx := log.IntoContext(context.Background(), funcr.New(func(_, args string) { logs = append(logs, args) }, funcr.Options{}))
		c := &VPAController{apiReader: getReader(forbidden)}

		for range 2 {
			Expect(c.vpaCRDState(logCtx)).To(Equal(crdUnknown))
		}
		Expect(logs).To(HaveExactElements(ContainSubstring(vpaCRDName)))
	})
})

// getReader answers every Get with get, after setting the object's name to the requested one.
type getReader func(ctx context.Context, obj client.Object) error

func (g getReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	obj.SetName(key.Name)
	return g(ctx, obj)
}

func (g getReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return fmt.Errorf("unexpected list")
}

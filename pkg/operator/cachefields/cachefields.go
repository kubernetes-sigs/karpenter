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

// Package cachefields drops fields Karpenter never reads from objects before they're stored in the informer cache.
// Every cached copy costs memory in proportion to cluster size, so unread fields are pure overhead.
package cachefields

//go:generate go run ../../../hack/code/strippedfields ../../../hack/tools/karpenterlint/internal/analyzers/strippedread/zz_generated.fields.go

import (
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/awslabs/operatorpkg/serrors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Field is a field of a cached object.
type Field struct {
	// Object is the type the field belongs to. Nil means every typed object.
	Object client.Object
	// Path is the Go field path from the object, e.g. "Status.Images". Embedded fields are spelled out, e.g.
	// "ObjectMeta.ManagedFields". Every field on the path except the last must be a struct value.
	Path string
}

// Stripped lists the fields dropped from cached objects. Reading one of them from a cached object silently returns the
// zero value, so the karpenterlint strippedread analyzer flags reads of these fields. `go generate` copies this table
// to the analyzer. To start reading a field, remove it here. Only typed objects are stripped, since the transforms find
// fields by their Go path. Write cached objects back with a patch, not an Update, since an Update would clear the
// stripped fields on the server.
var Stripped = []Field{
	// Karpenter never reads managedFields, which is often a quarter to a third of an object.
	{Path: "ObjectMeta.ManagedFields"},
	// Karpenter never reads the images on a node, which the kubelet reports up to 50 of by default.
	{Object: &corev1.Node{}, Path: "Status.Images"},
}

// SetTransforms sets cache transforms on opts that strip the Stripped fields, except the fields in keep. Cloud providers
// share the operator's cache, so they pass the Stripped fields they read in keep. An entry in keep with a nil Object
// keeps the field on every type.
func SetTransforms(opts *cache.Options, keep ...Field) error {
	for _, f := range slices.Concat(Stripped, keep) {
		if _, _, err := f.Resolve(); err != nil {
			return err
		}
	}
	isKept := func(t reflect.Type, path string) bool {
		return slices.ContainsFunc(keep, func(k Field) bool {
			return k.Path == path && (k.Object == nil || reflect.TypeOf(k.Object) == t)
		})
	}

	var defaults []string
	for _, f := range Stripped {
		if f.Object == nil && !isKept(nil, f.Path) {
			defaults = append(defaults, f.Path)
		}
	}
	// Every type with a field of its own, or kept on it alone, needs its own transform.
	var types []client.Object
	for _, f := range slices.Concat(Stripped, keep) {
		if f.Object != nil && !slices.ContainsFunc(types, func(o client.Object) bool { return reflect.TypeOf(o) == reflect.TypeOf(f.Object) }) {
			types = append(types, f.Object)
		}
	}
	if len(defaults) > 0 {
		if opts.DefaultTransform != nil {
			return fmt.Errorf("cache options already set a default transform")
		}
		opts.DefaultTransform = transform(defaults)
	}

	// A type's Transform replaces DefaultTransform rather than running after it, so each type's transform also strips the
	// fields stripped from every type.
	for _, obj := range types {
		t := reflect.TypeOf(obj)
		var paths []string
		for _, f := range Stripped {
			if (f.Object == nil || reflect.TypeOf(f.Object) == t) && !isKept(t, f.Path) {
				paths = append(paths, f.Path)
			}
		}
		if opts.ByObject == nil {
			opts.ByObject = map[client.Object]cache.ByObject{}
		}
		key, byObject := obj, cache.ByObject{}
		for k, v := range opts.ByObject {
			if reflect.TypeOf(k) == t {
				key, byObject = k, v
			}
		}
		if byObject.Transform != nil {
			return serrors.Wrap(fmt.Errorf("cache options already set a transform"), "kind", t.Elem().Name())
		}
		// Not nil even with no paths, since a nil Transform falls back to DefaultTransform.
		byObject.Transform = transform(paths)
		opts.ByObject[key] = byObject
	}
	return nil
}

// Resolve returns the struct type that declares the field at the end of the path, and the field's name.
func (f Field) Resolve() (reflect.Type, string, error) {
	// Every Kubernetes object has the fields of PartialObjectMetadata.
	t := reflect.TypeOf(f.Object)
	if f.Object == nil {
		t = reflect.TypeFor[*metav1.PartialObjectMetadata]()
	}
	t = t.Elem()
	names := strings.Split(f.Path, ".")
	for i, name := range names {
		if t.Kind() != reflect.Struct {
			return nil, "", f.errorf("%s is not a struct", strings.Join(names[:i], "."))
		}
		sf, ok := t.FieldByName(name)
		if !ok || len(sf.Index) != 1 || !sf.IsExported() {
			return nil, "", f.errorf("%s has no exported field %s", t.Name(), name)
		}
		if i == len(names)-1 {
			return t, name, nil
		}
		t = sf.Type
	}
	return nil, "", f.errorf("empty path")
}

func (f Field) errorf(format string, args ...any) error {
	err := fmt.Errorf("resolving cache field %q, "+format, append([]any{f.Path}, args...)...)
	if f.Object == nil {
		return err
	}
	return serrors.Wrap(err, "kind", reflect.TypeOf(f.Object).Elem().Name())
}

// transform zeroes each path on the object. It returns objects without the paths, such as unstructured objects,
// unchanged.
func transform(paths []string) toolscache.TransformFunc {
	split := make([][]string, len(paths))
	for i, p := range paths {
		split[i] = strings.Split(p, ".")
	}
	return func(in any) (any, error) {
		v := reflect.ValueOf(in)
		if v.Kind() != reflect.Pointer || v.IsNil() {
			return in, nil
		}
		for _, names := range split {
			if field := fieldByPath(v.Elem(), names); field.IsValid() && field.CanSet() {
				field.SetZero()
			}
		}
		return in, nil
	}
}

// fieldByPath returns the field at the path, or the zero Value if v doesn't have it.
func fieldByPath(v reflect.Value, names []string) reflect.Value {
	for _, name := range names {
		if v.Kind() != reflect.Struct {
			return reflect.Value{}
		}
		v = v.FieldByName(name)
	}
	return v
}

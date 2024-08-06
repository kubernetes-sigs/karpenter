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

package pkg

import (
	"context"
	"fmt"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type ObjectKey = types.NamespacedName

// A client which implements the minimal client interface to allow scheduling resimulation
type DataClient struct {
	persistentvolumeclaimlist *v1.PersistentVolumeClaimList
	persistentvolumelist      *v1.PersistentVolumeList
}

func New(pvlist *v1.PersistentVolumeList, pvclist *v1.PersistentVolumeClaimList) *DataClient {
	d := &DataClient{}
	d.persistentvolumeclaimlist = pvclist
	d.persistentvolumelist = pvlist
	return d
}

// Currently the scheduler only needs to be able to read for the persistent volume and the persistent volume claim
func (d DataClient) Get(_ context.Context, key ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if obj.GetObjectKind().GroupVersionKind().Kind == "PersistentVolumeClaim" {
		objPVC, found := lo.Find(d.persistentvolumeclaimlist.Items, func(pvc v1.PersistentVolumeClaim) bool {
			return pvc.Name == key.Name && pvc.Namespace == key.Namespace
		})
		if found {
			obj = objPVC.DeepCopy()
		} else {
			return fmt.Errorf("PersistentVolumeClaim not found with, %s", key)
		}
	}

	if obj.GetObjectKind().GroupVersionKind().Kind == "PersistentVolume" {
		objPV, found := lo.Find(d.persistentvolumelist.Items, func(pv v1.PersistentVolume) bool {
			return pv.Name == key.Name && pv.Namespace == key.Namespace
		})
		if found {
			obj = objPV.DeepCopy()
		} else {
			return fmt.Errorf("PersistentVolume not found with, %s", key)
		}
	}

	return nil
}

/* Empty interface definitions below. Not needed for resimulation */

func (DataClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return nil
}

func (DataClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	return nil
}

func (DataClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	return nil
}

func (DataClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	return nil
}

func (DataClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	return nil
}

func (DataClient) DeleteAllOf(ctx context.Context, obj client.Object, opts ...client.DeleteAllOfOption) error {
	return nil
}

func (DataClient) Status() client.SubResourceWriter { return nil }

func (DataClient) SubResource(subResource string) client.SubResourceClient { return nil }

// Scheme returns the scheme this client is using.
func (DataClient) Scheme() *runtime.Scheme { return nil }

// RESTMapper returns the rest this client is using.
func (DataClient) RESTMapper() meta.RESTMapper { return nil }

// GroupVersionKindFor returns the GroupVersionKind for the given object.
func (DataClient) GroupVersionKindFor(obj runtime.Object) (schema.GroupVersionKind, error) {
	return schema.GroupVersionKind{}, nil
}

// IsObjectNamespaced returns true if the GroupVersionKind of the object is namespaced.
func (DataClient) IsObjectNamespaced(obj runtime.Object) (bool, error) { return false, nil }

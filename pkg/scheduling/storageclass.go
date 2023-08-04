/*
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

package scheduling

import (
	"context"
	"sort"
	"time"

	"github.com/samber/lo"
	storagev1 "k8s.io/api/storage/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/utils/atomic"
)

const IsDefaultStorageClassAnnotation = "storageclass.kubernetes.io/is-default-class"

// Cache the lookup of our default storage class, global atomic cache since the volume usage is per node
var defaultStorageClass = atomic.NewCachedVariable[string](1 * time.Minute)

// ResetDefaultStorageClass is intended to be called from unit tests to reset the default storage class
func ResetDefaultStorageClass() {
	defaultStorageClass.Reset()
}

func DiscoverDefaultStorageClassName(ctx context.Context, kubeClient client.Client) (string, error) {
	if name, ok := defaultStorageClass.Get(); ok {
		return name, nil
	}

	storageClassList := &storagev1.StorageClassList{}
	if err := kubeClient.List(ctx, storageClassList); err != nil {
		return "", err
	}
	// Find all StorageClasses that have the default annotation
	defaults := lo.Filter(storageClassList.Items, func(sc storagev1.StorageClass, _ int) bool {
		return sc.Annotations[IsDefaultStorageClassAnnotation] == "true"
	})
	if len(defaults) == 0 {
		return "", nil
	}
	// Sort the default StorageClasses by timestamp and take the newest one
	// https://github.com/kubernetes/kubernetes/pull/110559
	sort.Slice(defaults, func(i, j int) bool {
		return defaults[i].CreationTimestamp.After(defaults[j].CreationTimestamp.Time)
	})
	defaultStorageClass.Set(defaults[0].Name)
	return defaults[0].Name, nil
}

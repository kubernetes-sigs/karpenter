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

package functional

import (
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/utils/clock"
	"knative.dev/pkg/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
)

type Pair[A, B any] struct {
	First  A
	Second B
}

type Option[T any] func(T) T

func ResolveOptions[T any](opts ...Option[T]) T {
	o := *new(T)
	for _, opt := range opts {
		if opt != nil {
			o = opt(o)
		}
	}
	return o
}

// HasAnyPrefix returns true if any of the provided prefixes match the given string s
func HasAnyPrefix(s string, prefixes ...string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

// SplitCommaSeparatedString splits a string by commas, removes whitespace, and returns
// a slice of strings
func SplitCommaSeparatedString(value string) []string {
	var result []string
	for _, value := range strings.Split(value, ",") {
		result = append(result, strings.TrimSpace(value))
	}
	return result
}

func Unmarshal[T any](raw []byte) (*T, error) {
	t := *new(T)
	if err := yaml.Unmarshal(raw, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func FilterMap[K comparable, V any](m map[K]V, f func(K, V) bool) map[K]V {
	ret := map[K]V{}
	for k, v := range m {
		if f(k, v) {
			ret[k] = v
		}
	}
	return ret
}

func GetExpirationTime(obj client.Object, provisioner *v1alpha5.Provisioner) time.Time {
	if provisioner == nil || provisioner.Spec.TTLSecondsUntilExpired == nil || obj == nil {
		// If not defined, return some much larger time.
		return time.Date(5000, 0, 0, 0, 0, 0, 0, time.UTC)
	}
	expirationTTL := time.Duration(ptr.Int64Value(provisioner.Spec.TTLSecondsUntilExpired)) * time.Second
	return obj.GetCreationTimestamp().Add(expirationTTL)
}

func IsExpired(obj client.Object, clock clock.Clock, provisioner *v1alpha5.Provisioner) bool {
	return clock.Now().After(GetExpirationTime(obj, provisioner))
}

func GetEmptinessTTLTime(obj client.Object, provisioner *v1alpha5.Provisioner) time.Time {
	if provisioner == nil || provisioner.Spec.TTLSecondsAfterEmpty == nil || obj == nil {
		// If not defined, return some much larger time.
		return time.Date(5000, 0, 0, 0, 0, 0, 0, time.UTC)
	}
	expirationTTL := time.Duration(ptr.Int64Value(provisioner.Spec.TTLSecondsAfterEmpty)) * time.Second
	return obj.GetCreationTimestamp().Add(expirationTTL)
}

func IsPastEmptinessTTL(obj client.Object, clock clock.Clock, provisioner *v1alpha5.Provisioner) bool {
	return clock.Now().After(GetEmptinessTTLTime(obj, provisioner))
}

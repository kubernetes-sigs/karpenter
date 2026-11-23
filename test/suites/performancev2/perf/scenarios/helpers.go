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

package scenarios

import (
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/test/suites/performancev2/perf"
)

// ownPool pins a deployment to the run's NodePool, so a real cluster's own nodes aren't topology domains.
func ownPool(r *perf.Run) test.DeploymentOptionModifier {
	return test.WithNodeSelector(map[string]string{v1.NodePoolLabelKey: r.NodePool.Name})
}

// frac returns n*num/den, at least 1.
func frac(n, num, den int) int {
	return max(1, n*num/den)
}

func objects[T client.Object](in []T) []client.Object {
	out := make([]client.Object, 0, len(in))
	for _, o := range in {
		out = append(out, o)
	}
	return out
}

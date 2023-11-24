/*
Copyright 2023 The Kubernetes Authors.

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
	"fmt"

	"github.com/samber/lo"
	"go.uber.org/multierr"
	v1 "k8s.io/api/core/v1"
	cloudproviderapi "k8s.io/cloud-provider/api"
)

var KnownEphemeralTaints = []v1.Taint{
	{Key: v1.TaintNodeNotReady, Effect: v1.TaintEffectNoSchedule},
	{Key: v1.TaintNodeUnreachable, Effect: v1.TaintEffectNoSchedule},
	{Key: cloudproviderapi.TaintExternalCloudProvider, Effect: v1.TaintEffectNoSchedule, Value: "true"},
}

// Taints is a decorated alias type for []v1.Taint
type Taints []v1.Taint

// Tolerates returns true if the pod tolerates all taints.
func (ts Taints) Tolerates(pod *v1.Pod) (errs error) {
	for i := range ts {
		taint := ts[i]
		tolerates := false
		for _, t := range pod.Spec.Tolerations {
			tolerates = tolerates || t.ToleratesTaint(&taint)
		}
		if !tolerates {
			errs = multierr.Append(errs, fmt.Errorf("did not tolerate %s=%s:%s", taint.Key, taint.Value, taint.Effect))
		}
	}
	return errs
}

// Merge merges in taints with the passed in taints.
func (ts Taints) Merge(with Taints) Taints {
	res := lo.Map(ts, func(t v1.Taint, _ int) v1.Taint {
		return t
	})
	for _, taint := range with {
		if _, ok := lo.Find(res, func(t v1.Taint) bool {
			return taint.MatchTaint(&t)
		}); !ok {
			res = append(res, taint)
		}
	}
	return res
}

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

package disruption

import (
	"context"

	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PDBLimits is used to evaluate if evicting a list of pods is possible.
type PDBLimits struct {
	ctx        context.Context
	kubeClient client.Client
	pdbs       []*pdbItem
}

func NewPDBLimits(ctx context.Context, kubeClient client.Client) (*PDBLimits, error) {
	ps := &PDBLimits{
		ctx:        ctx,
		kubeClient: kubeClient,
	}

	var pdbList policyv1.PodDisruptionBudgetList
	if err := kubeClient.List(ctx, &pdbList); err != nil {
		return nil, err
	}
	for _, pdb := range pdbList.Items {
		pi, err := newPdb(pdb)
		if err != nil {
			return nil, err
		}
		ps.pdbs = append(ps.pdbs, pi)
	}

	return ps, nil
}

// CanEvictPods returns true if every pod in the list is evictable. They may not all be evictable simultaneously, but
// for every PDB that controls the pods at least one pod can be evicted.
func (s *PDBLimits) CanEvictPods(pods []*v1.Pod) (client.ObjectKey, bool) {
	for _, pod := range pods {
		for _, pdb := range s.pdbs {
			if pdb.name.Namespace == pod.ObjectMeta.Namespace {
				if pdb.selector.Matches(labels.Set(pod.Labels)) {

					// if the PDB policy is set to allow evicting unhealthy pods, then it won't stop us from
					// evicting unhealthy pods
					ignorePod := false
					if pdb.canAlwaysEvictUnhealthyPods {
						for _, c := range pod.Status.Conditions {
							if c.Type == v1.PodReady && c.Status == v1.ConditionFalse {
								ignorePod = true
								continue
							}
						}
					}

					if !ignorePod && pdb.disruptionsAllowed == 0 {
						return pdb.name, false
					}
				}
			}
		}
	}
	return client.ObjectKey{}, true
}

type pdbItem struct {
	name                        client.ObjectKey
	selector                    labels.Selector
	disruptionsAllowed          int32
	canAlwaysEvictUnhealthyPods bool
}

func newPdb(pdb policyv1.PodDisruptionBudget) (*pdbItem, error) {
	selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
	if err != nil {
		return nil, err
	}
	canAlwaysEvictUnhealthyPods := false

	if pdb.Spec.UnhealthyPodEvictionPolicy != nil && *pdb.Spec.UnhealthyPodEvictionPolicy == policyv1.AlwaysAllow {
		canAlwaysEvictUnhealthyPods = true
	}
	return &pdbItem{
		name:                        client.ObjectKeyFromObject(&pdb),
		selector:                    selector,
		disruptionsAllowed:          pdb.Status.DisruptionsAllowed,
		canAlwaysEvictUnhealthyPods: canAlwaysEvictUnhealthyPods,
	}, nil
}

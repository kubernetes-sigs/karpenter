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

package dynamicresources

import (
	"unique"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

// PoolKey uniquely identifies a device pool by driver and pool name.
type PoolKey struct {
	Driver DriverID
	Pool   PoolID
}

// DeviceWithID pairs a device with its globally unique identifier.
type DeviceWithID struct {
	cloudprovider.Device
	ID DeviceID
}

// Pool represents a group of in-cluster ResourceSlices that share a driver and pool name.
// Pools are built from published ResourceSlices on the API server. Instance-type-specific
// device templates are not tracked here — they are provided separately by the NodeClaim
// interface during allocation.
type Pool struct {
	Key PoolKey
	// Slices contains the ResourceSlices that contribute to this pool. All slices
	// belong to the same generation (the highest observed).
	Slices []ResourceSlice
	// Devices is the flattened list of devices across all slices in this pool.
	Devices []DeviceWithID
	// Incomplete is true when the number of observed slices for the current generation
	// is less than the pool's declared ResourceSliceCount.
	Incomplete bool
	// Invalid is true when the pool has duplicate device names across its slices.
	// Invalid pools are skipped during allocation.
	Invalid bool
}

// GatherPools builds the set of in-cluster device pools from published ResourceSlices.
// Pools are validated for duplicate device names. Slices are filtered by node affinity
// compatibility with the NodeClaim's requirements.
//
// All slices are fed into the pool builder so that generation tracking and completeness
// checks consider the full pool across all nodes. Only slices whose node selectors match
// the requirements contribute devices to the returned pool. This mirrors the upstream
// scheduler behavior where completeness is determined globally but device visibility is
// scoped to the current node.
//
// inClusterSlices are pre-filtered ResourceSlices from the API server (deleting nodes
// already excluded by the caller).
func GatherPools(
	inClusterSlices []ResourceSlice,
	requirements scheduling.Requirements,
) []*Pool {
	builders := map[PoolKey]*poolBuilder{}

	for _, s := range inClusterSlices {
		matched := sliceMatchesRequirements(s, requirements)
		key := PoolKey{Driver: s.Driver(), Pool: s.Pool().Name}
		b, ok := builders[key]
		if !ok {
			b = &poolBuilder{}
			builders[key] = b
		}
		b.addSlice(s, matched)
	}

	pools := make([]*Pool, 0, len(builders))
	for key, b := range builders {
		if p := b.build(key); p != nil {
			pools = append(pools, p)
		}
	}
	return pools
}

// FilterPools returns the subset of pools that are still compatible with the given requirements,
// narrowing each pool's slices and devices to only those from matching slices. Pools with no
// matching slices after filtering are dropped entirely. This is used for incremental cache
// narrowing — the cached superset is re-filtered against tightened requirements without
// rebuilding from scratch.
func FilterPools(pools []*Pool, requirements scheduling.Requirements) []*Pool {
	var filtered []*Pool
	for _, pool := range pools {
		if p := filterPool(pool, requirements); p != nil {
			filtered = append(filtered, p)
		}
	}
	return filtered
}

// filterPool returns a copy of the pool containing only slices (and their devices) that match
// the requirements. Returns nil if no slices match.
func filterPool(pool *Pool, requirements scheduling.Requirements) *Pool {
	p := &Pool{
		Key:        pool.Key,
		Incomplete: pool.Incomplete,
		Invalid:    pool.Invalid,
	}
	for _, s := range pool.Slices {
		if !sliceMatchesRequirements(s, requirements) {
			continue
		}
		p.Slices = append(p.Slices, s)
		for _, d := range s.Devices() {
			p.Devices = append(p.Devices, DeviceWithID{
				Device: d,
				ID: DeviceID{
					Driver: pool.Key.Driver,
					Pool:   pool.Key.Pool,
					Device: d.Name,
				},
			})
		}
	}
	if len(p.Slices) == 0 {
		return nil
	}
	return p
}

// sliceMatchesRequirements checks whether a ResourceSlice's node affinity is compatible with
// the NodeClaim's requirements. Only in-cluster (non-potential) slices are supported; potential
// slices indicate a programming error.
func sliceMatchesRequirements(s ResourceSlice, requirements scheduling.Requirements) bool {
	if s.Potential() {
		panic("potential slices must not be passed to pool gathering or filtering")
	}
	if s.AllNodes() {
		return true
	}
	if ns := s.NodeSelector(); ns != nil {
		return nodeSelectorsMatch(ns, requirements)
	}
	return false
}

// nodeSelectorsMatch checks if any term in the NodeSelector is compatible with the requirements.
// Terms are OR'd; within a term, match expressions are AND'd.
func nodeSelectorsMatch(ns *corev1.NodeSelector, requirements scheduling.Requirements) bool {
	for _, term := range ns.NodeSelectorTerms {
		termReqs := scheduling.NewNodeSelectorRequirements(term.MatchExpressions...)
		if requirements.IsCompatible(termReqs, scheduling.AllowUndefinedWellKnownLabels) {
			return true
		}
	}
	return false
}


// sliceEntry pairs a ResourceSlice with whether it matched the node requirements.
type sliceEntry struct {
	slice   ResourceSlice
	matched bool
}

// poolBuilder accumulates slices for a pool during gathering, tracking the highest
// generation seen. All slices (matching and non-matching) are tracked for correct
// generation handling and completeness checks. Only matching slices contribute devices.
type poolBuilder struct {
	entries            []sliceEntry
	generation         int64
	resourceSliceCount int64
}

// addSlice adds a slice to the builder, handling generation supersession.
// Slices from older generations are discarded. When a newer generation arrives,
// all previously accumulated slices are replaced. The matched flag records whether
// the slice's node selector is compatible with the current requirements.
func (b *poolBuilder) addSlice(s ResourceSlice, matched bool) {
	gen := s.Generation()
	if len(b.entries) == 0 {
		b.entries = append(b.entries, sliceEntry{s, matched})
		b.generation = gen
		b.resourceSliceCount = s.ResourceSliceCount()
		return
	}
	if gen < b.generation {
		// Outdated slice — discard.
		return
	}
	if gen > b.generation {
		// Newer generation — replace all existing slices.
		b.entries = []sliceEntry{{s, matched}}
		b.generation = gen
		b.resourceSliceCount = s.ResourceSliceCount()
		return
	}
	// Same generation — append.
	b.entries = append(b.entries, sliceEntry{s, matched})
}

// build constructs the Pool from accumulated slices. Completeness is determined by
// the total number of slices at the current generation (matching + non-matching),
// matching the upstream behavior where completeness is a global pool property.
// Only slices whose node selectors matched contribute devices. Returns nil if no
// slices matched (the pool has no devices visible to this node).
func (b *poolBuilder) build(key PoolKey) *Pool {
	pool := &Pool{
		Key: key,
	}

	// Check completeness against ALL slices at this generation.
	if int64(len(b.entries)) != b.resourceSliceCount {
		pool.Incomplete = true
	}

	// Flatten devices only from matching slices; check for duplicates.
	seen := sets.New[unique.Handle[string]]()
	for _, e := range b.entries {
		if !e.matched {
			continue
		}
		pool.Slices = append(pool.Slices, e.slice)
		for _, d := range e.slice.Devices() {
			if seen.Has(d.Name) {
				pool.Invalid = true
			}
			seen.Insert(d.Name)
			pool.Devices = append(pool.Devices, DeviceWithID{
				Device: d,
				ID: DeviceID{
					Driver: key.Driver,
					Pool:   key.Pool,
					Device: d.Name,
				},
			})
		}
	}

	// No matching slices — pool has no visible devices for this node.
	if len(pool.Slices) == 0 {
		return nil
	}

	return pool
}

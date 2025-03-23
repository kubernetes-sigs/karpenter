package scheduling

import (
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

// SimulationCache is used specifically for consolidation as a method to cache
// expensive calculations across simulation runs. The methods are written so that they
// perform correctly (though without caching), if the cache object is nil.
type SimulationCache struct {
	stateNodeLabelRequirements *scheduling.RequirementsReadOnly
}

func NewSimulationCache() *SimulationCache {
	return &SimulationCache{}
}

// StateNodeLabelRequirements returns the scheduling requirements for the state nodes labels. This is safe to cache
// as we don't modify these requirements and the state nodes won't change during a consolidation pass.
func (c *SimulationCache) StateNodeLabelRequirements(n *state.StateNode) scheduling.RequirementsReadOnly {
	if c == nil {
		return scheduling.NewLabelRequirements(n.Node.Labels)
	}
	if c.stateNodeLabelRequirements != nil {
		return *c.stateNodeLabelRequirements
	}
	reqs := scheduling.RequirementsReadOnly(scheduling.NewLabelRequirements(n.Node.Labels))
	c.stateNodeLabelRequirements = &reqs
	return reqs
}

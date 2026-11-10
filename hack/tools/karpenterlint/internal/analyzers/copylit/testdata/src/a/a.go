package a

import "sync"

type Offering struct {
	Price            float64
	Available        bool
	Zone             string
	CapacityOverride map[string]string
	OverheadOverride map[string]string
}

// Shape of the price-overlay copy that drops the override fields.
func applyPrice(offering *Offering, price float64) *Offering {
	return &Offering{ // want `Offering is copied field by field from offering but drops CapacityOverride, OverheadOverride`
		Price:     price,
		Available: offering.Available,
		Zone:      offering.Zone,
	}
}

func applyPriceFixed(offering *Offering, price float64) *Offering {
	return &Offering{
		Price:            price,
		Available:        offering.Available,
		Zone:             offering.Zone,
		CapacityOverride: offering.CapacityOverride,
		OverheadOverride: offering.OverheadOverride,
	}
}

func assignedAfter(offering Offering) Offering {
	out := Offering{
		Available: offering.Available,
		Zone:      offering.Zone,
		Price:     1,
	}
	out.CapacityOverride = offering.CapacityOverride
	out.OverheadOverride = offering.OverheadOverride
	return out
}

func intentional(offering Offering) Offering {
	//karpenterlint:partial-copy only the identity fields matter for the cache key
	return Offering{Available: offering.Available, Zone: offering.Zone}
}

func oneField(offering Offering) Offering {
	return Offering{Zone: offering.Zone}
}

type StateNode struct {
	mu                 sync.Mutex
	Name               string
	Labels             map[string]string
	podDisruptionCosts map[string]float64
}

func refresh(old *StateNode) *StateNode {
	return &StateNode{ // want `StateNode is copied field by field from old but drops podDisruptionCosts`
		Name:   old.Name,
		Labels: old.Labels,
	}
}

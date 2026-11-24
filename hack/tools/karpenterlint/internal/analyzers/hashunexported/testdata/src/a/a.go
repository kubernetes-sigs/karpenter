package a

import (
	"time"

	"github.com/mitchellh/hashstructure/v2"
)

type Requirement struct {
	Key        string
	complement bool
	values     map[string]struct{}
}

type Requirements map[string]*Requirement

type TopologyNodeFilter struct {
	Requirements []Requirements
}

// Shape of TopologyGroup.Hash.
func topologyHash(key string, filter TopologyNodeFilter) uint64 {
	h, _ := hashstructure.Hash(struct { // want `ignores unexported fields Requirement.complement, Requirement.values`
		Key    string
		Filter TopologyNodeFilter
	}{key, filter}, hashstructure.FormatV2, &hashstructure.HashOptions{SlicesAsSets: true})
	return h
}

type Template struct {
	Labels   map[string]string
	Created  time.Time
	internal string        `hash:"ignore"`
	Budgets  []Requirement `hash:"ignore"`
}

func templateHash(t Template) uint64 {
	h, _ := hashstructure.Hash(t, hashstructure.FormatV2, nil)
	return h
}

type Custom struct{ secret string }

func (c Custom) Hash() (uint64, error) { return 0, nil }

func customHash(c []Custom, v any) uint64 {
	h, _ := hashstructure.Hash(c, hashstructure.FormatV2, nil)
	h2, _ := hashstructure.Hash(v, hashstructure.FormatV2, nil)
	return h + h2
}

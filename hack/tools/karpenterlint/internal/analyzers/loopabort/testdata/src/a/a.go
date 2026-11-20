package a

import (
	"context"
	"errors"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
)

type Command struct{}

func reserve(np *v1.NodePool) error { return errors.New("no") }

// Shape of kubernetes-sigs/karpenter#3398 static drift.
func staticDrift(nodePools []*v1.NodePool) ([]Command, error) {
	for _, np := range nodePools {
		if err := reserve(np); err != nil {
			return []Command{}, err // want `error return inside a loop over NodePool`
		}
	}
	return nil, nil
}

// Shape of kubernetes-sigs/karpenter#3398 repair.
func repair(byPool map[string][]*disruption.Candidate) error {
	for _, cands := range byPool {
		for range cands {
			return errors.New("x") // want `error return inside a loop over Candidate`
		}
	}
	return nil
}

func values(nps []v1.NodePool, nodes []*state.StateNode) error {
	for range nps {
		return errors.New("x") // want `error return inside a loop over NodePool`
	}
	for range nodes {
		return errors.New("x") // want `error return inside a loop over StateNode`
	}
	return nil
}

// Fixed shape: skip the failing item.
func fixed(nodePools []*v1.NodePool) []Command {
	var cmds []Command
	for _, np := range nodePools {
		if err := reserve(np); err != nil {
			continue
		}
		cmds = append(cmds, Command{})
	}
	return cmds
}

func cancelled(ctx context.Context, nodePools []*v1.NodePool) error {
	for range nodePools {
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return nil
}

func nested(nodePools []*v1.NodePool) {
	for range nodePools {
		_ = func() error { return errors.New("x") }
	}
}

func otherLoop(names []string) error {
	for range names {
		return errors.New("x")
	}
	return nil
}

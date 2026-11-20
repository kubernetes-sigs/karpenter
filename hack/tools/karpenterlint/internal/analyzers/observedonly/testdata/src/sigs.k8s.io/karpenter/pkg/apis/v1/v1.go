package v1

import "github.com/awslabs/operatorpkg/status"

type NodeClaim struct{ Name string }

func (in *NodeClaim) StatusConditions(opts ...status.ForOption) status.ConditionSet {
	return status.ConditionSet{}
}

func (in *NodeClaim) GetName() string { return in.Name }

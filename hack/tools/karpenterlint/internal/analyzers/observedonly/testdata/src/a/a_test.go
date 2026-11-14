package a

import v1 "sigs.k8s.io/karpenter/pkg/apis/v1"

func fixtureReady(nc *v1.NodeClaim) bool {
	return nc.StatusConditions().IsTrue("Ready")
}

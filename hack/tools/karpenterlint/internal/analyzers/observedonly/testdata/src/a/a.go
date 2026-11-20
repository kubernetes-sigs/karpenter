package a

import (
	"github.com/awslabs/operatorpkg/status"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// Shape of kubernetes-sigs/karpenter#3372.
var rebootPredicate = predicate.NewPredicateFuncs(func(o client.Object) bool {
	nc, ok := o.(*v1.NodeClaim)
	return ok && nc.StatusConditions().Get("Rebooting") != nil // want `in a predicate initializes conditions`
})

var updatePredicate = predicate.Funcs{
	UpdateFunc: func(e predicate.UpdateEvent) bool {
		nc := e.ObjectNew.(*v1.NodeClaim)
		_ = nc.StatusConditions().SetTrue("X") // want `in a predicate writes to the shared cached object`
		return true
	},
}

var fixedPredicate = predicate.NewPredicateFuncs(func(o client.Object) bool {
	nc, ok := o.(*v1.NodeClaim)
	return ok && nc.StatusConditions(status.WithObservedOnly()).Get("Rebooting") != nil
})

func readOnly(nc *v1.NodeClaim) bool {
	return nc.StatusConditions().IsTrue("Ready") // want `StatusConditions\(\).IsTrue only reads`
}

func readOnlyRoot(nc *v1.NodeClaim) bool {
	return nc.StatusConditions().Root() != nil // want `StatusConditions\(\).Root only reads`
}

// The owning controller reads and then writes the same object, so initializing is intended.
func ownerReconcile(nc *v1.NodeClaim) {
	if !nc.StatusConditions().IsTrue("Ready") {
		nc.StatusConditions().SetTrue("Ready")
	}
}

func observed(nc *v1.NodeClaim) bool {
	return nc.StatusConditions(status.WithObservedOnly()).IsTrue("Ready")
}

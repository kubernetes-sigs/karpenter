package predicate

import "sigs.k8s.io/controller-runtime/pkg/client"

type UpdateEvent struct{ ObjectOld, ObjectNew client.Object }

type Funcs struct {
	UpdateFunc func(UpdateEvent) bool
}

func NewPredicateFuncs(filter func(object client.Object) bool) Funcs { return Funcs{} }

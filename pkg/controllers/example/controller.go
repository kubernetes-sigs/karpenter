package example

import (
	"context"

	"github.com/aws/karpenter-core/pkg/operator"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type Example client.Object

type Controller struct {}

func NewController(ctx operator.Context) reconcile.Reconciler {
	return operator.NewControllerFor[Example](ctx, &Controller{})
}

func (n *Controller) Reconcile(ctx context.Context, example Example) (reconcile.Result, error) {
	return reconcile.Result{}, nil
}

func (n *Controller) Finalize(ctx context.Context, example Example) (reconcile.Result, error) {
	return reconcile.Result{}, nil
}

func (n *Controller) Register(ctx context.Context, b *builder.Builder) *builder.Builder {
	return b.WithOptions(controller.Options{})
}

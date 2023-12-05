/*
Copyright 2023 The Kubernetes Authors.

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

package controller

import (
	"context"

	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type Reconciler interface {
	reconcile.Reconciler
	// Name is the name of the Reconciler for metrics and logging
	Name() string
}

// Controller defines a controller that can be registered with controller-runtime
type Controller interface {
	Reconciler

	// Builder returns a Builder registered with the manager that can be wrapped
	// with other Builders and completed later to complete registration to the manager
	Builder(context.Context, manager.Manager) Builder
}

// Builder is a struct, that when complete, registers the passed reconciler with the manager stored
// insider of the builder. Typed reference implementations, see controllerruntime.Builder
type Builder interface {
	// Complete builds a builder by registering the Reconciler with the manager
	Complete(Reconciler) error
}

// Adapter adapts a controllerruntime.Builder into the Builder interface
type Adapter struct {
	builder *controllerruntime.Builder
}

func Adapt(builder *controllerruntime.Builder) Builder {
	return &Adapter{
		builder: builder,
	}
}

func (a *Adapter) Complete(r Reconciler) error {
	a.builder = a.builder.Named(r.Name())
	return a.builder.Complete(r)
}

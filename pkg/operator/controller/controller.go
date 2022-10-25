/*
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
	"net/http"

	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// Controller defines a controller that can be registered with controller-runtime
type Controller interface {
	reconcile.Reconciler

	// Builder returns a Builder registered with the manager that can be wrapped
	// with other Builders and completed later to complete registration to the manager
	Builder(context.Context, manager.Manager) Builder

	// LivenessProbe surfaces a health check to run on the controller
	LivenessProbe(req *http.Request) error
}

// Builder is a struct, that when complete, registers the passed reconciler with the manager stored
// insider of the builder. For reference implementations, see controllerruntime.Builder
type Builder interface {
	// Complete builds a builder by registering the Reconciler with the manager
	Complete(reconcile.Reconciler) error
}

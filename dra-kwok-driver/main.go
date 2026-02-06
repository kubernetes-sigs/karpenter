/*
Copyright The Kubernetes Authors.

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

package main

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	// Import DRA API types
	resourcev1 "k8s.io/api/resource/v1"

	// Import our CRD API types
	"sigs.k8s.io/karpenter/dra-kwok-driver/pkg/apis/v1alpha1"

	// Import our controllers
	"sigs.k8s.io/karpenter/dra-kwok-driver/pkg/controllers"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(resourcev1.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme)) // Register our CRD types
}

func main() {
	// Setup logging
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	ctx := context.Background()
	logger := ctrl.Log.WithName("dra-kwok-driver")

	logger.Info("Starting DRA KWOK Driver (One CRD per driver)")

	// Create manager
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: ":8082",
		},
		HealthProbeBindAddress: ":8083",
	})
	if err != nil {
		logger.Error(err, "unable to start manager")
		panic(err)
	}

	// Add health checks
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		logger.Error(err, "unable to set up health check")
		panic(err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		logger.Error(err, "unable to set up ready check")
		panic(err)
	}

	// Initialize ResourceSlice controller (single controller, no config store)
	// CRD name must match driver name (test.karpenter.sh)
	resourceSliceController := controllers.NewResourceSliceController(
		mgr.GetClient(),
		"test.karpenter.sh",
		"karpenter",
	)

	// Register controller
	logger.Info("Registering ResourceSlice controller")
	if err := resourceSliceController.Register(ctx, mgr); err != nil {
		logger.Error(err, "unable to register resourceslice controller")
		panic(err)
	}

	// Start manager
	logger.Info("Starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		logger.Error(err, "problem running manager")
		panic(err)
	}
}

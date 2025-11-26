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

	// Import our controllers and config
	"sigs.k8s.io/karpenter/dra-kwok-driver/pkg/config"
	"sigs.k8s.io/karpenter/dra-kwok-driver/pkg/controllers"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(resourcev1.AddToScheme(scheme))
}

func main() {
	// Setup logging
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	ctx := context.Background()
	logger := ctrl.Log.WithName("dra-kwok-driver")

	logger.Info("Starting DRA KWOK Driver")

	// Create simple manager
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: ":8080",
		},
		HealthProbeBindAddress: ":8081",
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

	// Initialize ResourceSlice controller first
	resourceSliceController := controllers.NewResourceSliceController(
		mgr.GetClient(),
		"karpenter.sh.dra-kwok-driver",
		nil, // Will be set after ConfigMap controller is created
	)

	// Initialize ConfigMap controller with callback to trigger ResourceSlice reconciliation
	configMapController := controllers.NewConfigMapController(
		mgr.GetClient(),
		"dra-kwok-configmap",
		"karpenter",
		func(cfg *config.Config) {
			if cfg != nil {
				logger.Info("Configuration updated, reconciling all nodes", "driver", cfg.Driver, "mappings", len(cfg.Mappings))
				if err := resourceSliceController.ReconcileAllNodes(ctx); err != nil {
					logger.Error(err, "Failed to reconcile nodes after configuration change")
				}
			} else {
				logger.Info("Configuration cleared")
			}
		},
	)

	// Update ResourceSlice controller with ConfigMap controller reference
	resourceSliceController.SetConfigController(configMapController)

	// Register controllers
	logger.Info("Registering controllers")
	if err := configMapController.Register(ctx, mgr); err != nil {
		logger.Error(err, "unable to register configmap controller")
		panic(err)
	}

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

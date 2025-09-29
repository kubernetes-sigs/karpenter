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

package controllers

import (
	"context"
	"fmt"

	"github.com/awslabs/operatorpkg/serrors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	"sigs.k8s.io/karpenter/dra-kwok-driver/pkg/config"
)

// ConfigMapController watches for ConfigMap changes and updates the driver configuration
type ConfigMapController struct {
	kubeClient         client.Client
	configMapName      string
	configMapNamespace string
	driverConfig       *config.Config
	onConfigChange     func(*config.Config)
}

// NewConfigMapController creates a new ConfigMap controller
func NewConfigMapController(kubeClient client.Client, configMapName, configMapNamespace string, onConfigChange func(*config.Config)) *ConfigMapController {
	return &ConfigMapController{
		kubeClient:         kubeClient,
		configMapName:      configMapName,
		configMapNamespace: configMapNamespace,
		onConfigChange:     onConfigChange,
	}
}

// Register registers the controller with the manager
func (r *ConfigMapController) Register(ctx context.Context, mgr manager.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("configmap").
		For(&corev1.ConfigMap{}).
		WithEventFilter(predicate.NewPredicateFuncs(func(obj client.Object) bool {
			// Only watch our specific ConfigMap
			return obj.GetName() == r.configMapName && obj.GetNamespace() == r.configMapNamespace
		})).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: 1, // Single threaded to avoid config race conditions
		}).
		Complete(r)
}

// Reconcile handles ConfigMap changes
func (r *ConfigMapController) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	log.V(1).Info("reconciling configmap", "name", req.Name, "namespace", req.Namespace)

	// Fetch the ConfigMap
	configMap := &corev1.ConfigMap{}
	if err := r.kubeClient.Get(ctx, req.NamespacedName, configMap); err != nil {
		if errors.IsNotFound(err) {
			log.Info("configmap not found, using empty configuration")
			// Clear configuration when ConfigMap is deleted
			if r.onConfigChange != nil {
				r.onConfigChange(nil)
			}
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, serrors.Wrap(fmt.Errorf("getting configmap, %w", err), "ConfigMap", klog.KRef(req.Namespace, req.Name))
	}

	// Parse configuration from ConfigMap
	cfg, err := r.parseConfigFromConfigMap(configMap)
	if err != nil {
		log.Error(err, "failed to parse configuration from configmap")
		// Don't return error to avoid constant requeuing of invalid config
		return reconcile.Result{}, nil
	}

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		log.Error(err, "invalid configuration in configmap")
		// Don't return error to avoid constant requeuing of invalid config
		return reconcile.Result{}, nil
	}

	log.Info("loaded configuration from configmap",
		"driver", cfg.Driver,
		"mappings", len(cfg.Mappings),
	)

	// Store configuration and notify callback
	r.driverConfig = cfg
	if r.onConfigChange != nil {
		r.onConfigChange(cfg)
	}

	return reconcile.Result{}, nil
}

// parseConfigFromConfigMap extracts configuration from the ConfigMap and sanitizes it
func (r *ConfigMapController) parseConfigFromConfigMap(cm *corev1.ConfigMap) (*config.Config, error) {
	// Get configuration from config.yaml key (as defined in tests)
	configData, exists := cm.Data["config.yaml"]
	if !exists {
		return nil, fmt.Errorf("no configuration found in configmap, expected key: config.yaml")
	}

	// Parse YAML configuration
	cfg := &config.Config{}
	if err := yaml.Unmarshal([]byte(configData), cfg); err != nil {
		return nil, fmt.Errorf("failed to parse YAML configuration: %w", err)
	}

	// Sanitize configuration to ensure Kubernetes compliance
	r.sanitizeConfig(cfg)

	return cfg, nil
}

// sanitizeConfig modifies the configuration to ensure all values are Kubernetes-compliant
func (r *ConfigMapController) sanitizeConfig(cfg *config.Config) {
	log := ctrl.Log.WithName("configmap-sanitizer")

	// Sanitize driver name
	originalDriverName := cfg.Driver
	cfg.Driver = config.SanitizeDriverName(cfg.Driver)
	if originalDriverName != cfg.Driver {
		log.Info("sanitized driver name", "original", originalDriverName, "sanitized", cfg.Driver)
	}

	// Device attribute sanitization removed - using upstream ResourceSlice types directly
	// Kubernetes API server validation will handle attribute name validation
}

// GetConfig returns the current configuration
func (r *ConfigMapController) GetConfig() *config.Config {
	return r.driverConfig
}

// LoadInitialConfig loads the initial configuration from the ConfigMap
func (r *ConfigMapController) LoadInitialConfig(ctx context.Context) error {
	log := klog.FromContext(ctx)

	configMap := &corev1.ConfigMap{}
	key := types.NamespacedName{
		Name:      r.configMapName,
		Namespace: r.configMapNamespace,
	}

	if err := r.kubeClient.Get(ctx, key, configMap); err != nil {
		if errors.IsNotFound(err) {
			log.Info("configmap not found during initial load, will wait for it to be created",
				"name", r.configMapName,
				"namespace", r.configMapNamespace,
			)
			return nil
		}
		return fmt.Errorf("failed to load initial configmap: %w", err)
	}

	// Parse and validate initial configuration
	cfg, err := r.parseConfigFromConfigMap(configMap)
	if err != nil {
		return fmt.Errorf("failed to parse initial configuration: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid initial configuration: %w", err)
	}

	r.driverConfig = cfg
	log.Info("loaded initial configuration",
		"driver", cfg.Driver,
		"mappings", len(cfg.Mappings),
	)

	return nil
}

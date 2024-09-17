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

package operator

import (
	"context"
	"fmt"
	"net/http"
	"net/http/pprof"
	"runtime"
	"runtime/debug"
	"sync"

	"github.com/awslabs/operatorpkg/controller"
	opmetrics "github.com/awslabs/operatorpkg/metrics"
	"github.com/go-logr/zapr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/samber/lo"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/util/flowcontrol"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	"sigs.k8s.io/karpenter/pkg/operator/logging"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/utils/env"
)

const (
	appName   = "karpenter"
	component = "controller"
)

var (
	BuildInfo = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Name:      "build_info",
			Help:      "A metric with a constant '1' value labeled by version from which karpenter was built.",
		},
		[]string{"version", "goversion", "goarch", "commit"},
	)
)

// Version is the karpenter app version injected during compilation
// when using the Makefile
var Version = "unspecified"

func init() {
	crmetrics.Registry.MustRegister(BuildInfo)
	opmetrics.RegisterClientMetrics(crmetrics.Registry)

	BuildInfo.WithLabelValues(Version, runtime.Version(), runtime.GOARCH, env.GetRevision()).Set(1)
}

type Operator struct {
	manager.Manager

	KubernetesInterface kubernetes.Interface
	EventRecorder       events.Recorder
	Clock               clock.Clock
}

// NewOperator instantiates a controller manager or panics
func NewOperator() (context.Context, *Operator) {
	// Root Context
	ctx := context.Background()

	// Options
	ctx = injection.WithOptionsOrDie(ctx, options.Injectables...)

	// Make the Karpenter binary aware of the container memory limit
	// https://pkg.go.dev/runtime/debug#SetMemoryLimit
	if options.FromContext(ctx).MemoryLimit > 0 {
		newLimit := int64(float64(options.FromContext(ctx).MemoryLimit) * 0.9)
		debug.SetMemoryLimit(newLimit)
	}

	// Logging
	logger := zapr.NewLogger(logging.NewLogger(ctx, component))
	log.SetLogger(logger)
	klog.SetLogger(logger)

	// Client Config
	config := ctrl.GetConfigOrDie()
	config.RateLimiter = flowcontrol.NewTokenBucketRateLimiter(float32(options.FromContext(ctx).KubeClientQPS), options.FromContext(ctx).KubeClientBurst)
	config.UserAgent = fmt.Sprintf("%s/%s", appName, Version)

	// Client
	kubernetesInterface := kubernetes.NewForConfigOrDie(config)

	log.FromContext(ctx).WithValues("version", Version).V(1).Info("discovered karpenter version")

	// Manager
	mgrOpts := ctrl.Options{
		Logger:                        logging.IgnoreDebugEvents(logger),
		LeaderElection:                !options.FromContext(ctx).DisableLeaderElection,
		LeaderElectionID:              "karpenter-leader-election",
		LeaderElectionResourceLock:    resourcelock.LeasesResourceLock,
		LeaderElectionReleaseOnCancel: true,
		Metrics: server.Options{
			BindAddress: fmt.Sprintf(":%d", options.FromContext(ctx).MetricsPort),
		},
		HealthProbeBindAddress: fmt.Sprintf(":%d", options.FromContext(ctx).HealthProbePort),
		BaseContext: func() context.Context {
			ctx := log.IntoContext(context.Background(), logger)
			ctx = injection.WithOptionsOrDie(ctx, options.Injectables...)
			return ctx
		},
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&coordinationv1.Lease{}: {
					Field: fields.SelectorFromSet(fields.Set{"metadata.namespace": "kube-node-lease"}),
				},
			},
		},
	}
	if options.FromContext(ctx).EnableProfiling {
		// TODO @joinnis: Investigate the mgrOpts.PprofBindAddress that would allow native support for pprof
		// On initial look, it seems like this native pprof doesn't support some of the routes that we have here
		// like "/debug/pprof/heap" or "/debug/pprof/block"
		mgrOpts.Metrics.ExtraHandlers = lo.Assign(mgrOpts.Metrics.ExtraHandlers, map[string]http.Handler{
			"/debug/pprof/":             http.HandlerFunc(pprof.Index),
			"/debug/pprof/cmdline":      http.HandlerFunc(pprof.Cmdline),
			"/debug/pprof/profile":      http.HandlerFunc(pprof.Profile),
			"/debug/pprof/symbol":       http.HandlerFunc(pprof.Symbol),
			"/debug/pprof/trace":        http.HandlerFunc(pprof.Trace),
			"/debug/pprof/allocs":       pprof.Handler("allocs"),
			"/debug/pprof/heap":         pprof.Handler("heap"),
			"/debug/pprof/block":        pprof.Handler("block"),
			"/debug/pprof/goroutine":    pprof.Handler("goroutine"),
			"/debug/pprof/threadcreate": pprof.Handler("threadcreate"),
		})
	}
	mgr, err := ctrl.NewManager(config, mgrOpts)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to setup manager")
	}
	err = mgr.GetFieldIndexer().IndexField(ctx, &corev1.Pod{}, "spec.nodeName", func(o client.Object) []string {
		return []string{o.(*corev1.Pod).Spec.NodeName}
	})
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to setup pod indexer")
	}
	err = mgr.GetFieldIndexer().IndexField(ctx, &corev1.Node{}, "spec.providerID", func(o client.Object) []string {
		return []string{o.(*corev1.Node).Spec.ProviderID}
	})
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to setup node provider id indexer")
	}
	err = mgr.GetFieldIndexer().IndexField(ctx, &v1.NodeClaim{}, "status.providerID", func(o client.Object) []string {
		return []string{o.(*v1.NodeClaim).Status.ProviderID}
	})
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to setup nodeclaim provider id indexer")
	}
	err = mgr.GetFieldIndexer().IndexField(ctx, &v1.NodeClaim{}, "spec.nodeClassRef.group", func(o client.Object) []string {
		return []string{o.(*v1.NodeClaim).Spec.NodeClassRef.Group}
	})
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to setup nodeclaim nodeclassref apiversion indexer")
	}
	err = mgr.GetFieldIndexer().IndexField(ctx, &v1.NodeClaim{}, "spec.nodeClassRef.kind", func(o client.Object) []string {
		return []string{o.(*v1.NodeClaim).Spec.NodeClassRef.Kind}
	})
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to setup nodeclaim nodeclassref kind indexer")
	}
	err = mgr.GetFieldIndexer().IndexField(ctx, &v1.NodeClaim{}, "spec.nodeClassRef.name", func(o client.Object) []string {
		return []string{o.(*v1.NodeClaim).Spec.NodeClassRef.Name}
	})
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to setup nodeclaim nodeclassref name indexer")
	}
	err = mgr.GetFieldIndexer().IndexField(ctx, &v1.NodePool{}, "spec.template.spec.nodeClassRef.group", func(o client.Object) []string {
		return []string{o.(*v1.NodePool).Spec.Template.Spec.NodeClassRef.Group}
	})
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to setup nodepool nodeclassref apiversion indexer")
	}
	err = mgr.GetFieldIndexer().IndexField(ctx, &v1.NodePool{}, "spec.template.spec.nodeClassRef.kind", func(o client.Object) []string {
		return []string{o.(*v1.NodePool).Spec.Template.Spec.NodeClassRef.Kind}
	})
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to setup nodepool nodeclassref kind indexer")
	}
	err = mgr.GetFieldIndexer().IndexField(ctx, &v1.NodePool{}, "spec.template.spec.nodeClassRef.name", func(o client.Object) []string {
		return []string{o.(*v1.NodePool).Spec.Template.Spec.NodeClassRef.Name}
	})
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to setup nodepool nodeclassref name indexer")
	}
	err = mgr.GetFieldIndexer().IndexField(ctx, &storagev1.VolumeAttachment{}, "spec.nodeName", func(o client.Object) []string {
		return []string{o.(*storagev1.VolumeAttachment).Spec.NodeName}
	})
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to setup volumeattachment indexer")
	}

	err = mgr.AddHealthzCheck("healthz", healthz.Ping)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to add healthz check")
	}

	err = mgr.AddReadyzCheck("readyz", healthz.Ping)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to add readyz check")
	}

	return ctx, &Operator{
		Manager:             mgr,
		KubernetesInterface: kubernetesInterface,
		EventRecorder:       events.NewRecorder(mgr.GetEventRecorderFor(appName)),
		Clock:               clock.RealClock{},
	}
}

func (o *Operator) WithControllers(ctx context.Context, controllers ...controller.Controller) *Operator {
	for _, c := range controllers {
		lo.Must0(c.Register(ctx, o.Manager))
	}
	return o
}

func (o *Operator) Start(ctx context.Context) {
	wg := &sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		lo.Must0(o.Manager.Start(ctx))
	}()
	wg.Wait()
}

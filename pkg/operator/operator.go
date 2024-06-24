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
	"time"

	"github.com/awslabs/operatorpkg/controller"
	"github.com/prometheus/client_golang/prometheus"
	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/klog/v2"
	"knative.dev/pkg/changeset"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/metrics"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/util/flowcontrol"
	"k8s.io/utils/clock"
	knativeinjection "knative.dev/pkg/injection"
	"knative.dev/pkg/signals"
	"knative.dev/pkg/system"
	"knative.dev/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/go-logr/zapr"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	"sigs.k8s.io/karpenter/pkg/operator/logging"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/operator/scheme"
	"sigs.k8s.io/karpenter/pkg/webhooks"
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
	BuildInfo.WithLabelValues(Version, runtime.Version(), runtime.GOARCH, changeset.Get()).Set(1)
}

type Operator struct {
	manager.Manager

	KubernetesInterface kubernetes.Interface
	EventRecorder       events.Recorder
	Clock               clock.Clock

	webhooks []knativeinjection.ControllerConstructor
}

// NewOperator instantiates a controller manager or panics
func NewOperator() (context.Context, *Operator) {
	// Root Context
	ctx := signals.NewContext()
	ctx = knativeinjection.WithNamespaceScope(ctx, system.Namespace())

	// Options
	ctx = injection.WithOptionsOrDie(ctx, options.Injectables...)

	// Make the Karpenter binary aware of the container memory limit
	// https://pkg.go.dev/runtime/debug#SetMemoryLimit
	if options.FromContext(ctx).MemoryLimit > 0 {
		newLimit := int64(float64(options.FromContext(ctx).MemoryLimit) * 0.9)
		debug.SetMemoryLimit(newLimit)
	}

	// Webhook
	ctx = webhook.WithOptions(ctx, webhook.Options{
		Port:        options.FromContext(ctx).WebhookPort,
		ServiceName: options.FromContext(ctx).ServiceName,
		SecretName:  fmt.Sprintf("%s-cert", options.FromContext(ctx).ServiceName),
		GracePeriod: 5 * time.Second,
	})

	// Client Config
	config := ctrl.GetConfigOrDie()
	config.RateLimiter = flowcontrol.NewTokenBucketRateLimiter(float32(options.FromContext(ctx).KubeClientQPS), options.FromContext(ctx).KubeClientBurst)
	config.UserAgent = fmt.Sprintf("%s/%s", appName, Version)

	// Client
	kubernetesInterface := kubernetes.NewForConfigOrDie(config)

	// Logging
	logger := zapr.NewLogger(logging.NewLogger(ctx, component))
	log.SetLogger(logger)
	klog.SetLogger(logger)

	log.FromContext(ctx).WithValues("version", Version).V(1).Info("discovered karpenter version")

	// Manager
	mgrOpts := ctrl.Options{
		Logger:                        logging.IgnoreDebugEvents(logger),
		LeaderElection:                options.FromContext(ctx).EnableLeaderElection,
		LeaderElectionID:              "karpenter-leader-election",
		LeaderElectionResourceLock:    resourcelock.LeasesResourceLock,
		LeaderElectionNamespace:       system.Namespace(),
		LeaderElectionReleaseOnCancel: true,
		Scheme:                        scheme.Scheme,
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
	mgr = lo.Must(mgr, err, "failed to setup manager")
	lo.Must0(mgr.GetFieldIndexer().IndexField(ctx, &v1.Pod{}, "spec.nodeName", func(o client.Object) []string {
		return []string{o.(*v1.Pod).Spec.NodeName}
	}), "failed to setup pod indexer")
	lo.Must0(mgr.GetFieldIndexer().IndexField(ctx, &v1.Node{}, "spec.providerID", func(o client.Object) []string {
		return []string{o.(*v1.Node).Spec.ProviderID}
	}), "failed to setup node provider id indexer")
	lo.Must0(mgr.GetFieldIndexer().IndexField(ctx, &v1beta1.NodeClaim{}, "status.providerID", func(o client.Object) []string {
		return []string{o.(*v1beta1.NodeClaim).Status.ProviderID}
	}), "failed to setup nodeclaim provider id indexer")
	lo.Must0(mgr.GetFieldIndexer().IndexField(ctx, &v1beta1.NodeClaim{}, "spec.nodeClassRef.apiVersion", func(o client.Object) []string {
		return []string{o.(*v1beta1.NodeClaim).Spec.NodeClassRef.APIVersion}
	}), "failed to setup nodeclaim nodeclassref apiversion indexer")
	lo.Must0(mgr.GetFieldIndexer().IndexField(ctx, &v1beta1.NodeClaim{}, "spec.nodeClassRef.kind", func(o client.Object) []string {
		return []string{o.(*v1beta1.NodeClaim).Spec.NodeClassRef.Kind}
	}), "failed to setup nodeclaim nodeclassref kind indexer")
	lo.Must0(mgr.GetFieldIndexer().IndexField(ctx, &v1beta1.NodeClaim{}, "spec.nodeClassRef.name", func(o client.Object) []string {
		return []string{o.(*v1beta1.NodeClaim).Spec.NodeClassRef.Name}
	}), "failed to setup nodeclaim nodeclassref name indexer")

	lo.Must0(mgr.AddReadyzCheck("manager", func(req *http.Request) error {
		return lo.Ternary(mgr.GetCache().WaitForCacheSync(req.Context()), nil, fmt.Errorf("failed to sync caches"))
	}))
	lo.Must0(mgr.AddHealthzCheck("healthz", healthz.Ping))
	lo.Must0(mgr.AddReadyzCheck("readyz", healthz.Ping))

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

func (o *Operator) WithWebhooks(ctx context.Context, ctors ...knativeinjection.ControllerConstructor) *Operator {
	if !options.FromContext(ctx).DisableWebhook {
		o.webhooks = append(o.webhooks, ctors...)
		lo.Must0(o.Manager.AddReadyzCheck("webhooks", webhooks.HealthProbe(ctx)))
		lo.Must0(o.Manager.AddHealthzCheck("webhooks", webhooks.HealthProbe(ctx)))
	}
	return o
}

func (o *Operator) Start(ctx context.Context, cp cloudprovider.CloudProvider) {
	wg := &sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		lo.Must0(o.Manager.Start(ctx))
	}()
	if options.FromContext(ctx).DisableWebhook {
		log.FromContext(ctx).Info("webhook disabled")
	} else {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Taking the first supported NodeClass to be the default NodeClass
			ctx = injection.NodeClassToContext(ctx, cp.GetSupportedNodeClasses())
			ctx = injection.WithClient(ctx, o.GetClient())
			webhooks.Start(ctx, o.GetConfig(), o.webhooks...)
		}()
	}
	wg.Wait()
}

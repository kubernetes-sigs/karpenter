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

package operator

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/go-logr/zapr"
	"github.com/samber/lo"
	coordinationv1 "k8s.io/api/coordination/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/util/flowcontrol"
	"k8s.io/utils/clock"
	knativeinjection "knative.dev/pkg/injection"
	knativelogging "knative.dev/pkg/logging"
	"knative.dev/pkg/signals"
	"knative.dev/pkg/system"
	"knative.dev/pkg/webhook"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/aws/karpenter-core/pkg/apis"
	"github.com/aws/karpenter-core/pkg/apis/settings"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/events"
	"github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/operator/injection"
	"github.com/aws/karpenter-core/pkg/operator/logging"
	"github.com/aws/karpenter-core/pkg/operator/options"
	"github.com/aws/karpenter-core/pkg/operator/scheme"
	"github.com/aws/karpenter-core/pkg/webhooks"
)

const (
	appName   = "karpenter"
	component = "controller"
)

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
	opts := options.New().MustParse(os.Args[1:]...)
	ctx = options.ToContext(ctx, opts)

	if opts.MemoryLimit > 0 {
		newLimit := int64(float64(opts.MemoryLimit) * 0.9)
		debug.SetMemoryLimit(newLimit)
	}

	// Webhook
	ctx = webhook.WithOptions(ctx, webhook.Options{
		Port:        opts.WebhookPort,
		ServiceName: opts.ServiceName,
		SecretName:  fmt.Sprintf("%s-cert", opts.ServiceName),
		GracePeriod: 5 * time.Second,
	})

	// Client Config
	config := controllerruntime.GetConfigOrDie()
	config.RateLimiter = flowcontrol.NewTokenBucketRateLimiter(float32(opts.KubeClientQPS), opts.KubeClientBurst)
	config.UserAgent = appName

	// Client
	kubernetesInterface := kubernetes.NewForConfigOrDie(config)

	// Logging
	logger := logging.NewLogger(ctx, component, kubernetesInterface)
	ctx = knativelogging.WithLogger(ctx, logger)
	logging.ConfigureGlobalLoggers(ctx)

	// Inject settings from the ConfigMap(s) into the context
	ctx = injection.WithSettingsOrDie(ctx, kubernetesInterface, apis.Settings...)
	opts.MergeSettings(settings.FromContext(ctx))

	// Manager
	mgr, err := controllerruntime.NewManager(config, controllerruntime.Options{
		Logger:                     logging.IgnoreDebugEvents(zapr.NewLogger(logger.Desugar())),
		LeaderElection:             opts.EnableLeaderElection,
		LeaderElectionID:           "karpenter-leader-election",
		LeaderElectionResourceLock: resourcelock.LeasesResourceLock,
		LeaderElectionNamespace:    system.Namespace(),
		Scheme:                     scheme.Scheme,
		MetricsBindAddress:         fmt.Sprintf(":%d", opts.MetricsPort),
		HealthProbeBindAddress:     fmt.Sprintf(":%d", opts.HealthProbePort),
		BaseContext: func() context.Context {
			ctx := context.Background()
			ctx = knativelogging.WithLogger(ctx, logger)
			ctx = options.ToContext(ctx, opts)
			ctx = injection.WithSettingsOrDie(ctx, kubernetesInterface, apis.Settings...)
			return ctx
		},
		NewCache: cache.BuilderWithOptions(cache.Options{
			SelectorsByObject: cache.SelectorsByObject{
				&coordinationv1.Lease{}: {
					Field: fields.SelectorFromSet(fields.Set{"metadata.namespace": "kube-node-lease"}),
				},
			},
		}),
	})
	mgr = lo.Must(mgr, err, "failed to setup manager")
	if opts.EnableProfiling {
		registerPprof(mgr)
	}
	lo.Must0(mgr.GetFieldIndexer().IndexField(ctx, &v1.Pod{}, "spec.nodeName", func(o client.Object) []string {
		return []string{o.(*v1.Pod).Spec.NodeName}
	}), "failed to setup pod indexer")
	lo.Must0(mgr.GetFieldIndexer().IndexField(ctx, &v1.Node{}, "spec.providerID", func(o client.Object) []string {
		return []string{o.(*v1.Node).Spec.ProviderID}
	}), "failed to setup node provider id indexer")
	lo.Must0(mgr.GetFieldIndexer().IndexField(ctx, &v1alpha5.Machine{}, "status.providerID", func(o client.Object) []string {
		return []string{o.(*v1alpha5.Machine).Status.ProviderID}
	}), "failed to setup machine provider id indexer")
	// TODO @joinnis: Add field indexer for NodeClaim .status.providerID

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
		lo.Must0(c.Builder(ctx, o.Manager).Complete(c))
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

func (o *Operator) Start(ctx context.Context) {
	wg := &sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		lo.Must0(o.Manager.Start(ctx))
	}()
	if options.FromContext(ctx).DisableWebhook {
		knativelogging.FromContext(ctx).Infof("webhook disabled")
	} else {
		wg.Add(1)
		go func() {
			defer wg.Done()
			webhooks.Start(ctx, o.GetConfig(), o.KubernetesInterface, o.webhooks...)
		}()
	}
	wg.Wait()
}

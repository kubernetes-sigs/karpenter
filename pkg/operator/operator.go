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
	"io"
	"net/http"
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
	"knative.dev/pkg/configmap/informer"
	knativeinjection "knative.dev/pkg/injection"
	"knative.dev/pkg/injection/sharedmain"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/signals"
	"knative.dev/pkg/system"
	"knative.dev/pkg/webhook"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/aws/karpenter-core/pkg/apis"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/events"
	corecontroller "github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/operator/injection"
	"github.com/aws/karpenter-core/pkg/operator/options"
	"github.com/aws/karpenter-core/pkg/operator/scheme"
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
	// TODO: This can be removed if we eventually decide that we need leader election. Having leader election has resulted in the webhook
	// having issues described in https://github.com/aws/karpenter/issues/2562 so these issues need to be resolved if this line is removed
	ctx = sharedmain.WithHADisabled(ctx) // Disable leader election for webhook

	// Options
	opts := options.New().MustParse()
	ctx = injection.WithOptions(ctx, *opts)

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
	configMapWatcher := informer.NewInformedWatcher(kubernetesInterface, system.Namespace())
	lo.Must0(configMapWatcher.Start(ctx.Done()))

	// Logging
	logger := NewLogger(ctx, component, config, configMapWatcher)
	ctx = logging.WithLogger(ctx, logger)
	ConfigureGlobalLoggers(ctx)

	// Inject settings from the ConfigMap(s) into the context
	ctx = injection.WithSettingsOrDie(ctx, kubernetesInterface, apis.Settings...)

	// Manager
	mgr, err := controllerruntime.NewManager(config, controllerruntime.Options{
		Logger:                     ignoreDebugEvents(zapr.NewLogger(logger.Desugar())),
		LeaderElection:             opts.EnableLeaderElection,
		LeaderElectionID:           "karpenter-leader-election",
		LeaderElectionResourceLock: resourcelock.LeasesResourceLock,
		LeaderElectionNamespace:    system.Namespace(),
		Scheme:                     scheme.Scheme,
		MetricsBindAddress:         fmt.Sprintf(":%d", opts.MetricsPort),
		HealthProbeBindAddress:     fmt.Sprintf(":%d", opts.HealthProbePort),
		BaseContext: func() context.Context {
			ctx := context.Background()
			ctx = logging.WithLogger(ctx, logger)
			ctx = injection.WithSettingsOrDie(ctx, kubernetesInterface, apis.Settings...)
			ctx = injection.WithConfig(ctx, config)
			ctx = injection.WithOptions(ctx, *opts)
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

func (o *Operator) WithControllers(ctx context.Context, controllers ...corecontroller.Controller) *Operator {
	for _, c := range controllers {
		lo.Must0(c.Builder(ctx, o.Manager).Complete(c))
	}
	return o
}

func (o *Operator) WithWebhooks(ctx context.Context, webhooks ...knativeinjection.ControllerConstructor) *Operator {
	if !injection.GetOptions(ctx).DisableWebhook {
		o.webhooks = append(o.webhooks, webhooks...)
		lo.Must0(o.Manager.AddReadyzCheck("webhooks", webhookChecker(ctx)))
		lo.Must0(o.Manager.AddHealthzCheck("webhooks", webhookChecker(ctx)))
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
	if injection.GetOptions(ctx).DisableWebhook {
		logging.FromContext(ctx).Infof("webhook disabled")
	} else {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sharedmain.MainWithConfig(sharedmain.WithHealthProbesDisabled(ctx), "webhook", o.GetConfig(), o.webhooks...)
		}()
	}
	wg.Wait()
}

func webhookChecker(ctx context.Context) healthz.Checker {
	// TODO: Add knative health check port for webhooks when health port can be configured
	// Issue: https://github.com/knative/pkg/issues/2765
	return func(req *http.Request) (err error) {
		res, err := http.Get(fmt.Sprintf("http://localhost:%d", injection.GetOptions(ctx).WebhookPort))
		// If the webhook connection errors out, liveness/readiness should fail
		if err != nil {
			return err
		}
		// Close the body to avoid leaking file descriptors
		// Always read the body so we can re-use the connection: https://stackoverflow.com/questions/17948827/reusing-http-connections-in-go
		_, _ = io.ReadAll(res.Body)
		res.Body.Close()

		// If there is a server-side error or path not found,
		// consider liveness to have failed
		if res.StatusCode >= 500 || res.StatusCode == 404 {
			return fmt.Errorf("webhook probe failed with status code %d", res.StatusCode)
		}
		return nil
	}
}

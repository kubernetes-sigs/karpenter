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

package webhooks

import (
	"context"
	"crypto/tls"
	stderrors "errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/samber/lo"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"knative.dev/pkg/configmap"
	"knative.dev/pkg/controller"
	knativeinjection "knative.dev/pkg/injection"
	"knative.dev/pkg/injection/sharedmain"
	knativelogging "knative.dev/pkg/logging"
	"knative.dev/pkg/metrics"
	"knative.dev/pkg/webhook"
	"knative.dev/pkg/webhook/certificates"
	"knative.dev/pkg/webhook/configmaps"
	"knative.dev/pkg/webhook/resourcesemantics"
	"knative.dev/pkg/webhook/resourcesemantics/conversion"
	"knative.dev/pkg/webhook/resourcesemantics/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	"sigs.k8s.io/karpenter/pkg/operator/logging"
	"sigs.k8s.io/karpenter/pkg/operator/options"
)

const component = "webhook"

var (
	Resources = map[schema.GroupVersionKind]resourcesemantics.GenericCRD{
		v1beta1.SchemeGroupVersion.WithKind("NodePool"):  &v1beta1.NodePool{},
		v1beta1.SchemeGroupVersion.WithKind("NodeClaim"): &v1beta1.NodeClaim{},
	}
)

func NewWebhooks() []knativeinjection.NamedControllerConstructor {
	return []knativeinjection.NamedControllerConstructor{
		{Name: "certificates", ControllerConstructor: certificates.NewController},
		{Name: "validation.webhook.karpenter.sh", ControllerConstructor: NewCRDValidationWebhook},
		{Name: "validation.webhook.config.karpenter.sh", ControllerConstructor: NewConfigValidationWebhook},
		{Name: "conversion.webhook.karpenter.sh", ControllerConstructor: NewCRDConversionWebhook},
	}
}

var (
	ConversionResource = map[schema.GroupKind]conversion.GroupKindConversion{
		v1beta1.SchemeGroupVersion.WithKind("NodePool").GroupKind(): {
			DefinitionName: "nodepools.karpenter.sh",
			HubVersion:     "v1",
			Zygotes: map[string]conversion.ConvertibleObject{
				"v1":      &v1.NodePool{},
				"v1beta1": &v1beta1.NodePool{},
			},
		},
		v1beta1.SchemeGroupVersion.WithKind("NodeClaim").GroupKind(): {
			DefinitionName: "nodeclaims.karpenter.sh",
			HubVersion:     "v1",
			Zygotes: map[string]conversion.ConvertibleObject{
				"v1":      &v1.NodeClaim{},
				"v1beta1": &v1beta1.NodeClaim{},
			},
		},
	}
)

func NewCRDConversionWebhook(ctx context.Context, _ configmap.Watcher) *controller.Impl {
	nodeclassCtx := injection.GetNodeClasses(ctx)
	client := injection.GetClient(ctx)
	return conversion.NewConversionController(
		ctx,
		"/conversion/karpenter.sh",
		ConversionResource,
		func(ctx context.Context) context.Context {
			return injection.WithClient(injection.WithNodeClasses(ctx, nodeclassCtx), client)
		},
	)
}

func NewCRDValidationWebhook(ctx context.Context, _ configmap.Watcher) *controller.Impl {
	return validation.NewAdmissionController(ctx,
		"validation.webhook.karpenter.sh",
		"/validate/karpenter.sh",
		Resources,
		func(ctx context.Context) context.Context { return ctx },
		true,
	)
}

func NewConfigValidationWebhook(ctx context.Context, _ configmap.Watcher) *controller.Impl {
	return configmaps.NewAdmissionController(ctx,
		"validation.webhook.config.karpenter.sh",
		"/validate/config.karpenter.sh",
		configmap.Constructors{
			knativelogging.ConfigMapName(): knativelogging.NewConfigFromConfigMap,
		},
	)
}

// Start copies the relevant portions for starting the webhooks from sharedmain.MainWithConfig
// https://github.com/knative/pkg/blob/0f52db700d63/injection/sharedmain/main.go#L227
func Start(ctx context.Context, cfg *rest.Config, ctors ...knativeinjection.NamedControllerConstructor) {
	ctx, startInformers := knativeinjection.EnableInjectionOrDie(ctx, cfg)
	logger := logging.NewLogger(ctx, component)
	ctx = knativelogging.WithLogger(ctx, logger)

	cmw := sharedmain.SetupConfigMapWatchOrDie(ctx, knativelogging.FromContext(ctx))
	controllers, webhooks := sharedmain.ControllersAndWebhooksFromCtors(ctx, cmw, lo.Map(ctors, func(ncc knativeinjection.NamedControllerConstructor, _ int) knativeinjection.ControllerConstructor {
		return ncc.ControllerConstructor
	})...)

	// Many of the webhooks rely on configuration, e.g. configurable defaults, feature flags.
	// So make sure that we have synchronized our configuration state before launching the
	// webhooks, so that things are properly initialized.
	logger.Info("Starting configuration manager...")
	if err := cmw.Start(ctx.Done()); err != nil {
		knativelogging.FromContext(ctx).Fatalw("Failed to start configuration manager", zap.Error(err))
	}

	// If we have one or more admission controllers, then start the webhook
	// and pass them in.
	var wh *webhook.Webhook
	var err error
	eg, egCtx := errgroup.WithContext(ctx)
	if len(webhooks) > 0 {
		// Update the metric exporter to point to a prometheus endpoint
		lo.Must0(metrics.UpdateExporter(ctx, metrics.ExporterOptions{
			Component:      strings.ReplaceAll(component, "-", "_"),
			ConfigMap:      lo.Must(metrics.NewObservabilityConfigFromConfigMap(nil)).GetConfigMap().Data,
			Secrets:        sharedmain.SecretFetcher(ctx),
			PrometheusPort: options.FromContext(ctx).WebhookMetricsPort,
		}, logger))
		// Register webhook metrics
		webhook.RegisterMetrics()

		wh, err = webhook.New(ctx, webhooks)
		if err != nil {
			knativelogging.FromContext(ctx).Fatalw("Failed to create webhook", zap.Error(err))
		}
		eg.Go(func() error {
			return wh.Run(ctx.Done())
		})
	}

	// Start the injection clients and informers.
	startInformers()

	// Wait for webhook informers to sync.
	if wh != nil {
		wh.InformersHaveSynced()
	}
	knativelogging.FromContext(ctx).Info("Starting controllers...")
	eg.Go(func() error {
		return controller.StartAll(ctx, controllers...)
	})
	// This will block until either a signal arrives or one of the grouped functions
	// returns an error.
	<-egCtx.Done()

	// Don't forward ErrServerClosed as that indicates we're already shutting down.
	if err := eg.Wait(); err != nil && !stderrors.Is(err, http.ErrServerClosed) {
		knativelogging.FromContext(ctx).Errorw("Error while running server", zap.Error(err))
	}
}

func HealthProbe(ctx context.Context) healthz.Checker {
	// Create new transport that doesn't validate the TLS certificate
	// This transport is just polling so validating the server certificate isn't necessary
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // nolint:gosec
	client := &http.Client{Transport: transport}

	// TODO: Add knative health check port for webhooks when health port can be configured
	// Issue: https://github.com/knative/pkg/issues/2765
	return func(req *http.Request) (err error) {
		res, err := client.Get(fmt.Sprintf("https://localhost:%d", options.FromContext(ctx).WebhookPort))
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

func ValidateConversionEnabled(ctx context.Context, directKubeClient client.Client) {
	failureCount := 0
	for {
		// We only need to try to get a single NodePool from the API server
		err := directKubeClient.List(ctx, &v1.NodePoolList{}, &client.ListOptions{Limit: 1})
		// We only want to bail out with a failure if we know the reason that we are not able to List on this API
		// is due to an internal error on the conversion webhook
		if errors.IsInternalError(err) && strings.Contains(err.Error(), "conversion webhook for karpenter.sh/v1beta1, Kind=NodePool failed") {
			failureCount++
			// If we see more than 2 consecutive failures, then we should panic at that point
			if failureCount > 2 {
				panic(fmt.Sprintf("validating conversion webhook reachable, %s", err.Error()))
			}
		} else {
			failureCount = 0
		}
		// If there's an error that occurs that we don't know about, and it's not about the API not existing, then log the error
		if err != nil && !meta.IsNoMatchError(err) {
			log.FromContext(ctx).Error(err, "failed validating conversion webhook reachable")
		}
		select {
		case <-ctx.Done(): // process is completing, so just exit
			return
		case <-time.After(time.Minute): // poll the API every minute
		}
	}
}

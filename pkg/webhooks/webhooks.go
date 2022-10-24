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

package webhooks

import (
	"context"

	"knative.dev/pkg/configmap"
	"knative.dev/pkg/controller"
	knativeinjection "knative.dev/pkg/injection"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/webhook/certificates"
	"knative.dev/pkg/webhook/configmaps"
	"knative.dev/pkg/webhook/resourcesemantics/defaulting"
	"knative.dev/pkg/webhook/resourcesemantics/validation"

	"github.com/aws/karpenter-core/pkg/apis"
)

func NewWebhooks() []knativeinjection.ControllerConstructor {
	return []knativeinjection.ControllerConstructor{
		certificates.NewController,
		NewCRDDefaultingWebhook,
		NewCRDValidationWebhook,
		NewConfigValidationWebhook,
	}
}

func NewCRDDefaultingWebhook(ctx context.Context, w configmap.Watcher) *controller.Impl {
	return defaulting.NewAdmissionController(ctx,
		"defaulting.webhook.provisioners.karpenter.sh",
		"/default-resource",
		apis.Resources,
		InjectContext,
		true,
	)
}

func NewCRDValidationWebhook(ctx context.Context, w configmap.Watcher) *controller.Impl {
	return validation.NewAdmissionController(ctx,
		"validation.webhook.provisioners.karpenter.sh",
		"/validate-resource",
		apis.Resources,
		InjectContext,
		true,
	)
}

func NewConfigValidationWebhook(ctx context.Context, cmw configmap.Watcher) *controller.Impl {
	return configmaps.NewAdmissionController(ctx,
		"validation.webhook.config.karpenter.sh",
		"/config-validation",
		configmap.Constructors{
			logging.ConfigMapName(): logging.NewConfigFromConfigMap,
		},
	)
}

func InjectContext(ctx context.Context) context.Context {
	return ctx
}

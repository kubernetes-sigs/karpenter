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

	"k8s.io/apimachinery/pkg/runtime/schema"
	"knative.dev/pkg/configmap"
	"knative.dev/pkg/controller"
	knativeinjection "knative.dev/pkg/injection"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/webhook/certificates"
	"knative.dev/pkg/webhook/configmaps"
	"knative.dev/pkg/webhook/resourcesemantics"
	"knative.dev/pkg/webhook/resourcesemantics/validation"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
)

func NewWebhooks() []knativeinjection.ControllerConstructor {
	return []knativeinjection.ControllerConstructor{
		certificates.NewController,
		NewCRDValidationWebhook,
		NewConfigValidationWebhook,
	}
}

func NewCRDValidationWebhook(ctx context.Context, w configmap.Watcher) *controller.Impl {
	return validation.NewAdmissionController(ctx,
		"validation.webhook.karpenter.sh",
		"/validate/karpenter.sh",
		Resources,
		func(ctx context.Context) context.Context { return ctx },
		true,
	)
}

func NewConfigValidationWebhook(ctx context.Context, cmw configmap.Watcher) *controller.Impl {
	return configmaps.NewAdmissionController(ctx,
		"validation.webhook.config.karpenter.sh",
		"/validate/config.karpenter.sh",
		configmap.Constructors{
			logging.ConfigMapName(): logging.NewConfigFromConfigMap,
		},
	)
}

var Resources = map[schema.GroupVersionKind]resourcesemantics.GenericCRD{
	v1alpha5.SchemeGroupVersion.WithKind("Provisioner"): &v1alpha5.Provisioner{},
}

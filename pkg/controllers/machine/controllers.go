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

package machine

import (
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/controllers/machine/garbagecollection"
	"github.com/aws/karpenter-core/pkg/controllers/machine/lifecycle"
	"github.com/aws/karpenter-core/pkg/controllers/machine/termination"
	"github.com/aws/karpenter-core/pkg/operator/controller"
)

func NewControllers(clk clock.Clock, kubeClient client.Client, cloudProvider cloudprovider.CloudProvider) []controller.Controller {
	return []controller.Controller{
		lifecycle.NewController(clk, kubeClient, cloudProvider),
		garbagecollection.NewController(clk, kubeClient, cloudProvider),
		termination.NewController(kubeClient, cloudProvider),
	}
}

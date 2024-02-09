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
	kwok "sigs.k8s.io/karpenter/kwok/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/controllers"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/operator"
)

func init() {
	v1beta1.RestrictedLabelDomains = v1beta1.RestrictedLabelDomains.Insert(kwok.Group)
	v1beta1.WellKnownLabels = v1beta1.WellKnownLabels.Insert(
		kwok.InstanceSizeLabelKey,
		kwok.InstanceFamilyLabelKey,
		kwok.InstanceCPULabelKey,
		kwok.InstanceMemoryLabelKey,
	)
}

func main() {
	ctx, op := operator.NewOperator()

	cloudProvider := kwok.NewCloudProvider(ctx, op.GetClient(), kwok.ConstructInstanceTypes())
	op.
		WithControllers(ctx, controllers.NewControllers(
			op.Clock,
			op.GetClient(),
			state.NewCluster(op.Clock, op.GetClient(), cloudProvider),
			op.EventRecorder,
			cloudProvider,
		)...).Start(ctx)
}

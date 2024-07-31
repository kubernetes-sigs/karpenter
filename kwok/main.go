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
	"sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/karpenter/kwok/apis/v1alpha1"
	kwok "sigs.k8s.io/karpenter/kwok/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/controllers"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/operator"
)

func init() {
	v1beta1.RestrictedLabelDomains = v1beta1.RestrictedLabelDomains.Insert(v1alpha1.Group)
	v1beta1.WellKnownLabels = v1beta1.WellKnownLabels.Insert(
		v1alpha1.InstanceTypeLabelKey,
		v1alpha1.InstanceSizeLabelKey,
		v1alpha1.InstanceFamilyLabelKey,
		v1alpha1.InstanceCPULabelKey,
		v1alpha1.InstanceMemoryLabelKey,
	)
}

func main() {
	ctx, op := operator.NewOperator()
	instanceTypes, err := kwok.ConstructInstanceTypes()
	if err != nil {
		log.FromContext(ctx).Error(err, "failed constructing instance types")
	}

	cloudProvider := kwok.NewCloudProvider(ctx, op.GetClient(), instanceTypes)
	op.
		WithControllers(ctx, controllers.NewControllers(
			op.Clock,
			op.GetClient(),
			state.NewCluster(op.Clock, op.GetClient(), cloudProvider),
			op.EventRecorder,
			cloudProvider,
		)...).Start(ctx, cloudProvider)
}

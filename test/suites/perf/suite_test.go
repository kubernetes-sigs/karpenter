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

package perf_test

import (
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/karpenter/kwok/apis/v1alpha1"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/test/pkg/debug"
	"sigs.k8s.io/karpenter/test/pkg/environment/common"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
)

var nodePool *v1.NodePool
var nodeClass *v1alpha1.KWOKNodeClass
var env *common.Environment

var testLabels = map[string]string{
	test.DiscoveryLabel: "owned",
}
var labelSelector = labels.SelectorFromSet(testLabels)

func TestPerf(t *testing.T) {
	RegisterFailHandler(Fail)
	BeforeSuite(func() {
		env = common.NewEnvironment(t)
	})
	AfterSuite(func() {
		// Write out the timestamps from our tests
		if err := debug.WriteTimestamps(env.OutputDir, env.TimeIntervalCollector); err != nil {
			log.FromContext(env).Info(fmt.Sprintf("Failed to write timestamps to files, %s", err))
		}
		env.Stop()
	})
	RunSpecs(t, "Perf")
}

var _ = BeforeEach(func() {
	env.BeforeEach()
	nodeClass = env.DefaultNodeClass()
	nodePool = env.DefaultNodePool(nodeClass)
	test.ReplaceRequirements(nodePool, v1.NodeSelectorRequirementWithMinValues{
		NodeSelectorRequirement: corev1.NodeSelectorRequirement{
			Key:      v1alpha1.InstanceSizeLabelKey,
			Operator: corev1.NodeSelectorOpLt,
			Values:   []string{"32"},
		},
	})
	nodePool.Spec.Disruption.ExpireAfter = v1.NillableDuration{Duration: lo.ToPtr(30 * time.Minute)}
})

var _ = AfterEach(func() {
	env.TimeIntervalCollector.Finalize()
	env.Cleanup()
	env.AfterEach()
})

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

package dra_test

import (
	"fmt"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/test/pkg/debug"
	"sigs.k8s.io/karpenter/test/pkg/environment/common"
)

var nodePool *v1.NodePool
var nodeClass *unstructured.Unstructured
var env *common.Environment

func TestDRA(t *testing.T) {
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
	RunSpecs(t, "DRA")
}

var _ = BeforeEach(func() {
	env.BeforeEach()
	nodeClass = env.DefaultNodeClass.DeepCopy()
	nodeClass.SetName(fmt.Sprintf("%s-%s", nodeClass.GetName(), test.RandomName()))
	nodePool = env.DefaultNodePool(nodeClass)
	// no limits!!! to the moon!!!
	nodePool.Spec.Limits = v1.Limits{}
	nodePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "100%"}}
	// Set expiration to some high value so that there's age-based ordering for consolidation tests
	nodePool.Spec.Template.Spec.ExpireAfter = v1.MustParseNillableDuration("30h")
})

var _ = AfterEach(func() {
	env.TimeIntervalCollector.Finalize()
	env.Cleanup()
	env.AfterEach()
})

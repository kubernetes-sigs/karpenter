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

package disruption_test

import (
	"testing"
	"time"

	"sigs.k8s.io/karpenter/kwok/apis/v1alpha1"
	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/test/pkg/environment/common"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
)

var nodePool *v1beta1.NodePool
var nodeClass *v1alpha1.KWOKNodeClass
var env *common.Environment

func TestDisruption(t *testing.T) {
	RegisterFailHandler(Fail)
	BeforeSuite(func() {
		env = common.NewEnvironment(t)
	})
	AfterSuite(func() {
		env.Stop()
	})
	RunSpecs(t, "Disruption")
}

var _ = BeforeEach(func() {
	env.BeforeEach()
	nodeClass = env.DefaultNodeClass()
	nodePool = env.DefaultNodePool(nodeClass)
	nodePool.Spec.Disruption.ExpireAfter = v1beta1.NillableDuration{Duration: lo.ToPtr(time.Second * 30)}
})
var _ = AfterEach(func() { env.Cleanup() })
var _ = AfterEach(func() { env.AfterEach() })

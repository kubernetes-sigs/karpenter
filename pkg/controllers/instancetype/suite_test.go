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

package instancetype_test

import (
	"context"
	"testing"

	"github.com/go-logr/zapr"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/samber/lo"
	"go.uber.org/zap/zaptest"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/karpenter/pkg/apis"
	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/instancetype"
)

var (
	ctx                  context.Context
	kubeClient           client.Client
	cloudProvider        *fake.CloudProvider
	instanceTypeProvider *instancetype.Provider
	nodePool             *v1beta1.NodePool
)

func Test(t *testing.T) {
	ctx = log.IntoContext(context.Background(), zapr.NewLogger(zaptest.NewLogger(t)))
	env := envtest.Environment{Scheme: scheme.Scheme, CRDs: apis.CRDs}
	defer func() { lo.Must0(env.Stop()) }()
	kubeClient = lo.Must(client.New(lo.Must(env.Start()), client.Options{}))
	cloudProvider = fake.NewCloudProvider()
	instanceTypeProvider = instancetype.NewProvider(cloudProvider)

	gomega.RegisterFailHandler(ginkgo.Fail)
	ginkgo.RunSpecs(t, "Eviction")
}

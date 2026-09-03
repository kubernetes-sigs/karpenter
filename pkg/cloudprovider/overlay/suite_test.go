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

package overlay_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/overlay"
	"sigs.k8s.io/karpenter/pkg/operator/options"
)

type erroringCloudProvider struct {
	*fake.CloudProvider
	err error
}

func (e *erroringCloudProvider) GetInstanceTypes(context.Context, *v1.NodePool) ([]*cloudprovider.InstanceType, error) {
	return nil, e.err
}

func TestOverlay(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "CloudProvider Overlay Suite")
}

var _ = Describe("CloudProvider Overlay", func() {
	It("should add NodePool context to cloud provider errors", func() {
		providerErr := errors.New("provider unavailable")
		nodePool := &v1.NodePool{}
		nodePool.Name = "default"
		decorated := overlay.Decorate(&erroringCloudProvider{
			CloudProvider: fake.NewCloudProvider(),
			err:           providerErr,
		}, nil, nil)

		_, err := decorated.GetInstanceTypes(options.ToContext(context.Background(), &options.Options{}), nodePool)
		Expect(err).To(MatchError(fmt.Sprintf("getting cloud provider instance types, %s (NodePool=%s)", providerErr, nodePool.Name)))
		Expect(errors.Is(err, providerErr)).To(BeTrue())
	})
	It("should add empty NodePool context when the NodePool is nil", func() {
		providerErr := errors.New("provider unavailable")
		decorated := overlay.Decorate(&erroringCloudProvider{
			CloudProvider: fake.NewCloudProvider(),
			err:           providerErr,
		}, nil, nil)

		_, err := decorated.GetInstanceTypes(options.ToContext(context.Background(), &options.Options{}), nil)
		Expect(err).To(MatchError(fmt.Sprintf("getting cloud provider instance types, %s (NodePool=)", providerErr)))
		Expect(errors.Is(err, providerErr)).To(BeTrue())
	})
})

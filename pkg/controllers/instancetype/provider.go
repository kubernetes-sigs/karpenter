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

package instancetype

import (
	"context"
	"fmt"
	"time"

	"github.com/awslabs/operatorpkg/option"
	"github.com/patrickmn/go-cache"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

var TTL = 5 * time.Minute

func NewProvider(cloudProvider cloudprovider.CloudProvider) *Provider {
	return &Provider{
		cloudProvider: cloudProvider,
		instanceTypes: cache.New(TTL, TTL),
	}
}

type Provider struct {
	cloudProvider cloudprovider.CloudProvider
	instanceTypes *cache.Cache
}

type Options struct {
	Cached bool
}

func FromCache(cached bool) func(options *Options) {
	return func(options *Options) {
		options.Cached = cached
	}
}

var DefaultOptions = []option.Function[Options]{
	FromCache(false),
}

func (p *Provider) Get(ctx context.Context, nodePool *v1beta1.NodePool, optionFuncs ...option.Function[Options]) ([]*cloudprovider.InstanceType, error) {
	options := option.Resolve(append(DefaultOptions, optionFuncs...)...)
	if instanceTypes, ok := p.instanceTypes.Get(string(nodePool.UID)); options.Cached && ok {
		return instanceTypes.([]*cloudprovider.InstanceType), nil
	}
	instanceTypes, err := p.get(ctx, nodePool)
	if err != nil {
		return nil, err
	}
	p.instanceTypes.SetDefault(string(nodePool.UID), instanceTypes)
	return instanceTypes, nil
}

func (p *Provider) get(ctx context.Context, nodePool *v1beta1.NodePool) ([]*cloudprovider.InstanceType, error) {
	instanceTypes, err := p.cloudProvider.GetInstanceTypes(ctx, nodePool)
	if err != nil {
		return nil, fmt.Errorf("getting instance types, %w", err)
	}
	return instanceTypes, nil
}

func (p *Provider) Flush() {
	p.instanceTypes.Flush()
}

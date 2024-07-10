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

package common

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/awslabs/operatorpkg/object"

	"github.com/awslabs/operatorpkg/status"
	"github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"

	"sigs.k8s.io/karpenter/kwok/apis/v1alpha1"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/utils/testing" //nolint:stylecheck
	"sigs.k8s.io/karpenter/test/pkg/debug"

	"knative.dev/pkg/system"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/operator"
)

type ContextKey string

const (
	GitRefContextKey = ContextKey("gitRef")
)

type Environment struct {
	context.Context
	cancel context.CancelFunc

	TimeIntervalCollector *debug.TimeIntervalCollector
	Client                client.Client
	Config                *rest.Config
	KubeClient            kubernetes.Interface
	Monitor               *Monitor

	OutputDir         string
	StartingNodeCount int
}

func NewEnvironment(t *testing.T) *Environment {
	ctx := TestContextWithLogger(t)
	ctx, cancel := context.WithCancel(ctx)
	config := NewConfig()
	client := NewClient(ctx, config)

	lo.Must0(os.Setenv(system.NamespaceEnvKey, "kube-system"))
	if val, ok := os.LookupEnv("GIT_REF"); ok {
		ctx = context.WithValue(ctx, GitRefContextKey, val)
	}
	// Get the output dir if it's set
	outputDir, _ := os.LookupEnv("OUTPUT_DIR")

	gomega.SetDefaultEventuallyTimeout(5 * time.Minute)
	gomega.SetDefaultEventuallyPollingInterval(1 * time.Second)
	return &Environment{
		Context:               ctx,
		cancel:                cancel,
		Config:                config,
		Client:                client,
		KubeClient:            kubernetes.NewForConfigOrDie(config),
		Monitor:               NewMonitor(ctx, client),
		TimeIntervalCollector: debug.NewTimestampCollector(),
		OutputDir:             outputDir,
	}
}

func (env *Environment) Stop() {
	env.cancel()
}

func NewConfig() *rest.Config {
	config := controllerruntime.GetConfigOrDie()
	config.UserAgent = fmt.Sprintf("testing-%s", operator.Version)
	config.QPS = 1e6
	config.Burst = 1e6
	return config
}

func NewClient(ctx context.Context, config *rest.Config) client.Client {
	cache := lo.Must(cache.New(config, cache.Options{Scheme: scheme.Scheme}))
	lo.Must0(cache.IndexField(ctx, &corev1.Pod{}, "spec.nodeName", func(o client.Object) []string {
		pod := o.(*corev1.Pod)
		return []string{pod.Spec.NodeName}
	}))
	lo.Must0(cache.IndexField(ctx, &corev1.Event{}, "involvedObject.kind", func(o client.Object) []string {
		evt := o.(*corev1.Event)
		return []string{evt.InvolvedObject.Kind}
	}))
	lo.Must0(cache.IndexField(ctx, &corev1.Node{}, "spec.unschedulable", func(o client.Object) []string {
		node := o.(*corev1.Node)
		return []string{strconv.FormatBool(node.Spec.Unschedulable)}
	}))
	lo.Must0(cache.IndexField(ctx, &corev1.Node{}, "spec.taints[*].karpenter.sh/disruption", func(o client.Object) []string {
		node := o.(*corev1.Node)
		t, _ := lo.Find(node.Spec.Taints, func(t corev1.Taint) bool {
			return t.Key == v1.DisruptionTaintKey
		})
		return []string{t.Value}
	}))
	lo.Must0(cache.IndexField(ctx, &v1.NodeClaim{}, "status.conditions[*].type", func(o client.Object) []string {
		nodeClaim := o.(*v1.NodeClaim)
		return lo.Map(nodeClaim.Status.Conditions, func(c status.Condition, _ int) string {
			return c.Type
		})
	}))

	c := lo.Must(client.New(config, client.Options{Scheme: scheme.Scheme, Cache: &client.CacheOptions{Reader: cache}}))

	go func() {
		lo.Must0(cache.Start(ctx))
	}()
	if !cache.WaitForCacheSync(ctx) {
		log.Fatalf("cache failed to sync")
	}
	return c
}

func (env *Environment) DefaultNodeClass() *v1alpha1.KWOKNodeClass {
	return &v1alpha1.KWOKNodeClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: test.RandomName(),
		},
	}
}

func (env *Environment) DefaultNodePool(nodeClass *v1alpha1.KWOKNodeClass) *v1.NodePool {
	nodePool := test.NodePool()
	nodePool.Spec.Template.Spec.NodeClassRef = &v1.NodeClassReference{
		Name:  nodeClass.Name,
		Kind:  object.GVK(nodeClass).Kind,
		Group: object.GVK(nodeClass).GroupVersion().String(),
	}
	nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirementWithMinValues{
		{
			NodeSelectorRequirement: corev1.NodeSelectorRequirement{
				Key:      corev1.LabelOSStable,
				Operator: corev1.NodeSelectorOpIn,
				Values:   []string{string(corev1.Linux)},
			},
		},
		{
			NodeSelectorRequirement: corev1.NodeSelectorRequirement{
				Key:      v1.CapacityTypeLabelKey,
				Operator: corev1.NodeSelectorOpIn,
				Values:   []string{v1.CapacityTypeOnDemand},
			},
		},
	}
	nodePool.Spec.Disruption.ConsolidateAfter = &v1.NillableDuration{}
	nodePool.Spec.Disruption.ExpireAfter.Duration = nil
	nodePool.Spec.Limits = v1.Limits(corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("1000"),
		corev1.ResourceMemory: resource.MustParse("1000Gi"),
	})
	return nodePool
}

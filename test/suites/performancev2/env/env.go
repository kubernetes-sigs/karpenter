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

// Package env is a standalone copy of what performancev2 needs from test/pkg/environment/common:
// clients and the default KWOK NodeClass and NodePool. It has no informer cache and no global
// flags, so the suite doesn't depend on the v1 environment.
package env

import (
	_ "embed"
	"fmt"

	"github.com/awslabs/operatorpkg/object"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"
)

var (
	//go:embed kwoknodeclass.yaml
	kwokNodeClass []byte
	//go:embed nodepool.yaml
	nodePool []byte
)

type Clients struct {
	Client     client.Client
	KubeClient kubernetes.Interface
}

func New() Clients {
	config := controllerruntime.GetConfigOrDie()
	config.QPS, config.Burst = 1e6, 1e6
	c, err := client.New(config, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		panic(err)
	}
	return Clients{Client: c, KubeClient: kubernetes.NewForConfigOrDie(config)}
}

func KWOKNodeClass() *unstructured.Unstructured {
	nc := object.Unmarshal[unstructured.Unstructured](kwokNodeClass)
	nc.SetName(fmt.Sprintf("%s-%s", nc.GetName(), test.RandomName()))
	return nc
}

func NodePool(nodeClass client.Object) *v1.NodePool {
	np := object.Unmarshal[v1.NodePool](nodePool)
	gvk := nodeClass.GetObjectKind().GroupVersionKind()
	np.Spec.Template.Spec.NodeClassRef = &v1.NodeClassReference{Kind: gvk.Kind, Group: gvk.Group, Name: nodeClass.GetName()}
	np.Name = fmt.Sprintf("%s-%s", np.Name, test.RandomName())
	return np
}

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

package perf

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// Provider is the plugin a KWOK-based Karpenter distribution implements to run the shared scenarios.
// A scenario runs only if Levels has an entry for at least one of its phases; anything else is Off.
type Provider struct {
	Name        string
	Summary     string
	Client      client.Client
	KubeClient  kubernetes.Interface
	Controller  Controller
	NodeClass   func() client.Object
	NodePool    func(nodeClass client.Object) *v1.NodePool
	Sizes       map[string]map[Vector]int
	Levels      Levels
	Calibration *Calibration
}

// Controller locates the Karpenter pod whose /metrics the suite scrapes through the API server proxy.
type Controller struct {
	Namespace   string
	Selector    labels.Selector
	MetricsPort string
}

type Levels map[string][]Gate

type Vector string

const (
	VectorPods      Vector = "pods"
	VectorNodePools Vector = "nodepools"
)

func sizesLabel(sizes map[Vector]int, kv, sep string) string {
	keys := make([]string, 0, len(sizes))
	for v := range sizes {
		keys = append(keys, string(v))
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s%s%d", k, kv, sizes[Vector(k)]))
	}
	return strings.Join(parts, sep)
}

func (p *Provider) size(scenario string, v Vector, def int) int {
	GinkgoHelper()
	o := map[Vector]int{}
	if s := os.Getenv("KARPENTER_PERF_SIZES"); s != "" {
		Expect(json.Unmarshal([]byte(s), &o)).To(Succeed(), "parsing KARPENTER_PERF_SIZES")
	}
	if n, ok := o[v]; ok {
		return n
	}
	if n, ok := p.Sizes[scenario][v]; ok {
		return n
	}
	return def
}

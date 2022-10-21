/*
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

package metrics

import (
	"context"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/operator/controller"
)

const pollingPeriod = 5 * time.Second

type Scraper interface {
	Scrape(context.Context)
}

type MetricScrapingController struct {
	cluster  *state.Cluster
	scrapers []Scraper
}

func NewMetricScrapingController(cluster *state.Cluster) *MetricScrapingController {
	return &MetricScrapingController{
		cluster:  cluster,
		scrapers: []Scraper{NewNodeScraper(cluster)},
	}
}

func (ms *MetricScrapingController) Register(_ context.Context, mgr manager.Manager) error {
	return controller.NewSingletonManagedBy(mgr).
		Named("metric-scraper").
		Complete(ms)
}

func (ms *MetricScrapingController) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	for _, scraper := range ms.scrapers {
		scraper.Scrape(ctx)
	}
	return reconcile.Result{RequeueAfter: pollingPeriod}, nil
}

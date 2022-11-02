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
	"net/http"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/controllers/metrics/state/scraper"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	corecontroller "github.com/aws/karpenter-core/pkg/operator/controller"
)

const pollingPeriod = 5 * time.Second

type Controller struct {
	cluster  *state.Cluster
	scrapers []scraper.Scraper
}

func NewController(cluster *state.Cluster) *Controller {
	return &Controller{
		cluster:  cluster,
		scrapers: []scraper.Scraper{scraper.NewNodeScraper(cluster)},
	}
}

func (c *Controller) Builder(_ context.Context, mgr manager.Manager) corecontroller.Builder {
	return corecontroller.NewSingletonManagedBy(mgr).
		Named("metric_scraper")
}

func (c *Controller) LivenessProbe(_ *http.Request) error {
	return nil
}

func (c *Controller) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	for _, scraper := range c.scrapers {
		scraper.Scrape(ctx)
	}
	return reconcile.Result{RequeueAfter: pollingPeriod}, nil
}

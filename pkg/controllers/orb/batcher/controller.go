package batcher

import (
	"context"
	"fmt"
	"time"

	"github.com/awslabs/operatorpkg/singleton"
	"k8s.io/apimachinery/pkg/util/sets"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// const (
// 	orbQueueBaseDelay = 100 * time.Millisecond
// 	orbQueueMaxDelay  = 10 * time.Second
// )

type Queue struct {
	//workqueue.RateLimitingInterface
	//mu  sync.Mutex
	set sets.Set[string]
}

func NewQueue() *Queue {
	queue := &Queue{
		//RateLimitingInterface: workqueue.NewRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(orbQueueBaseDelay, orbQueueMaxDelay)),
		set: sets.New[string](),
	}
	return queue
}

type Controller struct {
	queue *Queue
	// some sort of log store, or way to batch logs together before sending to PV
}

// TODO: add struct elements and their instantiations, when defined
func NewController(queue *Queue) *Controller {
	return &Controller{
		queue: queue,
	}
}

func (c *Controller) Reconcile(ctx context.Context) (reconcile.Result, error) {
	// TODO: what does this do / where does it reference to or need to reference to?
	// ctx = injection.WithControllerName(ctx, "orb.batcher")

	fmt.Println("Starting One Reconcile Print from ORB...")

	c.queue.set.Insert("Hello World from the ORB Batcher Reconciler")

	// for items in the Queue, print them
	for item := range c.queue.set {
		fmt.Println(item)
	}

	fmt.Println("Ending One Reconcile Print from ORB...")
	fmt.Println()

	return reconcile.Result{RequeueAfter: time.Second * 5}, nil
}

// TODO: What does this register function do? Is it needed for a controller to work?
func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("orb.batcher").
		WatchesRawSource(singleton.Source()).
		Complete(singleton.AsReconciler(c))
}

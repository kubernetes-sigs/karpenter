package reconcile

import "time"

type Result struct {
	Requeue      bool
	RequeueAfter time.Duration
}

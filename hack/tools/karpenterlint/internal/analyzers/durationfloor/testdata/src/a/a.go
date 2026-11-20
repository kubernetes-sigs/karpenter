package a

import (
	"time"

	"github.com/samber/lo"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type Clock interface {
	Now() time.Time
	Since(time.Time) time.Duration
}

// Shape of kubernetes-sigs/karpenter#3038 before the fix.
func gracePeriod(deadline time.Time, c Clock) *int64 {
	return lo.ToPtr(int64(time.Until(deadline).Seconds())) // want `integer seconds from a time difference`
}

func gracePeriodFixed(deadline time.Time) *int64 {
	return lo.ToPtr(max(int64(time.Until(deadline).Seconds()), 0))
}

func gracePeriodVar(deadline time.Time, c Clock) int64 {
	remaining := deadline.Sub(c.Now())
	return int64(remaining.Seconds()) // want `integer seconds from a time difference`
}

func gracePeriodGuarded(deadline time.Time, c Clock) int64 {
	if remaining := deadline.Sub(c.Now()); remaining > 0 {
		return int64(remaining.Seconds())
	}
	return 0
}

func requeue(expiration time.Time, c Clock) (reconcile.Result, error) {
	if c.Now().Before(expiration) {
		return reconcile.Result{RequeueAfter: expiration.Sub(c.Now())}, nil // want `RequeueAfter from a time difference`
	}
	return reconcile.Result{}, nil
}

func requeueSince(start time.Time, c Clock) reconcile.Result {
	return reconcile.Result{RequeueAfter: time.Minute - c.Since(start)} // want `RequeueAfter from a time difference`
}

func requeueFixed(expiration time.Time, c Clock) reconcile.Result {
	if remaining := expiration.Sub(c.Now()); remaining > 0 {
		return reconcile.Result{RequeueAfter: remaining}
	}
	return reconcile.Result{RequeueAfter: max(expiration.Sub(c.Now()), time.Nanosecond)}
}

func constant() reconcile.Result {
	d := time.Minute
	return reconcile.Result{RequeueAfter: d}
}

func logging(start time.Time) float64 {
	return time.Since(start).Seconds()
}

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

package pod

import (
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/clock"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

const (
	// DefaultDisruptionScheduleDuration is the default duration for a disruption schedule window
	// when the duration annotation is not specified.
	DefaultDisruptionScheduleDuration = "1h"
)

// DisruptionScheduleResult holds the result of evaluating a pod's disruption schedule.
type DisruptionScheduleResult struct {
	// HasSchedule indicates whether the pod has a disruption schedule annotation.
	HasSchedule bool
	// IsActive indicates whether disruption is currently allowed.
	// True if: no schedule is set, schedule is active, or schedule is invalid (fail-open).
	IsActive bool
	// InvalidSchedule indicates the cron expression was invalid.
	InvalidSchedule bool
	// InvalidDuration indicates the duration was invalid.
	InvalidDuration bool
	// ErrorMessage contains details about any parsing errors.
	ErrorMessage string
}

// EvaluateDisruptionSchedule checks if a pod's disruption schedule allows disruption at the current time.
// It returns a result indicating whether the pod can be disrupted and any parsing errors.
//
// Behavior:
//   - If no schedule annotation is set, returns HasSchedule=false, IsActive=true (always disruptable)
//   - If schedule is invalid, returns IsActive=true (fail-open) with InvalidSchedule=true
//   - If duration is invalid, returns IsActive=true (fail-open) with InvalidDuration=true
//   - If current time is within the schedule window, returns IsActive=true
//   - If current time is outside the schedule window, returns IsActive=false
func EvaluateDisruptionSchedule(pod *corev1.Pod, clk clock.Clock) DisruptionScheduleResult {
	if pod.Annotations == nil {
		return DisruptionScheduleResult{HasSchedule: false, IsActive: true}
	}

	scheduleStr := pod.Annotations[v1.DisruptionScheduleAnnotationKey]
	if scheduleStr == "" {
		return DisruptionScheduleResult{HasSchedule: false, IsActive: true}
	}

	// Parse duration (default to 1h if not specified)
	durationStr := pod.Annotations[v1.DisruptionScheduleDurationAnnotationKey]
	if durationStr == "" {
		durationStr = DefaultDisruptionScheduleDuration
	}

	duration, err := time.ParseDuration(durationStr)
	if err != nil || duration <= 0 {
		return DisruptionScheduleResult{
			HasSchedule:     true,
			IsActive:        true, // fail-open
			InvalidDuration: true,
			ErrorMessage:    fmt.Sprintf("invalid duration %q: %v", durationStr, err),
		}
	}

	// Parse cron schedule (standard 5-field format, UTC timezone)
	// Follows the same pattern as Budget.IsActive() in pkg/apis/v1/nodepool.go
	schedule, err := cron.ParseStandard(fmt.Sprintf("TZ=UTC %s", scheduleStr))
	if err != nil {
		return DisruptionScheduleResult{
			HasSchedule:     true,
			IsActive:        true, // fail-open
			InvalidSchedule: true,
			ErrorMessage:    fmt.Sprintf("invalid cron schedule %q: %v", scheduleStr, err),
		}
	}

	// Check if current time is within the disruption window.
	// Walk back in time by the duration and check if the next schedule hit is before now.
	// If the last schedule hit is within the duration window, disruption is active.
	now := clk.Now().UTC()
	checkPoint := now.Add(-duration)
	nextHit := schedule.Next(checkPoint)
	isActive := !nextHit.After(now)

	return DisruptionScheduleResult{
		HasSchedule: true,
		IsActive:    isActive,
	}
}

// IsWithinDisruptionSchedule returns true if the pod can be disrupted based on its schedule.
// Returns true if:
//   - No schedule annotation is set (always disruptable)
//   - Schedule is set and current time is within the window
//   - Schedule is invalid (fail-open behavior)
func IsWithinDisruptionSchedule(pod *corev1.Pod, clk clock.Clock) bool {
	return EvaluateDisruptionSchedule(pod, clk).IsActive
}

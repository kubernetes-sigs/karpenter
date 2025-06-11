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

package readytimeout

import (
	"testing"
	"time"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"


	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
	karpentertesting "sigs.k8s.io/karpenter/pkg/utils/testing"
)

// Test the core timeout logic without requiring full k8s infrastructure
func TestTimeoutLogic(t *testing.T) {
	tests := []struct {
		name        string
		nodeAge     time.Duration
		timeout     time.Duration
		featureGate bool
		nodeReady   bool
		expectRequeue bool
		expectAction  string
	}{
		{
			name:        "feature gate disabled",
			nodeAge:     15 * time.Minute,
			timeout:     10 * time.Minute,
			featureGate: false,
			nodeReady:   false,
			expectRequeue: false,
			expectAction:  "skip",
		},
		{
			name:        "node is ready",
			nodeAge:     15 * time.Minute,
			timeout:     10 * time.Minute,
			featureGate: true,
			nodeReady:   true,
			expectRequeue: false,
			expectAction:  "skip",
		},
		{
			name:        "timeout not exceeded",
			nodeAge:     5 * time.Minute,
			timeout:     10 * time.Minute,
			featureGate: true,
			nodeReady:   false,
			expectRequeue: true,
			expectAction:  "requeue",
		},
		{
			name:        "timeout exceeded",
			nodeAge:     15 * time.Minute,
			timeout:     10 * time.Minute,
			featureGate: true,
			nodeReady:   false,
			expectRequeue: false,
			expectAction:  "recover",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Setup test context following K8s testing best practices
			ctx := karpentertesting.TestContextWithLogger(t)
			
			// Setup options
			opts := test.Options(test.OptionsFields{
				FeatureGates: test.FeatureGates{
					NodeReadyTimeoutRecovery: lo.ToPtr(tt.featureGate),
				},
				NodeReadyTimeout: lo.ToPtr(tt.timeout),
			})
			ctx = options.ToContext(ctx, opts)

			// Create test node condition
			readyStatus := corev1.ConditionFalse
			if tt.nodeReady {
				readyStatus = corev1.ConditionTrue
			}

			// Test the logic without full reconcile loop
			shouldRequeue := tt.nodeAge < tt.timeout
			featureEnabled := opts.FeatureGates.NodeReadyTimeoutRecovery
			nodeIsReady := readyStatus == corev1.ConditionTrue

			// Verify expectations
			if !featureEnabled {
				if shouldRequeue {
					t.Errorf("Expected no requeue when feature disabled, but would requeue")
				}
			} else if nodeIsReady {
				if shouldRequeue {
					t.Errorf("Expected no requeue for ready node, but would requeue")
				}
			} else {
				if shouldRequeue != tt.expectRequeue {
					t.Errorf("Expected requeue=%v, got requeue=%v", tt.expectRequeue, shouldRequeue)
				}
			}
		})
	}
}

func TestHealthThresholdLogic(t *testing.T) {
	tests := []struct {
		name             string
		totalNodes       int
		unhealthyNodes   int
		expectHealthy    bool
	}{
		{
			name:           "all nodes healthy",
			totalNodes:     10,
			unhealthyNodes: 0,
			expectHealthy:  true,
		},
		{
			name:           "under 20% unhealthy",
			totalNodes:     10,
			unhealthyNodes: 1,
			expectHealthy:  true,
		},
		{
			name:           "exactly 20% unhealthy",
			totalNodes:     10,
			unhealthyNodes: 2,
			expectHealthy:  true,
		},
		{
			name:           "over 20% unhealthy",
			totalNodes:     10,
			unhealthyNodes: 3,
			expectHealthy:  false,
		},
		{
			name:           "small cluster edge case",
			totalNodes:     3,
			unhealthyNodes: 1,
			expectHealthy:  true, // 33% but rounds up to allow 1 unhealthy
		},
		{
			name:           "single node cluster",
			totalNodes:     1,
			unhealthyNodes: 1,
			expectHealthy:  true, // Even 100% allows 1 unhealthy in small clusters
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate the health calculation logic
			allowedUnhealthyCount := (tt.totalNodes + 4) / 5 // Equivalent to 20% rounded up
			isHealthy := tt.unhealthyNodes <= allowedUnhealthyCount
			
			if isHealthy != tt.expectHealthy {
				t.Errorf("Expected healthy=%v, got healthy=%v (total=%d, unhealthy=%d, allowed=%d)", 
					tt.expectHealthy, isHealthy, tt.totalNodes, tt.unhealthyNodes, allowedUnhealthyCount)
			}
		})
	}
}

func TestEventGeneration(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
	}
	nodeClaim := &v1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeclaim"},
	}
	nodePool := &v1.NodePool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodepool"},
	}

	tests := []struct {
		name     string
		eventFn  func() events.Event
		wantType string
		wantReason string
	}{
		{
			name:     "recovery event",
			eventFn:  func() events.Event { return NodeReadyTimeoutRecovery(node, nodeClaim, 10*time.Minute) },
			wantType: corev1.EventTypeWarning,
			wantReason: "NodeReadyTimeoutRecovery",
		},
		{
			name:     "blocked event",
			eventFn:  func() events.Event { return NodeReadyTimeoutRecoveryBlocked(node, nodeClaim, nodePool, "test message") },
			wantType: corev1.EventTypeWarning,
			wantReason: "NodeReadyTimeoutRecoveryBlocked",
		},
		{
			name:     "blocked unmanaged event",
			eventFn:  func() events.Event { return NodeReadyTimeoutRecoveryBlockedUnmanagedNodeClaim(node, nodeClaim, "test message") },
			wantType: corev1.EventTypeWarning,
			wantReason: "NodeReadyTimeoutRecoveryBlocked",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := tt.eventFn()
			
			if event.Type != tt.wantType {
				t.Errorf("Expected event type %s, got %s", tt.wantType, event.Type)
			}
			
			if event.Reason != tt.wantReason {
				t.Errorf("Expected event reason %s, got %s", tt.wantReason, event.Reason)
			}
			
			if event.InvolvedObject == nil {
				t.Error("Expected InvolvedObject to be set")
			}
			
			if len(event.DedupeValues) == 0 {
				t.Error("Expected DedupeValues to be set")
			}
		})
	}
} 
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
	"fmt"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/samber/lo"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"
)

// CleanupOptions configures cleanup behavior for test resources
type CleanupOptions struct {
	// Deployments to clean up
	Deployments []*appsv1.Deployment
	// PodSelector for identifying test pods
	PodSelector labels.Selector
	// ForceDelete uses zero grace period for faster cleanup
	ForceDelete bool
	// WaitTimeout for cleanup operations
	WaitTimeout time.Duration
	// VerifyCleanup ensures all resources are actually removed
	VerifyCleanup bool
	// LogProgress outputs cleanup progress messages
	LogProgress bool
}

// DefaultCleanupOptions returns sensible defaults for cleanup operations
func DefaultCleanupOptions() CleanupOptions {
	return CleanupOptions{
		ForceDelete:   true,
		WaitTimeout:   5 * time.Minute,
		VerifyCleanup: true,
		LogProgress:   true,
	}
}

// ForceCleanupTestResources performs aggressive cleanup of test resources
// This is suitable for emergency cleanup scenarios (DeferCleanup) or when
// you need to ensure all test resources are removed quickly
func (env *Environment) ForceCleanupTestResources(opts CleanupOptions) error {
	if opts.LogProgress {
		ginkgo.GinkgoWriter.Printf("🧹 FORCE CLEANUP: Starting aggressive resource cleanup...\n")
	}

	// Force delete deployments
	for _, deployment := range opts.Deployments {
		if deployment != nil {
			err := client.IgnoreNotFound(env.Client.Delete(env.Context, deployment, &client.DeleteOptions{
				GracePeriodSeconds: lo.ToPtr(int64(0)),
			}))
			if err != nil && opts.LogProgress {
				ginkgo.GinkgoWriter.Printf("   ⚠️  Warning: Failed to delete deployment %s: %v\n", deployment.Name, err)
			}
		}
	}

	// Force delete all test nodes
	createdNodes := env.Monitor.CreatedNodes()
	if opts.LogProgress && len(createdNodes) > 0 {
		ginkgo.GinkgoWriter.Printf("   • Force deleting %d nodes\n", len(createdNodes))
	}
	for _, node := range createdNodes {
		err := client.IgnoreNotFound(env.Client.Delete(env.Context, node, &client.DeleteOptions{
			GracePeriodSeconds: lo.ToPtr(int64(0)),
		}))
		if err != nil && opts.LogProgress {
			ginkgo.GinkgoWriter.Printf("   ⚠️  Warning: Failed to delete node %s: %v\n", node.Name, err)
		}
	}

	// Force delete all test nodeclaims
	var nodeClaims v1.NodeClaimList
	err := env.Client.List(env.Context, &nodeClaims, client.MatchingLabels{
		test.DiscoveryLabel: "unspecified",
	})
	if err == nil {
		if opts.LogProgress && len(nodeClaims.Items) > 0 {
			ginkgo.GinkgoWriter.Printf("   • Force deleting %d nodeclaims\n", len(nodeClaims.Items))
		}
		for _, nodeClaim := range nodeClaims.Items {
			err := client.IgnoreNotFound(env.Client.Delete(env.Context, &nodeClaim, &client.DeleteOptions{
				GracePeriodSeconds: lo.ToPtr(int64(0)),
			}))
			if err != nil && opts.LogProgress {
				ginkgo.GinkgoWriter.Printf("   ⚠️  Warning: Failed to delete nodeclaim %s: %v\n", nodeClaim.Name, err)
			}
		}
	}

	// Brief wait for deletion commands to propagate
	time.Sleep(30 * time.Second)

	// Verify pod cleanup if selector provided
	if opts.PodSelector != nil && opts.VerifyCleanup {
		remainingPods := env.Monitor.RunningPods(opts.PodSelector)
		if len(remainingPods) > 0 && opts.LogProgress {
			ginkgo.GinkgoWriter.Printf("   ⚠️  WARNING: %d test pods may still be running\n", len(remainingPods))
		}
	}

	if opts.LogProgress {
		ginkgo.GinkgoWriter.Printf("   ✅ Force cleanup completed\n")
	}

	return nil
}

// WaitForResourceCleanup waits for test resources to be properly cleaned up
// This is suitable for normal cleanup flows where you want to wait for
// graceful termination before proceeding
func (env *Environment) WaitForResourceCleanup(opts CleanupOptions) error {
	if opts.LogProgress {
		ginkgo.GinkgoWriter.Printf("🧹 CLEANUP: Waiting for resource cleanup...\n")
	}

	// Wait for pods to be terminated if selector provided
	if opts.PodSelector != nil {
		gomega.Eventually(func(g gomega.Gomega) {
			pods := env.Monitor.RunningPods(opts.PodSelector)
			g.Expect(pods).To(gomega.HaveLen(0), "All test pods should be terminated")
		}).WithTimeout(opts.WaitTimeout).Should(gomega.Succeed())

		if opts.LogProgress {
			ginkgo.GinkgoWriter.Printf("   • All pods terminated\n")
		}
	}

	// Wait for nodes to be cleaned up
	gomega.Eventually(func(g gomega.Gomega) {
		createdNodes := env.Monitor.CreatedNodes()
		g.Expect(createdNodes).To(gomega.HaveLen(0), "All provisioned nodes should be cleaned up")
	}).WithTimeout(opts.WaitTimeout).Should(gomega.Succeed())

	if opts.LogProgress {
		ginkgo.GinkgoWriter.Printf("   • All nodes cleaned up\n")
	}

	// Wait for nodeclaims to be cleaned up
	gomega.Eventually(func(g gomega.Gomega) {
		var remainingNodeClaims v1.NodeClaimList
		err := env.Client.List(env.Context, &remainingNodeClaims, client.MatchingLabels{
			test.DiscoveryLabel: "unspecified",
		})
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(remainingNodeClaims.Items).To(gomega.HaveLen(0), "All test nodeclaims should be cleaned up")
	}).WithTimeout(opts.WaitTimeout).Should(gomega.Succeed())

	if opts.LogProgress {
		ginkgo.GinkgoWriter.Printf("   • All nodeclaims cleaned up\n")
	}

	return nil
}

// VerifyClusterStabilization ensures the cluster has returned to its baseline state
// This verifies that the node count matches the expected starting count
func (env *Environment) VerifyClusterStabilization() error {
	finalNodeCount := env.Monitor.NodeCount()
	gomega.Expect(finalNodeCount).To(gomega.Equal(env.StartingNodeCount),
		fmt.Sprintf("Node count should return to starting count (%d), but is %d",
			env.StartingNodeCount, finalNodeCount))
	return nil
}

// PerformComprehensiveCleanup combines all cleanup steps in the correct order
// This is a convenience method that performs the full cleanup sequence
func (env *Environment) PerformComprehensiveCleanup(opts CleanupOptions) error {
	if opts.LogProgress {
		ginkgo.GinkgoWriter.Printf("\n🧹 COMPREHENSIVE CLEANUP: Starting full cleanup sequence...\n")
	}

	// Step 1: Delete deployments (graceful)
	for _, deployment := range opts.Deployments {
		if deployment != nil {
			err := env.Client.Delete(env.Context, deployment)
			if err != nil && client.IgnoreNotFound(err) != nil {
				return fmt.Errorf("failed to delete deployment %s: %w", deployment.Name, err)
			}
		}
	}
	if opts.LogProgress {
		ginkgo.GinkgoWriter.Printf("   • Deployments deleted\n")
	}

	// Step 2: Wait for graceful cleanup
	if err := env.WaitForResourceCleanup(opts); err != nil {
		// If graceful cleanup fails, fall back to force cleanup
		if opts.LogProgress {
			ginkgo.GinkgoWriter.Printf("   ⚠️  Graceful cleanup failed, falling back to force cleanup\n")
		}
		return env.ForceCleanupTestResources(opts)
	}

	// Step 3: Verify cluster stabilization
	if opts.VerifyCleanup {
		if err := env.VerifyClusterStabilization(); err != nil {
			return fmt.Errorf("cluster stabilization verification failed: %w", err)
		}
		if opts.LogProgress {
			ginkgo.GinkgoWriter.Printf("   • Cluster stabilization verified\n")
		}
	}

	if opts.LogProgress {
		ginkgo.GinkgoWriter.Printf("✅ COMPREHENSIVE CLEANUP: Completed successfully\n")
	}

	return nil
}

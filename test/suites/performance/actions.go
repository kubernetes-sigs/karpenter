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

package performance

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/samber/lo"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	. "github.com/onsi/ginkgo/v2"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/test/pkg/environment/common"
)

// OutputPerformanceReport outputs a performance report to console and file
func OutputPerformanceReport(report *PerformanceReport, filePrefix string) {
	// Console output (fallback)
	GinkgoWriter.Printf("\n=== %s PERFORMANCE REPORT ===\n", report.TestType)
	GinkgoWriter.Printf("Test: %s\n", report.TestName)
	GinkgoWriter.Printf("Type: %s\n", report.TestType)
	GinkgoWriter.Printf("Total Time: %v\n", report.TotalTime)
	GinkgoWriter.Printf("Total Pods: %d (Net Change: %+d)\n", report.TotalPods, report.PodsNetChange)
	GinkgoWriter.Printf("Total Nodes: %d (Net Change: %+d)\n", report.TotalNodes, report.NodesNetChange)
	GinkgoWriter.Printf("CPU Utilization: %.2f%%\n", report.TotalReservedCPUUtil*100)
	GinkgoWriter.Printf("Memory Utilization: %.2f%%\n", report.TotalReservedMemoryUtil*100)
	GinkgoWriter.Printf("Efficiency Score: %.1f%%\n", report.ResourceEfficiencyScore)
	GinkgoWriter.Printf("Pods per Node: %.1f\n", report.PodsPerNode)
	GinkgoWriter.Printf("Rounds: %d\n", report.Rounds)

	// File output
	if outputDir := os.Getenv("OUTPUT_DIR"); outputDir != "" {
		reportFile := filepath.Join(outputDir, fmt.Sprintf("%s_performance_report.json", filePrefix))
		reportJSON, err := json.MarshalIndent(report, "", "  ")
		if err == nil {
			if err := os.WriteFile(reportFile, reportJSON, 0600); err == nil {
				GinkgoWriter.Printf("Report written to: %s\n", reportFile)
			}
		}
	}
	GinkgoWriter.Printf("=====================================\n")
}

// ActionType represents the type of action to be performed
type ActionType string

const (
	ActionTypeCreateDeployment ActionType = "create_deployment"
	ActionTypeUpdateReplicas   ActionType = "update_replicas"
	ActionTypeTriggerDrift     ActionType = "trigger_drift"
)

// Action represents a single action that can be executed in a performance test
type Action interface {
	Execute(env *common.Environment) error
	GetDescription() string
	GetType() ActionType
}

// ResourceProfile defines the resource requirements for pods
type ResourceProfile struct {
	CPU    string
	Memory string
}

// Predefined resource profiles
var (
	SmallResourceProfile = ResourceProfile{
		CPU:    "950m",   // 0.95 vCPU
		Memory: "3900Mi", // 3900 MB
	}
	LargeResourceProfile = ResourceProfile{
		CPU:    "3800m", // 3.8 vCPU
		Memory: "31Gi",  // 31 GB
	}
	DoNotDisruptResourceProfile = ResourceProfile{
		CPU:    "950m",  // 0.95 vCPU
		Memory: "450Mi", // 450 MB
	}
)

// CreateDeploymentAction creates a deployment with specified parameters
type CreateDeploymentAction struct {
	Name                      string
	Replicas                  int32
	ResourceProfile           ResourceProfile
	Labels                    map[string]string
	Annotations               map[string]string
	TopologySpreadConstraints []corev1.TopologySpreadConstraint
	PodAntiAffinity           *corev1.PodAntiAffinity
	NodeAffinity              *corev1.NodeAffinity
	deployment                *appsv1.Deployment // Store created deployment for cleanup
}

// NewCreateDeploymentAction creates a new deployment action
func NewCreateDeploymentAction(name string, replicas int32, profile ResourceProfile) *CreateDeploymentAction {
	return &CreateDeploymentAction{
		Name:            name,
		Replicas:        replicas,
		ResourceProfile: profile,
		Labels: map[string]string{
			"app":               name,
			test.DiscoveryLabel: "unspecified",
		},
	}
}

// NewCreateDeploymentActionWithHostnameSpread creates a deployment action with hostname topology spreading
func NewCreateDeploymentActionWithHostnameSpread(name string, replicas int32, profile ResourceProfile) *CreateDeploymentAction {
	labels := map[string]string{
		"app":               name,
		test.DiscoveryLabel: "unspecified",
	}

	return &CreateDeploymentAction{
		Name:            name,
		Replicas:        replicas,
		ResourceProfile: profile,
		Labels:          labels,
		TopologySpreadConstraints: []corev1.TopologySpreadConstraint{
			{
				MaxSkew:           1,
				TopologyKey:       corev1.LabelHostname,
				WhenUnsatisfiable: corev1.DoNotSchedule,
				LabelSelector: &metav1.LabelSelector{
					MatchLabels: labels,
				},
			},
		},
	}
}

// NewCreateDeploymentActionWithTopologyConstraints creates a deployment action with custom topology constraints
func NewCreateDeploymentActionWithTopologyConstraints(name string, replicas int32, profile ResourceProfile, constraints []corev1.TopologySpreadConstraint) *CreateDeploymentAction {
	return &CreateDeploymentAction{
		Name:            name,
		Replicas:        replicas,
		ResourceProfile: profile,
		Labels: map[string]string{
			"app":               name,
			test.DiscoveryLabel: "unspecified",
		},
		TopologySpreadConstraints: constraints,
	}
}

// NewCreateDeploymentActionWithDoNotDisrupt creates a deployment action with do-not-disrupt annotation
func NewCreateDeploymentActionWithDoNotDisrupt(name string, replicas int32, profile ResourceProfile) *CreateDeploymentAction {
	return &CreateDeploymentAction{
		Name:            name,
		Replicas:        replicas,
		ResourceProfile: profile,
		Labels: map[string]string{
			"app":               name,
			test.DiscoveryLabel: "unspecified",
		},
		Annotations: map[string]string{
			"karpenter.sh/do-not-disrupt": "true",
		},
	}
}

// SetTopologySpreadConstraints sets topology spread constraints on the action
func (a *CreateDeploymentAction) SetTopologySpreadConstraints(constraints []corev1.TopologySpreadConstraint) *CreateDeploymentAction {
	a.TopologySpreadConstraints = constraints
	return a
}

// SetPodAntiAffinity sets pod anti-affinity on the action
func (a *CreateDeploymentAction) SetPodAntiAffinity(antiAffinity *corev1.PodAntiAffinity) *CreateDeploymentAction {
	a.PodAntiAffinity = antiAffinity
	return a
}

// SetNodeAffinity sets node affinity on the action
func (a *CreateDeploymentAction) SetNodeAffinity(nodeAffinity *corev1.NodeAffinity) *CreateDeploymentAction {
	a.NodeAffinity = nodeAffinity
	return a
}

// Execute creates the deployment
func (a *CreateDeploymentAction) Execute(env *common.Environment) error {
	resourceRequirements := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(a.ResourceProfile.CPU),
			corev1.ResourceMemory: resource.MustParse(a.ResourceProfile.Memory),
		},
	}

	podOptions := test.PodOptions{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      a.Labels,
			Annotations: a.Annotations,
		},
		ResourceRequirements: resourceRequirements,
	}

	// Add placement constraints if specified
	if len(a.TopologySpreadConstraints) > 0 {
		podOptions.TopologySpreadConstraints = a.TopologySpreadConstraints
	}

	if a.PodAntiAffinity != nil {
		// Use the PodAntiRequirements and PodAntiPreferences fields from PodOptions
		if a.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
			podOptions.PodAntiRequirements = a.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution
		}
		if a.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution != nil {
			podOptions.PodAntiPreferences = a.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution
		}
	}

	if a.NodeAffinity != nil {
		// Use the NodeRequirements and NodePreferences fields from PodOptions
		if a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
			for _, term := range a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
				podOptions.NodeRequirements = append(podOptions.NodeRequirements, term.MatchExpressions...)
			}
		}
		if a.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution != nil {
			for _, pref := range a.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
				podOptions.NodePreferences = append(podOptions.NodePreferences, pref.Preference.MatchExpressions...)
			}
		}
	}

	a.deployment = test.Deployment(test.DeploymentOptions{
		ObjectMeta: metav1.ObjectMeta{
			Name: a.Name, // Explicitly set the deployment name
		},
		Replicas:   a.Replicas,
		PodOptions: podOptions,
	})

	env.ExpectCreated(a.deployment)
	return nil
}

// GetDescription returns a human-readable description of the action
func (a *CreateDeploymentAction) GetDescription() string {
	return fmt.Sprintf("Create deployment '%s' with %d replicas (CPU: %s, Memory: %s)",
		a.Name, a.Replicas, a.ResourceProfile.CPU, a.ResourceProfile.Memory)
}

// GetType returns the action type
func (a *CreateDeploymentAction) GetType() ActionType {
	return ActionTypeCreateDeployment
}

// GetDeployment returns the created deployment (for use in subsequent actions)
func (a *CreateDeploymentAction) GetDeployment() *appsv1.Deployment {
	return a.deployment
}

// UpdateReplicasAction scales an existing deployment
type UpdateReplicasAction struct {
	DeploymentName string
	NewReplicas    int32
	deployment     *appsv1.Deployment
}

// NewUpdateReplicasAction creates a new replica update action
func NewUpdateReplicasAction(deploymentName string, newReplicas int32) *UpdateReplicasAction {
	return &UpdateReplicasAction{
		DeploymentName: deploymentName,
		NewReplicas:    newReplicas,
	}
}

// SetDeployment sets the deployment to be updated (called by the execution engine)
func (a *UpdateReplicasAction) SetDeployment(deployment *appsv1.Deployment) {
	a.deployment = deployment
}

// Execute updates the deployment replicas
func (a *UpdateReplicasAction) Execute(env *common.Environment) error {
	if a.deployment == nil {
		// Try to find the deployment in the cluster by name
		GinkgoWriter.Printf("DEBUG: Deployment '%s' not in map, looking up in cluster\n", a.DeploymentName)

		deployments := &appsv1.DeploymentList{}
		err := env.Client.List(env.Context, deployments)
		if err != nil {
			return fmt.Errorf("failed to list deployments while looking for '%s': %w", a.DeploymentName, err)
		}

		var foundDeployment *appsv1.Deployment
		for i := range deployments.Items {
			if deployments.Items[i].Name == a.DeploymentName {
				foundDeployment = &deployments.Items[i]
				break
			}
		}

		if foundDeployment == nil {
			deploymentNames := make([]string, len(deployments.Items))
			for i, dep := range deployments.Items {
				deploymentNames[i] = dep.Name
			}
			return fmt.Errorf("deployment '%s' not found in cluster - available deployments: %v", a.DeploymentName, deploymentNames)
		}

		a.deployment = foundDeployment
		//GinkgoWriter.Printf("DEBUG: Found deployment '%s' in cluster with %d replicas\n", a.DeploymentName, *foundDeployment.Spec.Replicas)
	}

	oldReplicas := int32(0)
	if a.deployment.Spec.Replicas != nil {
		oldReplicas = *a.deployment.Spec.Replicas
	}

	GinkgoWriter.Printf("DEBUG: Updating deployment '%s' from %d to %d replicas\n", a.DeploymentName, oldReplicas, a.NewReplicas)

	a.deployment.Spec.Replicas = lo.ToPtr(a.NewReplicas)
	env.ExpectUpdated(a.deployment)
	return nil
}

// GetDescription returns a human-readable description of the action
func (a *UpdateReplicasAction) GetDescription() string {
	return fmt.Sprintf("Update deployment '%s' to %d replicas", a.DeploymentName, a.NewReplicas)
}

// GetType returns the action type
func (a *UpdateReplicasAction) GetType() ActionType {
	return ActionTypeUpdateReplicas
}

// TriggerDriftAction triggers a drift scenario
type TriggerDriftAction struct {
	Description string
	DriftType   string
	nodePool    interface{} // Will be set to the actual nodePool from the test context
}

// NewTriggerDriftAction creates a new drift action
func NewTriggerDriftAction(driftType, description string) *TriggerDriftAction {
	return &TriggerDriftAction{
		DriftType:   driftType,
		Description: description,
	}
}

// SetNodePool sets the nodePool reference (called by the execution engine)
func (a *TriggerDriftAction) SetNodePool(np interface{}) {
	a.nodePool = np
}

// Execute triggers the drift scenario
func (a *TriggerDriftAction) Execute(env *common.Environment) error {
	switch a.DriftType {
	case "annotation":
		// Get all existing nodeclaims before triggering drift
		// We need to get the current count first, then get that many nodeclaims
		currentNodeClaimCount := 0
		// Try to get current nodeclaim count by checking existing ones
		existingNodeClaims := env.EventuallyExpectCreatedNodeClaimCount(">=", 0)
		currentNodeClaimCount = len(existingNodeClaims)

		if currentNodeClaimCount == 0 {
			return fmt.Errorf("no nodeclaims found to drift")
		}

		GinkgoWriter.Printf("DEBUG: Triggering drift for %d nodeclaims by updating NodePool template annotation\n", currentNodeClaimCount)

		// Get the nodePool from the global variable (we need to access it from the test context)
		// Since we can't directly access the global nodePool variable from here, we'll need to
		// pass it through the action or get it from the environment
		if a.nodePool == nil {
			return fmt.Errorf("nodePool not set - call SetNodePool() before executing")
		}

		// Cast to the correct type - we'll use interface{} and type assertion
		nodePoolPtr, ok := a.nodePool.(*v1.NodePool)
		if !ok {
			return fmt.Errorf("invalid nodePool type")
		}

		// Trigger drift by updating the NodePool template annotation
		// This follows the pattern from drift_test.go
		if nodePoolPtr.Spec.Template.Annotations == nil {
			nodePoolPtr.Spec.Template.Annotations = make(map[string]string)
		}
		nodePoolPtr.Spec.Template.Annotations["test-drift-trigger"] = fmt.Sprintf("drift-%d", time.Now().Unix())

		// Apply the update to trigger drift detection
		env.ExpectUpdated(nodePoolPtr)

		// Wait for Karpenter to detect the drift
		GinkgoWriter.Printf("DEBUG: Waiting for drift detection on %d nodeclaims\n", len(existingNodeClaims))
		env.EventuallyExpectDrifted(existingNodeClaims...)

		GinkgoWriter.Printf("DEBUG: Drift successfully triggered for %d nodeclaims\n", len(existingNodeClaims))
		return nil

	default:
		return fmt.Errorf("unsupported drift type: %s", a.DriftType)
	}
}

// GetDescription returns a human-readable description of the action
func (a *TriggerDriftAction) GetDescription() string {
	return fmt.Sprintf("Trigger drift: %s (%s)", a.DriftType, a.Description)
}

// GetType returns the action type
func (a *TriggerDriftAction) GetType() ActionType {
	return ActionTypeTriggerDrift
}

// ActionExecutionResult contains the results of executing an action
type ActionExecutionResult struct {
	Action      Action
	StartTime   time.Time
	EndTime     time.Time
	Duration    time.Duration
	Error       error
	Description string
}

// lookupDeploymentReplicasInCluster looks up current replica count for a deployment in the cluster
func lookupDeploymentReplicasInCluster(env *common.Environment, deploymentName string) int32 {
	deployments := &appsv1.DeploymentList{}
	err := env.Client.List(env.Context, deployments)
	if err != nil {
		return 0 // Return 0 if we can't lookup
	}

	for _, deployment := range deployments.Items {
		if deployment.Name == deploymentName {
			if deployment.Spec.Replicas != nil {
				return *deployment.Spec.Replicas
			}
		}
	}
	return 0 // Deployment not found
}

// detectTestType analyzes the actions to determine the test type
func detectTestType(actions []Action, deploymentMap map[string]*appsv1.Deployment, env *common.Environment) string {
	hasDrift := false
	hasScaleDown := false

	GinkgoWriter.Printf("DEBUG: Detecting test type for %d actions with %d deployments in map\n", len(actions), len(deploymentMap))

	for _, action := range actions {
		GinkgoWriter.Printf("DEBUG: Analyzing action: %s (type: %s)\n", action.GetDescription(), action.GetType())

		if action.GetType() == ActionTypeTriggerDrift {
			hasDrift = true
			GinkgoWriter.Printf("DEBUG: Found drift action - will be detected as drift test\n")
		}
		if updateAction, ok := action.(*UpdateReplicasAction); ok {
			var currentReplicas int32 = 0

			// First try deployment map
			if deployment, exists := deploymentMap[updateAction.DeploymentName]; exists {
				if deployment.Spec.Replicas != nil {
					currentReplicas = *deployment.Spec.Replicas
				}
				GinkgoWriter.Printf("DEBUG: Found '%s' in map with %d replicas\n", updateAction.DeploymentName, currentReplicas)
			} else {
				// Fallback: lookup in cluster
				currentReplicas = lookupDeploymentReplicasInCluster(env, updateAction.DeploymentName)
				GinkgoWriter.Printf("DEBUG: Looked up '%s' in cluster, found %d replicas\n", updateAction.DeploymentName, currentReplicas)
			}

			GinkgoWriter.Printf("DEBUG: Update action '%s': %d -> %d replicas\n", updateAction.DeploymentName, currentReplicas, updateAction.NewReplicas)

			if currentReplicas > 0 && updateAction.NewReplicas < currentReplicas {
				hasScaleDown = true
				GinkgoWriter.Printf("DEBUG: Found scale-down action - will be detected as consolidation test\n")
			}
		}
	}

	testType := "scale-out"
	if hasDrift {
		testType = "drift"
	} else if hasScaleDown {
		testType = "consolidation"
	}

	GinkgoWriter.Printf("DEBUG: Detected test type: '%s' (hasDrift: %t, hasScaleDown: %t)\n", testType, hasDrift, hasScaleDown)
	return testType
}

// MonitorScaleOut monitors scale-out operations (always 1 round)
func MonitorScaleOut(env *common.Environment, expectedPods int, timeout time.Duration) (*PerformanceReport, error) {
	startTime := time.Now()

	// Wait for all pods to be healthy
	allPodsSelector := labels.SelectorFromSet(map[string]string{test.DiscoveryLabel: "unspecified"})
	if expectedPods > 0 {
		env.EventuallyExpectHealthyPodCountWithTimeout(timeout, allPodsSelector, expectedPods)
	}

	totalTime := time.Since(startTime)

	// Collect metrics
	nodeCount := env.Monitor.CreatedNodeCount()
	avgCPUUtil := env.Monitor.AvgUtilization(corev1.ResourceCPU)
	avgMemUtil := env.Monitor.AvgUtilization(corev1.ResourceMemory)

	// Calculate derived metrics
	resourceEfficiencyScore := (avgCPUUtil*90 + avgMemUtil*10)
	podsPerNode := float64(0)
	if nodeCount > 0 {
		podsPerNode = float64(expectedPods) / float64(nodeCount)
	}

	return &PerformanceReport{
		TestType:                "scale-out",
		TotalPods:               expectedPods,
		TotalNodes:              nodeCount,
		TotalTime:               totalTime,
		PodsNetChange:           expectedPods,
		NodesNetChange:          nodeCount,
		TotalReservedCPUUtil:    avgCPUUtil,
		TotalReservedMemoryUtil: avgMemUtil,
		ResourceEfficiencyScore: resourceEfficiencyScore,
		PodsPerNode:             podsPerNode,
		Rounds:                  1, // Scale-out is always 1 round (For now, could come back to this)
		Timestamp:               time.Now(),
	}, nil
}

// MonitorConsolidationTest monitors consolidation operations with multiple rounds
func MonitorConsolidationTest(env *common.Environment, initialPods, finalPods, initialNodes int, timeout time.Duration) (*PerformanceReport, error) {
	startTime := time.Now()

	// Wait for pods to scale down first
	allPodsSelector := labels.SelectorFromSet(map[string]string{test.DiscoveryLabel: "unspecified"})
	if finalPods > 0 {
		GinkgoWriter.Printf("DEBUG: MonitorConsolidationTest - Waiting for %d healthy pods with selector %v, timeout: %v\n", finalPods, allPodsSelector, timeout/2)
		env.EventuallyExpectHealthyPodCountWithTimeout(timeout/2, allPodsSelector, finalPods)
		GinkgoWriter.Printf("DEBUG: MonitorConsolidationTest - Successfully reached %d healthy pods\n", finalPods)
	}

	// Monitor consolidation rounds
	consolidationRounds, _, err := MonitorConsolidation(env, initialNodes, timeout)
	if err != nil {
		// Continue even if consolidation monitoring fails
		consolidationRounds = []ConsolidationRound{}
		GinkgoWriter.Printf("DEBUG: error while monitoring consolidation %s", err)
	}

	totalTime := time.Since(startTime)

	// Collect final metrics
	finalNodes := env.Monitor.CreatedNodeCount()
	avgCPUUtil := env.Monitor.AvgUtilization(corev1.ResourceCPU)
	avgMemUtil := env.Monitor.AvgUtilization(corev1.ResourceMemory)

	// Calculate derived metrics
	resourceEfficiencyScore := (avgCPUUtil*90 + avgMemUtil*10)
	podsPerNode := float64(0)
	if finalNodes > 0 {
		podsPerNode = float64(finalPods) / float64(finalNodes)
	}

	return &PerformanceReport{
		TestType:                "consolidation",
		TotalPods:               finalPods,
		TotalNodes:              finalNodes,
		TotalTime:               totalTime,
		PodsNetChange:           finalPods - initialPods,
		NodesNetChange:          finalNodes - initialNodes,
		TotalReservedCPUUtil:    avgCPUUtil,
		TotalReservedMemoryUtil: avgMemUtil,
		ResourceEfficiencyScore: resourceEfficiencyScore,
		PodsPerNode:             podsPerNode,
		Rounds:                  len(consolidationRounds),
		Timestamp:               time.Now(),
	}, nil
}

// MonitorDrift monitors drift operations with replacement rounds
func MonitorDrift(env *common.Environment, expectedPods int, timeout time.Duration) (*PerformanceReport, error) {
	startTime := time.Now()
	initialNodeCount := env.Monitor.CreatedNodeCount()

	GinkgoWriter.Printf("DEBUG: Starting drift monitoring with %d initial nodes, expecting %d pods\n", initialNodeCount, expectedPods)

	// Track node replacement during drift
	driftRounds := 0
	lastReplacementTime := time.Now()
	driftStartTime := time.Now()

	// Monitor for node replacements during drift
	for time.Since(driftStartTime) < timeout {
		// Check if nodes are being replaced (draining/terminating)
		var drainingNodes []corev1.Node
		allNodes := env.Monitor.CreatedNodes()
		for _, node := range allNodes {
			// Check if node has draining taint or is being deleted
			if node.DeletionTimestamp != nil {
				drainingNodes = append(drainingNodes, *node)
			}
			for _, taint := range node.Spec.Taints {
				if taint.Key == "karpenter.sh/disrupted" {
					drainingNodes = append(drainingNodes, *node)
					break
				}
			}
		}

		// If we detect draining nodes, this indicates a drift replacement round
		if len(drainingNodes) > 0 {
			lastReplacementTime = time.Now()
			driftRounds++
			GinkgoWriter.Printf("DEBUG: Drift round %d detected - %d nodes draining\n", driftRounds, len(drainingNodes))

			// Wait for replacement to complete
			time.Sleep(30 * time.Second)
		}

		// Check for stability (no replacements for 2 minutes)
		if time.Since(lastReplacementTime) >= 2*time.Minute {
			GinkgoWriter.Printf("DEBUG: Drift appears stable - no replacements for 2 minutes\n")
			break
		}

		// Wait before next check
		time.Sleep(15 * time.Second)
	}

	// Ensure all pods are healthy after drift
	allPodsSelector := labels.SelectorFromSet(map[string]string{test.DiscoveryLabel: "unspecified"})
	if expectedPods > 0 {
		GinkgoWriter.Printf("DEBUG: Waiting for %d pods to be healthy after drift\n", expectedPods)
		env.EventuallyExpectHealthyPodCountWithTimeout(timeout/2, allPodsSelector, expectedPods)
	}

	totalTime := time.Since(startTime)
	finalNodeCount := env.Monitor.CreatedNodeCount()

	// Collect metrics
	avgCPUUtil := env.Monitor.AvgUtilization(corev1.ResourceCPU)
	avgMemUtil := env.Monitor.AvgUtilization(corev1.ResourceMemory)

	// Calculate derived metrics
	resourceEfficiencyScore := (avgCPUUtil*90 + avgMemUtil*10)
	podsPerNode := float64(0)
	if finalNodeCount > 0 {
		podsPerNode = float64(expectedPods) / float64(finalNodeCount)
	}

	// If no drift rounds were detected, assume at least 1 round occurred
	if driftRounds == 0 {
		driftRounds = 1
		GinkgoWriter.Printf("DEBUG: No explicit drift rounds detected, assuming 1 round\n")
	}

	GinkgoWriter.Printf("DEBUG: Drift monitoring completed - %d rounds, %v total time, %d final nodes\n",
		driftRounds, totalTime, finalNodeCount)

	return &PerformanceReport{
		TestType:                "drift",
		TotalPods:               expectedPods,
		TotalNodes:              finalNodeCount,
		TotalTime:               totalTime,
		PodsNetChange:           0,                                 // Pods don't change in drift
		NodesNetChange:          finalNodeCount - initialNodeCount, // Net change in nodes (should be ~0 for drift)
		TotalReservedCPUUtil:    avgCPUUtil,
		TotalReservedMemoryUtil: avgMemUtil,
		ResourceEfficiencyScore: resourceEfficiencyScore,
		PodsPerNode:             podsPerNode,
		Rounds:                  driftRounds,
		Timestamp:               time.Now(),
	}, nil
}

// getDeploymentNames returns a list of deployment names for debugging
func getDeploymentNames(deploymentMap map[string]*appsv1.Deployment) []string {
	names := make([]string, 0, len(deploymentMap))
	for name := range deploymentMap {
		names = append(names, name)
	}
	return names
}

// executeActions executes a list of actions and tracks deployments
func executeActions(actions []Action, env *common.Environment) (map[string]*appsv1.Deployment, int, error) {
	deploymentMap := make(map[string]*appsv1.Deployment)
	initialPodCount := 0

	//GinkgoWriter.Printf("DEBUG: Executing %d actions\n", len(actions))

	for i, action := range actions {
		//GinkgoWriter.Printf("DEBUG: Executing action %d/%d: %s\n", i+1, len(actions), action.GetDescription())

		// Handle deployment references for UpdateReplicasAction
		if updateAction, ok := action.(*UpdateReplicasAction); ok {
			availableDeployments := getDeploymentNames(deploymentMap)
			if deployment, exists := deploymentMap[updateAction.DeploymentName]; exists {
				updateAction.SetDeployment(deployment)
				//GinkgoWriter.Printf("DEBUG: Found deployment '%s' for update action\n", updateAction.DeploymentName)
			} else {
				GinkgoWriter.Printf("DEBUG: Deployment '%s' not found in map. Available deployments: %v\n",
					updateAction.DeploymentName, availableDeployments)
			}
		}

		// Handle nodePool reference for TriggerDriftAction
		// Note: nodePool must be set externally before calling executeActions
		// This is handled by ExecuteActionsAndGenerateReportWithNodePool

		// Execute the action
		err := action.Execute(env)
		if err != nil {
			availableDeployments := getDeploymentNames(deploymentMap)
			return nil, 0, fmt.Errorf("action failed [%s] at step %d/%d: %s - available deployments: %v - underlying error: %w",
				action.GetType(), i+1, len(actions), action.GetDescription(), availableDeployments, err)
		}

		// Track deployments for monitoring
		if createAction, ok := action.(*CreateDeploymentAction); ok {
			deployment := createAction.GetDeployment()
			deploymentMap[createAction.Name] = deployment
			initialPodCount += int(createAction.Replicas)
			//GinkgoWriter.Printf("DEBUG: Tracked deployment '%s' with %d replicas\n", createAction.Name, createAction.Replicas)
		}
	}

	//GinkgoWriter.Printf("DEBUG: Action execution completed. Tracked deployments: %v, Initial pod count: %d\n",
	//	getDeploymentNames(deploymentMap), initialPodCount)

	return deploymentMap, initialPodCount, nil
}

// routeToMonitoringWithType routes to appropriate monitoring function based on predetermined test type
func routeToMonitoringWithType(testType string, initialPodCount, finalPodCount, initialNodeCount int, env *common.Environment, timeOut time.Duration) (*PerformanceReport, error) {
	GinkgoWriter.Printf("DEBUG: Using predetermined test type '%s' for monitoring\n", testType)

	var report *PerformanceReport
	var err error

	switch testType {
	case "scale-out":
		report, err = MonitorScaleOut(env, finalPodCount, timeOut)
	case "consolidation":
		report, err = MonitorConsolidationTest(env, initialPodCount, finalPodCount, initialNodeCount, timeOut)
	case "drift":
		report, err = MonitorDrift(env, finalPodCount, timeOut)
	default:
		GinkgoWriter.Printf("DEBUG: Unknown predetermined test type '%s', defaulting to MonitorScaleOut\n", testType)
		report, err = MonitorScaleOut(env, finalPodCount, timeOut)
	}

	if err != nil {
		return nil, fmt.Errorf("monitoring failed for predetermined test type '%s' with %d pods (initial: %d, final: %d, timeout: %v): %w",
			testType, finalPodCount, initialPodCount, finalPodCount, timeOut, err)
	}

	return report, nil
}

// calculateInitialPodCount calculates the initial pod count based on test type
func calculateInitialPodCount(testType string, actions []Action, env *common.Environment) int {
	if testType == "consolidation" {
		// For consolidation, calculate initial pod count from cluster before updates
		initialPodCount := 0
		for _, action := range actions {
			if updateAction, ok := action.(*UpdateReplicasAction); ok {
				originalReplicas := lookupDeploymentReplicasInCluster(env, updateAction.DeploymentName)
				initialPodCount += int(originalReplicas)
			}
		}
		GinkgoWriter.Printf("DEBUG: Calculated initial pod count for consolidation from cluster: %d\n", initialPodCount)
		return initialPodCount
	}
	return 0 // Will be set from executeActions for other test types
}

// buildUpdateActionMap creates a map of deployment names to their new replica counts from update actions
func buildUpdateActionMap(actions []Action) map[string]int32 {
	updateActionMap := make(map[string]int32)
	for _, action := range actions {
		if updateAction, ok := action.(*UpdateReplicasAction); ok {
			updateActionMap[updateAction.DeploymentName] = updateAction.NewReplicas
		}
	}
	return updateActionMap
}

// calculateConsolidationPodCount calculates pod count for consolidation tests
func calculateConsolidationPodCount(actions []Action, deploymentMap map[string]*appsv1.Deployment) int {
	updateActionMap := buildUpdateActionMap(actions)
	finalPodCount := 0

	// First, add up all the update actions
	for _, newReplicas := range updateActionMap {
		finalPodCount += int(newReplicas)
	}

	// Then, check if there are any test deployments that weren't updated
	if len(deploymentMap) > 0 {
		for deploymentName, deployment := range deploymentMap {
			if _, hasUpdate := updateActionMap[deploymentName]; !hasUpdate {
				// This deployment wasn't updated, so include its current replica count
				if deployment.Spec.Replicas != nil && *deployment.Spec.Replicas > 1 {
					finalPodCount += int(*deployment.Spec.Replicas)
					GinkgoWriter.Printf("DEBUG: Including unchanged deployment '%s' with %d replicas\n", deploymentName, *deployment.Spec.Replicas)
				}
			}
		}
	}

	GinkgoWriter.Printf("DEBUG: Calculated final pod count for consolidation (updated: %d actions, total: %d pods)\n", len(updateActionMap), finalPodCount)
	return finalPodCount
}

// calculateScaleOutPodCount calculates pod count for scale-out and drift tests
func calculateScaleOutPodCount(deploymentMap map[string]*appsv1.Deployment) int {
	finalPodCount := 0
	for _, deployment := range deploymentMap {
		if deployment.Spec.Replicas != nil {
			finalPodCount += int(*deployment.Spec.Replicas)
		}
	}
	GinkgoWriter.Printf("DEBUG: Calculated final pod count from deployments: %d\n", finalPodCount)
	return finalPodCount
}

// calculateFinalPodCount calculates the final expected pod count based on test type
func calculateFinalPodCount(testType string, actions []Action, deploymentMap map[string]*appsv1.Deployment) int {
	if testType == "consolidation" {
		return calculateConsolidationPodCount(actions, deploymentMap)
	}
	return calculateScaleOutPodCount(deploymentMap)
}

// ExecuteActionsAndGenerateReport executes a list of actions and generates a performance report
func ExecuteActionsAndGenerateReport(actions []Action, testName string, env *common.Environment, timeOut time.Duration) (*PerformanceReport, error) {
	return ExecuteActionsAndGenerateReportWithNodePool(actions, testName, env, timeOut, nil)
}

// ExecuteActionsAndGenerateReportWithNodePool executes a list of actions with nodePool support and generates a performance report
func ExecuteActionsAndGenerateReportWithNodePool(actions []Action, testName string, env *common.Environment, timeOut time.Duration, nodePool *v1.NodePool) (*PerformanceReport, error) {
	// Record initial state for consolidation tests
	initialNodeCount := env.Monitor.CreatedNodeCount()

	// Set nodePool for any TriggerDriftAction instances
	for _, action := range actions {
		if driftAction, ok := action.(*TriggerDriftAction); ok {
			if nodePool != nil {
				driftAction.SetNodePool(nodePool)
			}
		}
	}

	// IMPORTANT: Detect test type BEFORE executing actions, when we can still see original replica counts
	emptyDeploymentMap := make(map[string]*appsv1.Deployment) // Empty map for initial detection
	testType := detectTestType(actions, emptyDeploymentMap, env)
	GinkgoWriter.Printf("DEBUG: Pre-execution test type detection: '%s'\n", testType)

	// Calculate initial pod count for consolidation tests BEFORE executing actions
	initialPodCount := calculateInitialPodCount(testType, actions, env)

	// Execute actions and track deployments
	deploymentMap, executionInitialPodCount, err := executeActions(actions, env)
	if err != nil {
		return nil, err
	}

	// Use execution initial pod count for non-consolidation tests
	if testType != "consolidation" {
		initialPodCount = executionInitialPodCount
	}

	// Calculate final expected pods based on test type
	finalPodCount := calculateFinalPodCount(testType, actions, deploymentMap)

	GinkgoWriter.Printf("DEBUG: Pod count summary - Initial: %d, Final: %d, Net Change: %d\n",
		initialPodCount, finalPodCount, finalPodCount-initialPodCount)

	// Route to appropriate monitoring based on pre-determined test type
	report, err := routeToMonitoringWithType(testType, initialPodCount, finalPodCount, initialNodeCount, env, timeOut)
	if err != nil {
		return nil, err
	}

	// Set the test name
	report.TestName = testName

	return report, nil
}

// MonitorConsolidation monitors node consolidation and returns consolidation rounds
func MonitorConsolidation(env *common.Environment, preScaleInNodes int, timeout time.Duration) ([]ConsolidationRound, time.Duration, error) {
	var consolidationRounds []ConsolidationRound
	roundNumber := 1
	lastDrainingTime := time.Now()
	consolidationStartTime := time.Now()

	consolidationComplete := false

	for time.Since(consolidationStartTime) < timeout && !consolidationComplete {
		currentNodes := env.Monitor.CreatedNodeCount()

		// Check if nodes are draining/terminating
		var drainingNodes []corev1.Node
		allNodes := env.Monitor.CreatedNodes()
		for _, node := range allNodes {
			// Check if node has draining taint or is being deleted
			if node.DeletionTimestamp != nil {
				drainingNodes = append(drainingNodes, *node)
			}
			for _, taint := range node.Spec.Taints {
				if taint.Key == "karpenter.sh/disrupted" {
					drainingNodes = append(drainingNodes, *node)
					break
				}
			}
		}

		// If we detect draining nodes, record this as a consolidation round
		if len(drainingNodes) > 0 {
			lastDrainingTime = time.Now()

			// Wait for this round to complete
			roundStartTime := time.Now()
			//env.EventuallyExpectNodeCount("<", currentNodes)
			time.Sleep(25 * time.Second)
			finalNodeCount := env.Monitor.CreatedNodeCount()
			roundDuration := time.Since(roundStartTime)

			round := ConsolidationRound{
				RoundNumber:   roundNumber,
				StartTime:     roundStartTime,
				Duration:      roundDuration,
				NodesRemoved:  currentNodes - finalNodeCount,
				StartingNodes: currentNodes,
				EndingNodes:   finalNodeCount,
			}
			consolidationRounds = append(consolidationRounds, round)
			roundNumber++
		}

		// Check for stability (no draining for 3 minutes)
		if time.Since(lastDrainingTime) >= 3*time.Minute {
			consolidationComplete = true
			break
		}

		// Wait before next check
		time.Sleep(30 * time.Second)
	}

	totalConsolidationTime := time.Since(consolidationStartTime)

	if !consolidationComplete {
		return consolidationRounds, totalConsolidationTime, fmt.Errorf("consolidation timeout reached after %v", timeout)
	}

	return consolidationRounds, totalConsolidationTime, nil
}

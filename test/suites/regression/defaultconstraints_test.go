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

package integration_test

import (
	_ "embed"
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
)

// schedulerConfigFile is the KubeSchedulerConfiguration hack/kind/cluster.yaml hands to the cluster's
// kube-scheduler. These specs configure Karpenter from the same file, so the two sides can't drift.
//
//go:embed testdata/kube-scheduler-config.yaml
var schedulerConfigFile []byte

const (
	// schedulerConfigMountPath is where hack/kind/cluster.yaml mounts the file above. Its presence in the
	// scheduler's arguments is how these specs recognize a cluster they can run against.
	schedulerConfigMountPath = "/etc/kubernetes/karpenter-scheduler-config/kube-scheduler-config.yaml"
	schedulerConfigEnvVar    = "SCHEDULER_CONFIG"
	// podCPURequest is a whole CPU, so the instance type Karpenter picks shows how many pods it expected per node.
	podCPURequest = "1"
	// packablePodCPURequest lets several pods share a node, so landing on one node says something about a
	// workload's constraints rather than its size.
	packablePodCPURequest = "200m"
	// numPods is more than two so packing and spreading can't produce the same node count, and no more than the
	// number of zones the provider offers.
	numPods = 4
	// spreadRoleLabel pins a workload to its own NodePool.
	spreadRoleLabel = "testing/spread-role"
)

// kubeSchedulerConfig is the subset of KubeSchedulerConfiguration these specs read, declared here because this
// repository doesn't depend on k8s.io/kubernetes.
type kubeSchedulerConfig struct {
	Profiles []struct {
		SchedulerName string `json:"schedulerName"`
		PluginConfig  []struct {
			Name string `json:"name"`
			Args struct {
				DefaultConstraints []corev1.TopologySpreadConstraint `json:"defaultConstraints"`
			} `json:"args"`
		} `json:"pluginConfig"`
	} `json:"profiles"`
}

// kube-scheduler applies its podTopologySpread.defaultConstraints to every pod that declares no constraints of
// its own and that it can deduce a label selector for. Karpenter's simulation has to agree: ignoring them
// provisions capacity kube-scheduler then refuses, and applying them too eagerly provisions a node per pod for
// workloads that were never going to be spread.
var _ = Describe("DefaultTopologySpreadConstraints", func() {
	// The name of the kube-scheduler profile carrying the cluster's defaults, and the defaults themselves.
	var schedulerName string
	var defaultConstraints []corev1.TopologySpreadConstraint
	var appLabels map[string]string
	var appSelector labels.Selector

	BeforeEach(func() {
		if !schedulerHasDefaultConstraints() {
			Skip(fmt.Sprintf("cluster's kube-scheduler isn't configured with %s, see hack/kind/cluster.yaml", schedulerConfigMountPath))
		}
		schedulerName, defaultConstraints = defaultConstraintsProfile()
		appLabels = map[string]string{"app": test.RandomName()}
		appSelector = labels.SelectorFromSet(appLabels)

		env.ExpectSettingsOverridden(corev1.EnvVar{Name: schedulerConfigEnvVar, Value: karpenterSchedulerConfig(defaultConstraints)})
		DeferCleanup(func() {
			env.ExpectSettingsRemoved(corev1.EnvVar{Name: schedulerConfigEnvVar})
		})
	})

	// The owner and the Service are independent sources for the deduced selector, so they get a spec each -
	// a regression in one would otherwise hide behind the other.
	It("should provision a node per pod for a workload its owner is the selector for", func() {
		// No Service selects these pods, so the selector comes from the ReplicaSet the deployment creates.
		dep := test.Deployment(test.DeploymentOptions{
			Replicas: numPods,
			PodOptions: test.PodOptions{
				ObjectMeta:           metav1.ObjectMeta{Labels: appLabels},
				ResourceRequirements: spreadPodRequests(podCPURequest),
			},
		})
		// Opt into the profile carrying the defaults. The pods declare no constraints of their own.
		dep.Spec.Template.Spec.SchedulerName = schedulerName

		env.ExpectCreated(nodeClass, nodePool, dep)

		expectDefaultedWorkloadSpread(appSelector)
	})

	It("should provision a node per pod for a workload a Service is the selector for", func() {
		// These pods have no controller, so only the Service can supply a selector - the half of the deduction
		// that needs Karpenter to read Services.
		pods := lo.Times(numPods, func(_ int) *corev1.Pod {
			pod := test.Pod(test.PodOptions{
				ObjectMeta:           metav1.ObjectMeta{Labels: appLabels},
				ResourceRequirements: spreadPodRequests(podCPURequest),
			})
			pod.Spec.SchedulerName = schedulerName
			return pod
		})
		svc := test.Service(test.ServiceOptions{Selector: appLabels})

		env.ExpectCreated(nodeClass, nodePool, svc)
		env.ExpectCreated(lo.Map(pods, func(p *corev1.Pod, _ int) client.Object { return p })...)

		expectDefaultedWorkloadSpread(appSelector)
	})

	It("should pack pods the cluster's defaults don't apply to", func() {
		// Owned and selected by nothing, so no selector can be deduced and neither side defaults them. This is
		// also what proves the pods above are packable, so those specs can't pass for the wrong reason.
		pods := lo.Times(numPods, func(_ int) *corev1.Pod {
			pod := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: appLabels},
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(podCPURequest)},
				},
			})
			pod.Spec.SchedulerName = schedulerName
			return pod
		})

		env.ExpectCreated(nodeClass, nodePool)
		env.ExpectCreated(lo.Map(pods, func(p *corev1.Pod, _ int) client.Object { return p })...)

		env.EventuallyExpectHealthyPodCount(appSelector, numPods)
		env.EventuallyExpectCreatedNodeCount("==", 1)
		env.EventuallyExpectUniqueNodeNames(appSelector, 1)
	})

	It("should default only the pods in a batch that declare no constraints of their own", func() {
		// A pod carrying any constraints of its own is exempt, all or nothing, and both kinds arrive in the same
		// batch here. Each workload is pinned to its own NodePool so it can't land on the other's capacity,
		// which is what makes its node count evidence about its own constraints.
		ownConstraintsNodePool := nodePool.DeepCopy()
		ownConstraintsNodePool.Name = fmt.Sprintf("%s-own", nodePool.Name)
		ownConstraintsNodePool.Spec.Template.Labels = lo.Assign(ownConstraintsNodePool.Spec.Template.Labels,
			map[string]string{spreadRoleLabel: "own-constraints"})
		nodePool.Spec.Template.Labels = lo.Assign(nodePool.Spec.Template.Labels,
			map[string]string{spreadRoleLabel: "defaulted"})

		defaultedDep := test.Deployment(test.DeploymentOptions{
			Replicas: numPods,
			PodOptions: test.PodOptions{
				ObjectMeta:           metav1.ObjectMeta{Labels: appLabels},
				NodeSelector:         map[string]string{spreadRoleLabel: "defaulted"},
				ResourceRequirements: spreadPodRequests(packablePodCPURequest),
			},
		})
		defaultedDep.Spec.Template.Spec.SchedulerName = schedulerName

		ownLabels := map[string]string{"app": test.RandomName()}
		ownSelector := labels.SelectorFromSet(ownLabels)
		ownConstraintsDep := test.Deployment(test.DeploymentOptions{
			Replicas: numPods,
			PodOptions: test.PodOptions{
				ObjectMeta:           metav1.ObjectMeta{Labels: ownLabels},
				NodeSelector:         map[string]string{spreadRoleLabel: "own-constraints"},
				ResourceRequirements: spreadPodRequests(packablePodCPURequest),
				// Tolerates co-location, so these pods can share a node - which the hostname default wouldn't
				// allow. Anything the pod declares exempts it.
				TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{
					MaxSkew:           numPods,
					TopologyKey:       corev1.LabelHostname,
					WhenUnsatisfiable: corev1.ScheduleAnyway,
					LabelSelector:     &metav1.LabelSelector{MatchLabels: ownLabels},
				}},
			},
		})
		ownConstraintsDep.Spec.Template.Spec.SchedulerName = schedulerName

		env.ExpectCreated(nodeClass, nodePool, ownConstraintsNodePool, defaultedDep, ownConstraintsDep)

		env.EventuallyExpectHealthyPodCount(appSelector, numPods)
		env.EventuallyExpectHealthyPodCount(ownSelector, numPods)
		env.EventuallyExpectUniqueNodeNames(appSelector, numPods)
		expectUniqueNodeZones(appSelector, numPods)
		// The workload that brought its own keeps them, and packs onto a single node.
		env.EventuallyExpectUniqueNodeNames(ownSelector, 1)
	})
})

func spreadPodRequests(cpuRequest string) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpuRequest)},
	}
}

// expectDefaultedWorkloadSpread asserts a defaulted workload came up the way both configured constraints
// describe: every pod running (so kube-scheduler accepted Karpenter's capacity), one pod per node, one per zone.
func expectDefaultedWorkloadSpread(selector labels.Selector) {
	GinkgoHelper()

	env.EventuallyExpectHealthyPodCount(selector, numPods)
	nodes := env.EventuallyExpectCreatedNodeCount("==", numPods)
	env.EventuallyExpectUniqueNodeNames(selector, numPods)
	// The second configured constraint: injecting only the first would still place one pod per node.
	expectUniqueNodeZones(selector, numPods)

	// Sizing only holds if the constraints were in Karpenter's simulation rather than something kube-scheduler
	// worked around afterwards: a node expected to hold two of these pods would be big enough for two.
	podCPU := resource.MustParse(podCPURequest)
	for _, node := range nodes {
		Expect(node.Status.Allocatable.Cpu().MilliValue()).To(BeNumerically("<", 2*podCPU.MilliValue()),
			fmt.Sprintf("expected node %s to be sized for a single pod", node.Name))
	}
}

// expectUniqueNodeZones asserts the pods matching the selector are spread across the given number of zones.
func expectUniqueNodeZones(selector labels.Selector, zoneCount int) {
	GinkgoHelper()

	Eventually(func(g Gomega) {
		zones := sets.New[string]()
		for _, pod := range env.Monitor.RunningPods(selector) {
			node := &corev1.Node{}
			g.Expect(env.Client.Get(env, client.ObjectKey{Name: pod.Spec.NodeName}, node)).To(Succeed())
			zones.Insert(node.Labels[corev1.LabelTopologyZone])
		}
		g.Expect(zones.UnsortedList()).To(HaveLen(zoneCount))
	}).Should(Succeed())
}

// schedulerHasDefaultConstraints reports whether the cluster's kube-scheduler was handed the testdata config.
// A cluster without it - a managed control plane, or a kind cluster created without hack/kind/cluster.yaml -
// defaults no pods, so there is nothing for Karpenter to mirror and these specs skip.
func schedulerHasDefaultConstraints() bool {
	GinkgoHelper()

	podList := &corev1.PodList{}
	Expect(env.Client.List(env, podList, client.InNamespace("kube-system"), client.MatchingLabels{"component": "kube-scheduler"})).To(Succeed())
	return lo.ContainsBy(podList.Items, func(p corev1.Pod) bool {
		return lo.ContainsBy(p.Spec.Containers, func(c corev1.Container) bool {
			return lo.ContainsBy(append(c.Command, c.Args...), func(arg string) bool {
				return strings.Contains(arg, schedulerConfigMountPath)
			})
		})
	})
}

// defaultConstraintsProfile returns the profile carrying podTopologySpread.defaultConstraints and those
// constraints.
func defaultConstraintsProfile() (string, []corev1.TopologySpreadConstraint) {
	GinkgoHelper()

	config := &kubeSchedulerConfig{}
	Expect(yaml.Unmarshal(schedulerConfigFile, config)).To(Succeed())
	for _, profile := range config.Profiles {
		for _, pluginConfig := range profile.PluginConfig {
			if pluginConfig.Name == "PodTopologySpread" && len(pluginConfig.Args.DefaultConstraints) != 0 {
				return profile.SchedulerName, pluginConfig.Args.DefaultConstraints
			}
		}
	}
	Fail("expected the embedded kube-scheduler configuration to carry podTopologySpread.defaultConstraints")
	return "", nil
}

// karpenterSchedulerConfig renders the constraints as Karpenter's --scheduler-config document. Karpenter fails
// fast on a malformed one, so it's validated here rather than surfacing as a controller that won't come back.
func karpenterSchedulerConfig(constraints []corev1.TopologySpreadConstraint) string {
	GinkgoHelper()

	raw := string(lo.Must(yaml.Marshal(options.SchedulerConfiguration{
		PodTopologySpread: &options.PodTopologySpreadConfig{DefaultConstraints: constraints},
	})))
	config, err := options.ParseSchedulerConfiguration(raw)
	Expect(err).ToNot(HaveOccurred())
	Expect(config.PodTopologySpread.DefaultConstraints).To(Equal(constraints))
	return raw
}

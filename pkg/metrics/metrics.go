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
	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	nodeSubsystem      = "nodes"
	machineSubsystem   = "machines"
	nodeClaimSubsystem = "nodeclaims"
)

var (
	NodeClaimsCreatedCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: Namespace,
			Subsystem: nodeClaimSubsystem,
			Name:      "created",
			Help:      "Number of machines created in total by Karpenter. Labeled by reason the machine was created and the owning provisioner.",
		},
		[]string{
			ReasonLabel,
			NodePoolLabel,
		},
	)
	NodeClaimsTerminatedCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: Namespace,
			Subsystem: nodeClaimSubsystem,
			Name:      "terminated",
			Help:      "Number of machines terminated in total by Karpenter. Labeled by reason the machine was terminated.",
		},
		[]string{
			ReasonLabel,
			NodePoolLabel,
		},
	)
	NodeClaimsLaunchedCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: Namespace,
			Subsystem: nodeClaimSubsystem,
			Name:      "launched",
			Help:      "Number of machines launched in total by Karpenter. Labeled by the owning provisioner.",
		},
		[]string{
			NodePoolLabel,
		},
	)
	NodeClaimsRegisteredCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: Namespace,
			Subsystem: nodeClaimSubsystem,
			Name:      "registered",
			Help:      "Number of machines registered in total by Karpenter. Labeled by the owning provisioner.",
		},
		[]string{
			NodePoolLabel,
		},
	)
	NodeClaimsInitializedCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: Namespace,
			Subsystem: nodeClaimSubsystem,
			Name:      "initialized",
			Help:      "Number of machines initialized in total by Karpenter. Labeled by the owning provisioner.",
		},
		[]string{
			NodePoolLabel,
		},
	)
	NodesCreatedCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: Namespace,
			Subsystem: nodeSubsystem,
			Name:      "created",
			Help:      "Number of nodes created in total by Karpenter. Labeled by owning provisioner.",
		},
		[]string{
			NodePoolLabel,
			ProvisionerLabel,
		},
	)
	NodesTerminatedCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: Namespace,
			Subsystem: nodeSubsystem,
			Name:      "terminated",
			Help:      "Number of nodes terminated in total by Karpenter. Labeled by owning provisioner.",
		},
		[]string{
			NodePoolLabel,
			ProvisionerLabel,
		},
	)
	// TODO @joinnis: Remove these metrics when dropping v1alpha5 and no longer supporting Machines
	MachinesCreatedCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: Namespace,
			Subsystem: machineSubsystem,
			Name:      "created",
			Help:      "Number of machines created in total by Karpenter. Labeled by reason the machine was created and the owning provisioner.",
		},
		[]string{
			ReasonLabel,
			ProvisionerLabel,
		},
	)
	MachinesTerminatedCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: Namespace,
			Subsystem: machineSubsystem,
			Name:      "terminated",
			Help:      "Number of machines terminated in total by Karpenter. Labeled by reason the machine was terminated.",
		},
		[]string{
			ReasonLabel,
			ProvisionerLabel,
		},
	)
	MachinesLaunchedCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: Namespace,
			Subsystem: machineSubsystem,
			Name:      "launched",
			Help:      "Number of machines launched in total by Karpenter. Labeled by the owning provisioner.",
		},
		[]string{
			ProvisionerLabel,
		},
	)
	MachinesRegisteredCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: Namespace,
			Subsystem: machineSubsystem,
			Name:      "registered",
			Help:      "Number of machines registered in total by Karpenter. Labeled by the owning provisioner.",
		},
		[]string{
			ProvisionerLabel,
		},
	)
	MachinesInitializedCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: Namespace,
			Subsystem: machineSubsystem,
			Name:      "initialized",
			Help:      "Number of machines initialized in total by Karpenter. Labeled by the owning provisioner.",
		},
		[]string{
			ProvisionerLabel,
		},
	)
)

func init() {
	crmetrics.Registry.MustRegister(NodeClaimsCreatedCounter, NodeClaimsTerminatedCounter, NodeClaimsLaunchedCounter,
		NodeClaimsRegisteredCounter, NodeClaimsInitializedCounter, MachinesCreatedCounter, MachinesTerminatedCounter,
		MachinesLaunchedCounter, MachinesRegisteredCounter, MachinesInitializedCounter, NodesCreatedCounter, NodesTerminatedCounter)
}

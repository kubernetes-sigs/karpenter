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
	NodeSubsystem    = "nodes"
	machineSubsystem = "machines"
)

var (
	NodesCreatedCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: Namespace,
			Subsystem: NodeSubsystem,
			Name:      "created",
			Help:      "Number of nodes created in total by Karpenter. Labeled by reason the node was created and the owning provisioner.",
		},
		[]string{
			ReasonLabel,
			ProvisionerLabel,
		},
	)
	NodesTerminatedCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: Namespace,
			Subsystem: NodeSubsystem,
			Name:      "terminated",
			Help:      "Number of nodes terminated in total by Karpenter. Labeled by reason the node was terminated and the owning provisioner.",
		},
		[]string{
			ReasonLabel,
			ProvisionerLabel,
		},
	)
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
			Help:      "Number of machines terminated in total by Karpenter. Labeled by reason the machine was terminated and the owning provisioner.",
		},
		[]string{
			ReasonLabel,
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

func MustRegister() {
	crmetrics.Registry.MustRegister(NodesCreatedCounter, NodesTerminatedCounter)
}

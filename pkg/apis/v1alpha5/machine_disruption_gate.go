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

package v1alpha5

import (
	"context"
	"fmt"

	"github.com/robfig/cron/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/clock"
	"knative.dev/pkg/logging"
)

var AllowedActions sets.String = sets.NewString("Consolidation", "Emptiness", "Expiration", "Drift")
var CrontabTemplateFormat = "TZ=UTC %s"

// MachineDisruptionGateSpec is the top level field for MachineDisruptionGates.
type MachineDisruptionGateSpec struct {
	// Schedules represent times where disruption is prohibited by Karpenter.
	// Schedules will be parsed in UTC. If the current time is captured
	// by any of the schedules, the Machine Disruption Gate is considered active.
	Schedules []Schedule `json:"schedules"`
	// Actions are the well-known deprovisioning actions that cannot be executed by Karpenter.
	Actions []string `json:"actions"`
	// Selector is a label selector for nodes that Karpenter is prohbited from disrupting.
	// Omitting selector will select all Karpenter nodes.
	//+optional
	Selector *metav1.LabelSelector `json:"selector,omitempty"`
}

// Schedule represents the windows in which a Machine Disruption Gate is active.
type Schedule struct {
	// Crontab represents the start of the schedule at each crontab hit.
	// Crontab should be formatted as (Minute | Hour | Day of Month | Month | Day of Week)
	// Crontabs are parsed in UTC.
	Crontab string `json:"crontab"`
	// Duration represents how long the schedule will be active since the last crontab hit.
	Duration metav1.Duration `json:"duration"`
}

// MachineDisruptionGate is the Schema for the MachineDisruptionGates API
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=machinedisruptiongates,scope=Cluster,categories=karpenter
// +kubebuilder:subresource:status
type MachineDisruptionGate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MachineDisruptionGateSpec   `json:"spec,omitempty"`
	Status MachineDisruptionGateStatus `json:"status,omitempty"`
}

// MachineDisruptionGateList contains a list of MachineDisruptionGates
// +kubebuilder:object:root=true
type MachineDisruptionGateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MachineDisruptionGate `json:"items"`
}

func (m *MachineDisruptionGate) IsGateActive(ctx context.Context, clk clock.Clock) bool {
	schedules := m.Spec.Schedules
	p := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

	for _, s := range schedules {
		schedule, err := p.Parse(fmt.Sprintf(CrontabTemplateFormat, s.Crontab))
		// This shouldn't happen as we validate this on creation.
		// This would only happen if validation was bypassed.
		if err != nil {
			logging.FromContext(ctx).Debugf("validation bypassed, invalid schedule crontab, %w", err)
			return false
		}
		// Walk back in time for the duration of the schedule. If the next crontab hit
		// doesn't occur before or at the current time, then we know the schedule can't be active.
		if !schedule.Next(clk.Now().Add(-s.Duration.Duration)).After(clk.Now()) {
			return true
		}
	}
	return false
}

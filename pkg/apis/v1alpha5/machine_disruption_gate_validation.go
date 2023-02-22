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
	"knative.dev/pkg/apis"
)

// Validate the MachineDisruptionGate
func (m *MachineDisruptionGate) Validate(_ context.Context) (errs *apis.FieldError) {
	p := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	for i, s := range m.Spec.Schedules {
		if _, err := p.Parse(fmt.Sprintf(CrontabTemplateFormat, s.Crontab)); err != nil {
			errs = errs.Also(apis.ErrInvalidValue(fmt.Sprintf("crontab %s", s.Crontab), fmt.Sprintf("schedules[%d]", i), err.Error()))
		}
	}
	for i, a := range m.Spec.Actions {
		if !AllowedActions.Has(a) {
			errs = errs.Also(apis.ErrInvalidValue(fmt.Sprintf("action %s", a), fmt.Sprintf("actions[%d]", i), "found an unsupported action"))
		}
	}
	return
}

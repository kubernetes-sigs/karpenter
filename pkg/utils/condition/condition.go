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

package condition

import (
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"knative.dev/pkg/apis"
)

type Manager struct {
	apis.ConditionManager
}

func NewManager(m apis.ConditionManager) apis.ConditionManager {
	return &Manager{
		ConditionManager: m,
	}
}

// MarkTrue sets the status of t to true, and then marks the
func (c *Manager) MarkTrue(t apis.ConditionType) {
	stored := c.GetCondition(t)
	if stored == nil {
		c.ConditionManager.MarkTrue(t)
		return
	}

	cond := stored.DeepCopy()
	cond.Status = v1.ConditionTrue

	if !equality.Semantic.DeepEqual(stored, cond) {
		c.ConditionManager.MarkTrue(t)
	}
}

func (c *Manager) MarkTrueWithReason(t apis.ConditionType, reason, messageFormat string, messageA ...interface{}) {
	stored := c.GetCondition(t)
	if stored == nil {
		c.ConditionManager.MarkUnknown(t, reason, messageFormat, messageA)
		return
	}

	cond := stored.DeepCopy()
	cond.Status = v1.ConditionTrue
	cond.Reason = reason
	cond.Message = fmt.Sprintf(messageFormat, messageA...)

	if !equality.Semantic.DeepEqual(stored, cond) {
		c.ConditionManager.MarkUnknown(t, reason, messageFormat, messageA...)
	}
}

func (c *Manager) MarkUnknown(t apis.ConditionType, reason, messageFormat string, messageA ...interface{}) {
	stored := c.GetCondition(t)
	if stored == nil {
		c.ConditionManager.MarkUnknown(t, reason, messageFormat, messageA...)
		return
	}

	cond := stored.DeepCopy()
	cond.Status = v1.ConditionUnknown
	cond.Reason = reason
	cond.Message = fmt.Sprintf(messageFormat, messageA...)

	if !equality.Semantic.DeepEqual(stored, cond) {
		c.ConditionManager.MarkUnknown(t, reason, messageFormat, messageA...)
	}
}

func (c *Manager) MarkFalse(t apis.ConditionType, reason, messageFormat string, messageA ...interface{}) {
	stored := c.GetCondition(t)
	if stored == nil {
		c.ConditionManager.MarkUnknown(t, reason, messageFormat, messageA...)
		return
	}

	cond := stored.DeepCopy()
	cond.Status = v1.ConditionFalse
	cond.Reason = reason
	cond.Message = fmt.Sprintf(messageFormat, messageA...)

	if !equality.Semantic.DeepEqual(stored, cond) {
		c.ConditionManager.MarkUnknown(t, reason, messageFormat, messageA...)
	}
}

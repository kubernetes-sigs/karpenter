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

package options

import (
	"context"
	"flag"
)

// Injectable defines a set of flag based options to be parsed and injected
// into Karpenter's contexts
type Injectable interface {
	// AddFlags adds the injectable's flags to karpenter's flag set
	AddFlags(*flag.FlagSet)
	// Parse parses the flag set and handles any required post-processing on
	// the flags
	Parse(*flag.FlagSet, ...string) error
	// ToContext injects the callee into the given context
	ToContext(context.Context) context.Context
	// MergeSettings extracts settings from the given context and merges them
	// with the options in-place. Values that were previously set by CLI flags
	// or environment variables take precedent over settings.
	// TODO: Remove with karpenter-global-settings
	MergeSettings(context.Context)
}

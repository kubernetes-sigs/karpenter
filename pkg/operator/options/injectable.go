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
// type Injectable interface {
// 	// Inject is used to inject the Options into the context and set up flags.
// 	// Called before flag.Parse().
// 	Inject(context.Context) context.Context
//
// 	// Validate is called after flag.Parse and should validate the values of
// 	// individual fields as well as do any required post-processing.
// 	Validate(context.Context) error
//
// 	MergeSettings(context.Context) context.Context
// }

type Injectable interface {
	AddFlags(*flag.FlagSet)
	Parse(*flag.FlagSet, ...string) error
	ToContext(context.Context) context.Context
	MergeSettings(context.Context)
}

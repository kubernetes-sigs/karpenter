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

package provisioning

type LaunchOptions struct {
	// RecordPodNomination causes nominate pod events to be recorded against the node.
	RecordPodNomination bool
}

type LaunchOption func(LaunchOptions) LaunchOptions

func RecordPodNomination(o LaunchOptions) LaunchOptions {
	o.RecordPodNomination = true
	return o
}

func ResolveOptions(opts ...LaunchOption) LaunchOptions {
	o := LaunchOptions{}
	for _, opt := range opts {
		if opt != nil {
			o = opt(o)
		}
	}
	return o
}

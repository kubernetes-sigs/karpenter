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

// Command labelsbind runs the labelsbind analyzer: go run sigs.k8s.io/karpenter/hack/tools/labelsbind/cmd/labelsbind ./...
package main

import (
	"golang.org/x/tools/go/analysis/singlechecker"

	"sigs.k8s.io/karpenter/hack/tools/labelsbind"
)

func main() { singlechecker.Main(labelsbind.Analyzer) }

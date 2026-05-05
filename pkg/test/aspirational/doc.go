//go:build test_aspirational

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

// Package aspirational contains tests that document desired behavior not yet
// implemented. These are expected to fail on the current codebase. When one
// starts passing, it means the underlying issue has been fixed.
//
// Build tag: test_aspirational
// Run: go test -tags=test_aspirational -v ./pkg/test/aspirational/
package aspirational

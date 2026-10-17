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

package common

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteArtifactUnderCreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not", "yet", "created")
	path, err := WriteArtifactUnder(dir, "report.json", []byte("{}"))
	if err != nil {
		t.Fatalf("WriteArtifactUnder() error = %v", err)
	}
	if want := filepath.Join(dir, "report.json"); path != want {
		t.Errorf("WriteArtifactUnder() path = %q, want %q", path, want)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "{}" {
		t.Errorf("ReadFile(%q) = %q, %v; want \"{}\"", path, data, err)
	}
}

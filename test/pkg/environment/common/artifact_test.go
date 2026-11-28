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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("WriteArtifactUnder", func() {
	It("creates the directory before writing", func() {
		dir := filepath.Join(GinkgoT().TempDir(), "not", "yet", "created")
		path, err := WriteArtifactUnder(dir, "report.json", []byte("{}"))
		Expect(err).ToNot(HaveOccurred())
		Expect(path).To(Equal(filepath.Join(dir, "report.json")))
		Expect(os.ReadFile(path)).To(Equal([]byte("{}")))
	})
})

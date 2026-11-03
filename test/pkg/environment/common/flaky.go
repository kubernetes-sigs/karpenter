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
	"fmt"
	"os"
	"regexp"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	"github.com/samber/lo"
)

type knownFlake struct {
	pattern *regexp.Regexp
	reason  string
}

func knownFlakesFromEnv() []knownFlake {
	path, ok := os.LookupEnv("KARPENTER_E2E_FLAKY_FILE")
	if !ok {
		return nil
	}
	var res []knownFlake
	for line := range strings.SplitSeq(string(lo.Must(os.ReadFile(path))), "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		pattern, reason, _ := strings.Cut(line, "\t")
		res = append(res, knownFlake{pattern: regexp.MustCompile(pattern), reason: strings.TrimSpace(reason)})
	}
	return res
}

func (env *Environment) SkipKnownFlaky() {
	GinkgoHelper()
	text := CurrentSpecReport().FullText()
	for _, f := range env.knownFlakes {
		if f.pattern.MatchString(text) {
			Skip(fmt.Sprintf("[flaky] %s", f.reason))
		}
	}
}

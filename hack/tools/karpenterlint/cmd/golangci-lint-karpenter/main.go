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

package main

import (
	"cmp"
	"fmt"
	"os"
	"regexp"
	"runtime/debug"
	"strings"

	"github.com/golangci/golangci-lint/v2/pkg/commands"
	"github.com/golangci/golangci-lint/v2/pkg/exitcodes"
)

var (
	goVersion = "unknown"

	// Populated by goreleaser during build.
	version = "unknown"
	commit  = "?"
	date    = ""
)

func main() {
	info := createBuildInfo()

	setBuildVersion(&info)

	if err := commands.Execute(info); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Failed executing command with error: %v\n", err)
		os.Exit(exitcodes.Failure)
	}
}

func createBuildInfo() commands.BuildInfo {
	info := commands.BuildInfo{
		Commit:    commit,
		Version:   version,
		GoVersion: goVersion,
		Date:      date,
	}

	buildInfo, available := debug.ReadBuildInfo()
	if !available {
		return info
	}

	info.GoVersion = buildInfo.GoVersion

	if date != "" {
		return info
	}

	info.Version = buildInfo.Main.Version

	matched, _ := regexp.MatchString(`v\d+\.\d+\.\d+`, buildInfo.Main.Version)
	if matched {
		info.Version = strings.TrimPrefix(buildInfo.Main.Version, "v")
	}

	var revision, modified string

	for _, setting := range buildInfo.Settings {
		// The `vcs.xxx` information is only available with `go build`.
		// This information is not available with `go install` or `go run`.
		switch setting.Key {
		case "vcs.time":
			info.Date = setting.Value
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value
		}
	}

	info.Date = cmp.Or(info.Date, "(unknown)")

	info.Commit = fmt.Sprintf("(%s, modified: %s, mod sum: %q)",
		cmp.Or(revision, "unknown"), cmp.Or(modified, "?"), buildInfo.Main.Sum)

	return info
}

// Import path of golangci-lint.
const golangciPath = "github.com/golangci/golangci-lint/v2"

// setBuildVersion sets a build version message naming the bundled plugins.
// It fetches the golangci-lint version from the build dependencies.
// Then appends the local module version.
func setBuildVersion(info *commands.BuildInfo) {
	golangciVersion := "unknown"

	buildInfo, available := debug.ReadBuildInfo()
	if available {
		for _, dep := range buildInfo.Deps {
			if dep.Path == golangciPath {
				golangciVersion = dep.Version
				break
			}
		}
	}

	matched, _ := regexp.MatchString(`v\d+\.\d+\.\d+`, golangciVersion)
	if matched {
		golangciVersion = strings.TrimPrefix(golangciVersion, "v")
	}

	info.Version = fmt.Sprintf("%s, with kube-api-linter and karpenterlint at %s", golangciVersion, info.Version)
}

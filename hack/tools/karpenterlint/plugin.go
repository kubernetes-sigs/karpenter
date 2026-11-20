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

// Package karpenterlint is a golangci-lint module plugin carrying Karpenter
// specific analyzers. Each analyzer targets a class of regression that has
// shipped more than once; its package doc names the issues and PRs behind it.
package karpenterlint

import (
	"fmt"
	"slices"

	"github.com/golangci/plugin-module-register/register"
	"golang.org/x/tools/go/analysis"

	"sigs.k8s.io/karpenter/hack/tools/karpenterlint/internal/analyzers"
)

func init() {
	register.Plugin("karpenterlint", New)
}

// Settings is the plugin configuration from .golangci.yaml.
type Settings struct {
	// Disable lists analyzer names to skip.
	Disable []string `json:"disable"`
}

type plugin struct {
	settings Settings
}

func New(raw any) (register.LinterPlugin, error) {
	s, err := register.DecodeSettings[Settings](raw)
	if err != nil {
		return nil, fmt.Errorf("decoding karpenterlint settings, %w", err)
	}
	return &plugin{settings: s}, nil
}

func (p *plugin) BuildAnalyzers() ([]*analysis.Analyzer, error) {
	all := analyzers.All()
	for _, name := range p.settings.Disable {
		if !slices.ContainsFunc(all, func(a *analysis.Analyzer) bool { return a.Name == name }) {
			return nil, fmt.Errorf("unknown karpenterlint analyzer %q", name)
		}
	}
	return slices.DeleteFunc(all, func(a *analysis.Analyzer) bool {
		return slices.Contains(p.settings.Disable, a.Name)
	}), nil
}

func (p *plugin) GetLoadMode() string {
	return register.LoadModeTypesInfo
}

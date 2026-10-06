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

package options_test

import (
	"reflect"

	"github.com/awslabs/operatorpkg/docs"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"

	"sigs.k8s.io/karpenter/pkg/operator/options"
)

var _ = Describe("KarpenterFeatureGates", func() {
	It("should document every feature gate", func() {
		for _, gate := range options.KarpenterFeatureGates {
			Expect(gate.Name).ToNot(BeEmpty())
			Expect(gate.Help).ToNot(BeEmpty(), gate.Name)
			Expect(gate.Stage).To(BeElementOf(docs.Alpha, docs.Beta, docs.GA), gate.Name)
		}
	})
	It("should document every FeatureGates field", func() {
		fields := lo.FilterMap(reflect.VisibleFields(reflect.TypeOf(options.FeatureGates{})), func(f reflect.StructField, _ int) (string, bool) {
			return f.Name, f.IsExported()
		})
		Expect(lo.Map(options.KarpenterFeatureGates, func(g options.FeatureGate, _ int) string { return g.Name })).To(ConsistOf(fields))
	})
	It("should default every feature gate to its documented default", func() {
		defaults := reflect.ValueOf(options.DefaultFeatureGates())
		for _, gate := range options.KarpenterFeatureGates {
			Expect(defaults.FieldByName(gate.Name).Bool()).To(Equal(gate.Default), gate.Name)
		}
	})
	It("should parse every feature gate by its documented name", func() {
		for _, gate := range options.KarpenterFeatureGates {
			gates, err := options.ParseFeatureGates(gate.Name + "=" + lo.Ternary(gate.Default, "false", "true"))
			Expect(err).ToNot(HaveOccurred())
			Expect(reflect.ValueOf(gates).FieldByName(gate.Name).Bool()).To(Equal(!gate.Default), gate.Name)
		}
	})
})

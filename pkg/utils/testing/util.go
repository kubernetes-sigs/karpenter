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

package testing

import (
	"context"
	"strings"
	"sync"

	"github.com/go-logr/logr/funcr"
	"github.com/go-logr/zapr"
	"github.com/samber/lo"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// TestContextWithLogger returns a context with a logger to be used in tests
func TestContextWithLogger(t zaptest.TestingT) context.Context {
	opts := zaptest.WrapOptions(
		zap.AddCaller(),
		zap.Development(),
	)
	return log.IntoContext(context.Background(), zapr.NewLogger(zaptest.NewLogger(t, opts)))
}

// CaptureLogs returns a context that keeps its default-level log lines in memory, and a function that returns the lines
// so far that contain substr. Each line is funcr's text for one log call. Code under test may log from many goroutines.
func CaptureLogs(ctx context.Context, substr string) (context.Context, func() []string) {
	var mu sync.Mutex
	var lines []string
	logger := funcr.New(func(_, args string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, args)
	}, funcr.Options{})
	return log.IntoContext(ctx, logger), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return lo.Filter(lines, func(line string, _ int) bool { return strings.Contains(line, substr) })
	}
}

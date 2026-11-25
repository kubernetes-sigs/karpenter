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
	"strings"

	"go.uber.org/zap/zaptest"
)

// safeTestingT is a wrapper around zaptest.TestingT that suppresses the specific panic that occurs when a goroutine
// attempts to log after the test has completed. Suppressing these should reduce levels of test flakiness; in most
// cases, goroutines DO shut down in time, but not always. Only this specific panic is suppressed, so if there are other
// issues with logging after test completion, those will still be surfaced.
type safeTestingT struct {
	// inner is the TestingT that we're wrapping.
	inner zaptest.TestingT

	// muted is true if we've previously seen a panic (once we've seen one, we know that the test has completed, so we
	// can avoid all future panics).
	muted bool
}

var _ zaptest.TestingT = &safeTestingT{}

// newSafeTestingT returns a new safeTestingT that wraps the given zaptest.TestingT.
func newSafeTestingT(inner zaptest.TestingT) *safeTestingT {
	return &safeTestingT{
		inner: inner,
	}
}

// Logf logs the given message without failing the test.
func (t *safeTestingT) Logf(format string, args ...interface{}) {
	if t.muted {
		// We've previously seen a panic, so we know the test has completed. Don't even try to log.
		return
	}

	defer func() {
		if r := recover(); r != nil {
			if t.suppressPanic(r) {
				t.muted = true
			} else {
				panic(r)
			}
		}
	}()

	t.inner.Logf(format, args...)
}

// Errorf logs the given message and marks the test as failed.
func (t *safeTestingT) Errorf(format string, args ...interface{}) {
	if t.muted {
		// We've previously seen a panic, so we know the test has completed. Don't even try to log.
		return
	}

	defer func() {
		if r := recover(); r != nil {
			if t.suppressPanic(r) {
				t.muted = true
			} else {
				panic(r)
			}
		}
	}()

	t.inner.Errorf(format, args...)
}

// Fail marks the test as failed.
func (t *safeTestingT) Fail() {
	t.inner.Fail()
}

// FailNow marks the test as failed and stops its execution.
func (t *safeTestingT) FailNow() {
	t.inner.FailNow()
}

// Failed returns true if the test has been marked as failed.
func (t *safeTestingT) Failed() bool {
	return t.inner.Failed()
}

// Name returns the name of the test.
func (t *safeTestingT) Name() string {
	return t.inner.Name()
}

/*
 * In the standard library we find this code raising the panic we're trying to suppress in testing.go@1030
 *
 * 	if n == nil {
 *		// The test and all its parents are done. The log cannot be output.
 *		panic("Log in goroutine after " + c.name + " has completed: " + s)
 *	}
 *
 * We can use this message to identify the specific panic we're trying to suppress, and ignore it.
 */

// suppressPanic returns true if the given panic is the specific panic we're trying to suppress, false otherwise.
func (t *safeTestingT) suppressPanic(r any) bool {
	s, ok := r.(string)
	return ok && strings.Contains(s, "Log in goroutine")
}

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
	"testing"

	. "github.com/onsi/gomega"

	"go.uber.org/zap/zaptest"
)

func TestSafeTestingT_Logf_SuppressesExpectedPanic(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		logMessage    string
		errorMessage  string
		panicPayload  any
		expectedPanic string
	}{
		"When no panic, Logf works normally": {
			logMessage: "This is a log message",
		},
		"When panic due to test completion, Logf panic is suppressed": {
			logMessage:   "This is a log message",
			panicPayload: "Log in goroutine after test completed",
		},
		"When panic due to other reasons, Logf panic is not suppressed": {
			logMessage:    "This is a log message",
			panicPayload:  "Unexpected panic",
			expectedPanic: "Unexpected panic",
		},
		"When no panic, Errorf works normally": {
			errorMessage: "This is a log message",
		},
		"When panic due to test completion, Errorf panic is suppressed": {
			errorMessage: "This is a log message",
			panicPayload: "Log in goroutine after test completed",
		},
		"When panic due to other reasons, Errorf panic is not suppressed": {
			errorMessage:  "This is a log message",
			panicPayload:  "Unexpected panic",
			expectedPanic: "Unexpected panic",
		},
	}

	for n, c := range cases {
		t.Run(n, func(t *testing.T) {
			t.Parallel()
			g := NewWithT(t)

			unsafe := &panickingTestingT{
				payload: c.panicPayload,
			}

			safe := newSafeTestingT(unsafe)

			var fn func()
			if c.logMessage != "" {
				fn = func() {
					safe.Logf(c.logMessage)
				}
			} else if c.errorMessage != "" {
				fn = func() {
					safe.Errorf(c.errorMessage)
				}
			}

			if c.expectedPanic == "" {
				g.Expect(fn).ToNot(Panic())
			} else {
				g.Expect(fn).To(PanicWith(c.expectedPanic))
			}
		})
	}
}

// panickingTestingT is a zaptest.TestingT that panics on all log method calls.
type panickingTestingT struct {
	payload any
}

var _ zaptest.TestingT = &panickingTestingT{}

// Errorf implements [zaptest.TestingT].
func (p *panickingTestingT) Errorf(string, ...interface{}) {
	if p.payload != nil {
		panic(p.payload)
	}
}

// Fail implements [zaptest.TestingT].
func (p *panickingTestingT) Fail() {
	// Nothing
}

// FailNow implements [zaptest.TestingT].
func (p *panickingTestingT) FailNow() {
	// Nothing
}

// Failed implements [zaptest.TestingT].
func (p *panickingTestingT) Failed() bool {
	return false
}

// Logf implements [zaptest.TestingT].
func (p *panickingTestingT) Logf(string, ...interface{}) {
	if p.payload != nil {
		panic(p.payload)
	}
}

// Name implements [zaptest.TestingT].
func (p *panickingTestingT) Name() string {
	return "panickingTestingT"
}

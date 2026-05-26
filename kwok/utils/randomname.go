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

package utils

import (
	"fmt"
	"strings"
	"sync"

	"github.com/Pallinder/go-randomdata"
)

var (
	sequentialNumber     = 0
	sequentialNumberLock = new(sync.Mutex)
)

// RandomName generates a unique random name safe for use in non-test binaries.
// This is equivalent to test.RandomName() but avoids importing the test package,
// which transitively pulls in envtest and causes init() panics in read-only
// filesystem environments.
func RandomName() string {
	sequentialNumberLock.Lock()
	defer sequentialNumberLock.Unlock()
	sequentialNumber++
	return strings.ToLower(fmt.Sprintf("%s-%d-%s", randomdata.SillyName(), sequentialNumber, randomdata.Alphanumeric(10)))
}

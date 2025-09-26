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

package ringbuffer

type Buffer[T any] struct {
	values       []T
	currentIndex int
}

func NewBuffer[T any](capacity int) *Buffer[T] {
	return &Buffer[T]{
		values: make([]T, 0, capacity),
	}
}

func (b *Buffer[T]) Insert(value T) {
	// If buffer is not full, append the new value
	if len(b.values) < cap(b.values) {
		b.values = append(b.values, value)
		return
	}
	// If buffer is full, replace the oldest entry
	b.values[b.currentIndex] = value
	b.currentIndex = (b.currentIndex + 1) % cap(b.values)
}

func (b *Buffer[T]) Len() int {
	return len(b.values)
}

func (b *Buffer[T]) Reset() {
	b.values = b.values[:0]
	b.currentIndex = 0
}

func (b *Buffer[T]) GetItems() []T {
	return b.values
}

func (b *Buffer[T]) Capacity() int {
	return cap(b.values)
}

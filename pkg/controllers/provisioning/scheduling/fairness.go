package scheduling

import (
	"sync"

	"k8s.io/apimachinery/pkg/types"
)

// FairnessState tracks scheduling retry attempts for pods across
// provisioning cycles. Pods with more previous failures are prioritized
// during subsequent scheduling attempts.
type FairnessState struct {
	mu       sync.RWMutex
	attempts map[types.UID]uint32
}

// NewFairnessState creates a new FairnessState.
func NewFairnessState() *FairnessState {
	return &FairnessState{
		attempts: make(map[types.UID]uint32),
	}
}

// Increment records another unsuccessful scheduling attempt for the pod.
func (f *FairnessState) Increment(uid types.UID) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.attempts[uid]++
}

// Attempts returns the number of recorded scheduling failures for the pod.
func (f *FairnessState) Attempts(uid types.UID) uint32 {
	f.mu.RLock()
	defer f.mu.RUnlock()

	return f.attempts[uid]
}

// Delete removes the pod's retry history after it has been successfully
// scheduled.
func (f *FairnessState) Delete(uid types.UID) {
	f.mu.Lock()
	defer f.mu.Unlock()

	delete(f.attempts, uid)
}
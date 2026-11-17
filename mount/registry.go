package mount

import (
	"sync"

	"ella.to/simplefs"
)

// Domain is a mount made by this process: its identifier and display name.
type Domain struct {
	ID          string
	DisplayName string
}

var (
	regMu   sync.RWMutex
	current simplefs.FileProvider
)

// Register installs p as the provider Mount serves.
func Register(p simplefs.FileProvider) {
	regMu.Lock()
	current = p
	regMu.Unlock()
}

// Current returns the registered provider, or nil.
func Current() simplefs.FileProvider {
	regMu.RLock()
	defer regMu.RUnlock()
	return current
}

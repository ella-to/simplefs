package memfs_test

import (
	"testing"

	"ella.to/simplefs"
	"ella.to/simplefs/memfs"
	"ella.to/simplefs/providertest"
)

func TestConformance(t *testing.T) {
	providertest.Run(t, func(t *testing.T) simplefs.FileProvider {
		return memfs.New()
	}, providertest.Options{})
}

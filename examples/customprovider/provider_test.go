package main

import (
	"testing"

	"ella.to/simplefs"
	"ella.to/simplefs/providertest"
)

// A custom provider proves itself with the conformance suite; read-only
// providers run its read-only checks.
func TestConformance(t *testing.T) {
	t.Setenv("SIMPLEFS_EXAMPLE", "hello")
	providertest.Run(t, func(t *testing.T) simplefs.FileProvider {
		return newSysFS()
	}, providertest.Options{ReadOnly: true})
}

func TestEnvFile(t *testing.T) {
	t.Setenv("SIMPLEFS_EXAMPLE", "hello")
	data, err := simplefs.ReadFile(t.Context(), newSysFS(), "/env/SIMPLEFS_EXAMPLE")
	if err != nil || string(data) != "hello\n" {
		t.Fatalf("got %q, %v", data, err)
	}
}

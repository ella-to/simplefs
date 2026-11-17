//go:build !darwin || !cgo

package fileprovider

import "context"

// AddDomain is unsupported on this platform or build (see ErrUnsupported).
func AddDomain(ctx context.Context, domainID, displayName string) (string, error) {
	return "", ErrUnsupported
}

// Domains is unsupported on this platform or build (see ErrUnsupported).
func Domains(ctx context.Context) ([]Domain, error) { return nil, ErrUnsupported }

// RemoveAllDomains is unsupported on this platform or build (see
// ErrUnsupported).
func RemoveAllDomains(ctx context.Context) error { return ErrUnsupported }

// RemoveDomain is unsupported on this platform or build (see
// ErrUnsupported).
func RemoveDomain(ctx context.Context, domainID string) error { return ErrUnsupported }

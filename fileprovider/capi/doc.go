// Package capi is the CGO bridge between the Objective-C File Provider host
// and fileprovider.Bridge; see capi.go. This file has no cgo so the package
// exists — and a blank import compiles — in every build, even though the
// exported symbols only exist with cgo enabled.
package capi

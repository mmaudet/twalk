//go:build !goolm

package main

// The Olm implementation must be the pure-Go one (goolm); without the tag
// mautrix would link the C libolm. Build with:
//
//	CGO_ENABLED=0 go build -tags goolm -o bin/collector-matrix ./cmd/collector-matrix
var _ = "build with -tags goolm and CGO_ENABLED=0" + missingBuildTagGoolm

package buildidentity

import _ "embed"

// Source is the shared implementation for generated build reports.
//
//go:embed runtime.go
var Source string

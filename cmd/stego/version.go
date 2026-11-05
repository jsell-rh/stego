package main

import "github.com/jsell-rh/stego/internal/buildidentity"

// compilerIdentity is the identity of the running compiler (ST-DIS-RM04): its version and its build identity.
type compilerIdentity struct {
	Version string               `json:"version"`
	Build   buildidentity.Record `json:"build"`
}

// currentIdentity answers ST-DIS-QRY04, Get the compiler build identity: the version it is given and the build
// identity that buildidentity.Current reads from the Go build metadata of this executable.
func currentIdentity(version string) compilerIdentity {
	return compilerIdentity{Version: version, Build: buildidentity.Current()}
}

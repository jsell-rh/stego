package registry

import "fmt"

// Trust says whether a registry source is the reviewed common registry or a
// local registry that adds to it.
type Trust string

const (
	// TrustCommon is the reviewed common registry: the first source.
	TrustCommon Trust = "common"
	// TrustLocal is a local registry that adds parts to the common registry.
	TrustLocal Trust = "local"
)

// ParseTrust gives the trust a value names, and refuses any value that is not
// exactly common or local, naming the value and the two values it may be.
func ParseTrust(value string) (Trust, error) {
	switch Trust(value) {
	case TrustCommon, TrustLocal:
		return Trust(value), nil
	}
	return "", fmt.Errorf("registry trust %q is not valid; it must be %s or %s", value, TrustCommon, TrustLocal)
}

// TrustOf gives the trust of the source at a position of the composition:
// the first source is the common registry, and each later one is local.
func TrustOf(position int) Trust {
	if position == 0 {
		return TrustCommon
	}
	return TrustLocal
}

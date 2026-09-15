package gen

import (
	"errors"
	"strings"
)

// ValidateHTTPBasePath requires a literal prefix shared by routes and links.
// Keep path parameters in collection declarations. Do not normalize an input
// whose encoded or cleaned form could identify a different route.
func ValidateHTTPBasePath(value string) error {
	if value == "" {
		return nil
	}
	invalid := errors.New("base_path must be empty or a canonical absolute path with literal ASCII segments; use letters, digits, '-', '.', '_', and '~', and omit '/' for the root")
	if value[0] != '/' {
		return invalid
	}
	for segment := range strings.SplitSeq(value[1:], "/") {
		if segment == "" || segment == "." || segment == ".." {
			return invalid
		}
		for i := 0; i < len(segment); i++ {
			c := segment[i]
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '.' || c == '_' || c == '~') {
				return invalid
			}
		}
	}
	return nil
}

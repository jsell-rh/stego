// Package browseridentity resolves the shared browser telemetry identity.
package browseridentity

import (
	"fmt"
	"regexp"
)

var servicePattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)

// Resolve uses the service name unless either component declares an identity.
// Both explicit declarations must agree. It does not change either input map.
func Resolve(service string, client, backend map[string]any) (string, error) {
	name := service
	var explicit string
	for _, setting := range []struct {
		values map[string]any
		key    string
	}{{client, "service_name"}, {backend, "telemetry_service_name"}} {
		value, present := setting.values[setting.key]
		if !present {
			continue
		}
		selected, ok := value.(string)
		if !ok || !servicePattern.MatchString(selected) {
			return "", fmt.Errorf("browser telemetry requires a bounded service identity")
		}
		if explicit != "" && explicit != selected {
			return "", fmt.Errorf("browser telemetry client and backend identities differ")
		}
		name, explicit = selected, selected
	}
	if !servicePattern.MatchString(name) {
		return "", fmt.Errorf("browser telemetry requires a bounded service identity")
	}
	return name, nil
}

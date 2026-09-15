package kubernetesservice

import (
	"fmt"
	"sort"

	"github.com/jsell-rh/stego/internal/gen"
)

func externalEndpoints(ctx gen.Context) ([]string, error) {
	required, _, err := endpointDeclarations(ctx)
	return required, err
}

func endpointDeclarations(ctx gen.Context) (required, optional []string, err error) {
	seen := map[string]bool{}
	for _, key := range []string{"external_endpoints", "optional_external_endpoints"} {
		entries, err := configList(ctx, key)
		if err != nil {
			return nil, nil, err
		}
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			name, ok := entry.(string)
			if !ok || !label.MatchString(name) || seen[name] {
				return nil, nil, fmt.Errorf("external endpoint declarations require distinct DNS label names")
			}
			seen[name] = true
			if len(seen) > 32 {
				return nil, nil, fmt.Errorf("external endpoint declarations permit at most 32 names")
			}
			names = append(names, name)
		}
		sort.Strings(names)
		if key == "external_endpoints" {
			required = names
		} else {
			optional = names
		}
	}
	return required, optional, nil
}

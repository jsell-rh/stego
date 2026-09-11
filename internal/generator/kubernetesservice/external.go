package kubernetesservice

import (
	"fmt"
	"sort"

	"github.com/jsell-rh/stego/internal/gen"
)

func externalEndpoints(ctx gen.Context) ([]string, error) {
	entries, err := configList(ctx, "external_endpoints")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var names []string
	for _, entry := range entries {
		name, ok := entry.(string)
		if !ok || !label.MatchString(name) || seen[name] {
			return nil, fmt.Errorf("external_endpoints requires distinct DNS label names")
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

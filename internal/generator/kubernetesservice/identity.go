package kubernetesservice

import (
	"fmt"
	"regexp"
	"sort"

	"github.com/jsell-rh/stego/internal/gen"
)

var apiResource = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}(/[a-z][a-z0-9-]{0,62})?$`)
var resourceName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,252}$`)
var apiGroup = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)

func kubernetesAccess(ctx gen.Context) ([]any, error) {
	enabled := false
	if value, exists := ctx.ComponentConfig["kubernetes_api"]; exists {
		var ok bool
		enabled, ok = value.(bool)
		if !ok {
			return nil, fmt.Errorf("kubernetes_api must be a boolean")
		}
	}
	entries, err := configList(ctx, "kubernetes_permissions")
	if err != nil {
		return nil, err
	}
	if !enabled && len(entries) > 0 {
		return nil, fmt.Errorf("Kubernetes permissions require kubernetes_api")
	}
	if len(entries) > 32 {
		return nil, fmt.Errorf("Kubernetes permission limit exceeded")
	}
	if enabled {
		endpoints, err := externalEndpoints(ctx)
		if err != nil {
			return nil, err
		}
		found := false
		for _, endpoint := range endpoints {
			found = found || endpoint == "kubernetes"
		}
		if !found {
			return nil, fmt.Errorf("kubernetes_api requires the kubernetes external endpoint")
		}
	}
	rules := map[string][]any{}
	for _, entry := range entries {
		values, ok := entry.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("Kubernetes permission must be an object")
		}
		for key := range values {
			switch key {
			case "scope", "api_group", "resources", "verbs", "resource_names":
			default:
				return nil, fmt.Errorf("unknown Kubernetes permission field")
			}
		}
		scope, _ := values["scope"].(string)
		group, groupOK := values["api_group"].(string)
		if (scope != "namespace" && scope != "cluster") || !groupOK || len(group) > 253 || (group != "" && !apiGroup.MatchString(group)) {
			return nil, fmt.Errorf("invalid Kubernetes permission scope or API group")
		}
		list := func(key string, required bool, valid func(string) bool) ([]string, error) {
			raw, exists := values[key]
			if !exists && !required {
				return nil, nil
			}
			items, ok := raw.([]any)
			if !ok || len(items) == 0 || len(items) > 32 {
				return nil, fmt.Errorf("invalid Kubernetes permission %s", key)
			}
			result := []string{}
			seen := map[string]bool{}
			for _, item := range items {
				s, ok := item.(string)
				if !ok || seen[s] || !valid(s) {
					return nil, fmt.Errorf("invalid Kubernetes permission %s", key)
				}
				seen[s] = true
				result = append(result, s)
			}
			sort.Strings(result)
			return result, nil
		}
		resources, err := list("resources", true, func(s string) bool { return apiResource.MatchString(s) })
		if err != nil {
			return nil, err
		}
		verbs, err := list("verbs", true, func(s string) bool {
			switch s {
			case "get", "list", "watch", "create", "update", "patch", "delete":
				return true
			}
			return false
		})
		if err != nil {
			return nil, err
		}
		names, err := list("resource_names", false, func(s string) bool {
			return resourceName.MatchString(s)
		})
		if err != nil {
			return nil, err
		}
		if len(names) > 0 {
			for _, verb := range verbs {
				if verb == "create" {
					return nil, fmt.Errorf("resource_names cannot restrict create")
				}
			}
		}
		rule := object{"apiGroups": []string{group}, "resources": resources, "verbs": verbs}
		if len(names) > 0 {
			rule["resourceNames"] = names
		}
		rules[scope] = append(rules[scope], rule)
	}
	var objects []any
	for _, scope := range []string{"namespace", "cluster"} {
		if len(rules[scope]) == 0 {
			continue
		}
		kind, name := "Role", ctx.ServiceName
		meta := object{"name": name, "namespace": "{{.Namespace}}"}
		if scope == "cluster" {
			kind = "ClusterRole"
			name = "{{.Namespace}}-" + ctx.ServiceName
			meta = object{"name": name}
		}
		objects = append(objects, object{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": kind, "metadata": meta, "rules": rules[scope]}, object{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": kind + "Binding", "metadata": meta, "roleRef": object{"apiGroup": "rbac.authorization.k8s.io", "kind": kind, "name": name}, "subjects": []any{object{"kind": "ServiceAccount", "name": ctx.ServiceName, "namespace": "{{.Namespace}}"}}})
	}
	return objects, nil
}

func kubernetesIdentity(ctx gen.Context, pod, container object) {
	if enabled, _ := ctx.ComponentConfig["kubernetes_api"].(bool); !enabled {
		return
	}
	pod["volumes"] = append(pod["volumes"].([]any), object{"name": "kubernetes-api", "projected": object{"defaultMode": 288, "sources": []any{
		object{"serviceAccountToken": object{"path": "token", "expirationSeconds": 3600}},
		object{"configMap": object{"name": "kube-root-ca.crt", "items": []any{object{"key": "ca.crt", "path": "ca.crt"}}}},
	}}})
	container["volumeMounts"] = append(container["volumeMounts"].([]any), object{"name": "kubernetes-api", "mountPath": "/var/run/stego-kubernetes", "readOnly": true})
}

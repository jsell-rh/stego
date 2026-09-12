package kubernetesservice

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"go/format"
	"path"
	"regexp"
	"strings"
	"text/template"

	"github.com/jsell-rh/stego/internal/gen"
)

type allocationRole struct {
	Name, Scope string
	Rules       []any
}
type allocationBinding struct{ Role, ExternalRole, ServiceAccount, Namespace string }
type allocationProfile struct {
	Name, Prefix, OwnerLabel, Manager string
	SuffixLength                      int
	Bindings                          []allocationBinding
	Quota                             map[string]string
}
type allocationConfiguration struct {
	Service, Allocator string
	Roles              []allocationRole
	Profiles           []allocationProfile
}

var allocationPrefix = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*-$`)
var allocationQuantity = regexp.MustCompile(`^[1-9][0-9]{0,8}(m|Ki|Mi|Gi|Ti)?$`)

func allocationConfig(ctx gen.Context) (allocationConfiguration, error) {
	result := allocationConfiguration{Service: ctx.ServiceName}
	roleEntries, err := configList(ctx, "allocation_roles")
	if err != nil {
		return result, err
	}
	profiles, err := configList(ctx, "allocation_profiles")
	if err != nil {
		return result, err
	}
	if len(roleEntries) == 0 && len(profiles) == 0 {
		workers, err := configList(ctx, "workers")
		if err != nil {
			return result, err
		}
		for _, entry := range workers {
			if w, ok := entry.(map[string]any); ok {
				if value, exists := w["namespace_allocator"]; exists && value != false {
					return result, fmt.Errorf("namespace_allocator requires allocation profiles")
				}
			}
		}
		return result, nil
	}
	if len(roleEntries) > 16 || len(roleEntries) == 0 || len(profiles) == 0 || len(profiles) > 16 {
		return result, fmt.Errorf("allocation requires 1..16 roles and profiles")
	}
	if ctx.ModuleName == "" || ctx.PeerNamespaces["kubernetes-client"] == "" {
		return result, fmt.Errorf("allocation requires kubernetes-client and a module")
	}
	if err := gen.ValidateGoPackageNamespace(ctx.PeerNamespaces["kubernetes-client"]); err != nil {
		return result, err
	}
	roles := map[string]string{}
	for _, entry := range roleEntries {
		values, ok := entry.(map[string]any)
		if !ok || len(values) != 3 {
			return result, fmt.Errorf("allocation role requires name, scope, and rules")
		}
		name, _ := values["name"].(string)
		scope, _ := values["scope"].(string)
		if !label.MatchString(name) || name == "proof" || roles[name] != "" || (scope != "namespace" && scope != "cluster") {
			return result, fmt.Errorf("invalid allocation role")
		}
		raw, ok := values["rules"].([]any)
		if !ok || len(raw) == 0 {
			return result, fmt.Errorf("allocation role rules are required")
		}
		rules := []any{}
		for _, r := range raw {
			fields, ok := r.(map[string]any)
			if !ok {
				return result, fmt.Errorf("invalid allocation rule")
			}
			copy := map[string]any{"scope": "cluster"}
			for key, value := range fields {
				if key == "scope" {
					return result, fmt.Errorf("allocation rule scope belongs to its role")
				}
				copy[key] = value
			}
			rules = append(rules, copy)
		}
		objects, err := kubernetesAccess(gen.Context{ServiceName: ctx.ServiceName, ComponentConfig: map[string]any{"kubernetes_api": true, "external_endpoints": []any{"kubernetes"}, "kubernetes_permissions": rules}})
		if err != nil {
			return result, err
		}
		parsed := objects[0].(object)["rules"].([]any)
		if scope == "cluster" {
			for _, r := range parsed {
				r := r.(object)
				group := r["apiGroups"].([]string)[0]
				for _, resource := range r["resources"].([]string) {
					for _, verb := range r["verbs"].([]string) {
						read := (verb == "get" || verb == "list" || verb == "watch") && group == "" && (resource == "nodes" || resource == "namespaces")
						review := verb == "create" && ((group == "authentication.k8s.io" && resource == "tokenreviews") || (group == "authorization.k8s.io" && resource == "subjectaccessreviews"))
						if !read && !review {
							return result, fmt.Errorf("allocated cluster roles permit only cluster metadata reads and identity reviews")
						}
					}
				}
			}
		}
		roles[name] = scope
		result.Roles = append(result.Roles, allocationRole{name, scope, parsed})
	}
	names := map[string]bool{}
	for _, entry := range profiles {
		values, ok := entry.(map[string]any)
		if !ok {
			return result, fmt.Errorf("allocation profile must be an object")
		}
		for key := range values {
			switch key {
			case "name", "namespace_prefix", "suffix_length", "owner_label", "manager", "bindings", "quota":
			default:
				return result, fmt.Errorf("unknown allocation profile field")
			}
		}
		name, _ := values["name"].(string)
		prefix, _ := values["namespace_prefix"].(string)
		length, lok := values["suffix_length"].(int)
		owner, _ := values["owner_label"].(string)
		manager, _ := values["manager"].(string)
		if !label.MatchString(name) || names[name] || !allocationPrefix.MatchString(prefix) || !lok || length < 1 || len(prefix)+length > 63 || !validLabelKey(owner) || len(owner) > 253 || strings.HasPrefix(owner, "stego.dev/") || strings.HasPrefix(owner, "pod-security.kubernetes.io/") || owner == "app.kubernetes.io/managed-by" || owner == "pod-security.kubernetes.io/enforce" || !label.MatchString(manager) {
			return result, fmt.Errorf("invalid allocation profile identity")
		}
		for _, old := range result.Profiles {
			if len(old.Prefix)+old.SuffixLength == len(prefix)+length && (strings.HasPrefix(prefix, old.Prefix) || strings.HasPrefix(old.Prefix, prefix)) {
				return result, fmt.Errorf("allocation namespace patterns overlap")
			}
		}
		names[name] = true
		p := allocationProfile{Name: name, Prefix: prefix, SuffixLength: length, OwnerLabel: owner, Manager: manager, Quota: map[string]string{}}
		quota, ok := values["quota"].([]any)
		if !ok || len(quota) != 5 {
			return result, fmt.Errorf("allocation requires all five resource bounds")
		}
		for _, v := range quota {
			q, ok := v.(map[string]any)
			if !ok || len(q) != 2 {
				return result, fmt.Errorf("invalid allocation quota")
			}
			resource, _ := q["resource"].(string)
			value, _ := q["value"].(string)
			switch resource {
			case "pods", "limits.cpu", "limits.memory", "limits.ephemeral-storage", "requests.storage":
			default:
				return result, fmt.Errorf("invalid allocation quota resource")
			}
			if p.Quota[resource] != "" || !allocationQuantity.MatchString(value) || (resource == "pods" && strings.Trim(value, "0123456789") != "") {
				return result, fmt.Errorf("invalid allocation quota value")
			}
			if resource == "limits.cpu" && strings.TrimRight(value, "0123456789m") != "" {
				return result, fmt.Errorf("CPU quota requires cores or millicores")
			}
			if resource != "limits.cpu" && resource != "pods" && strings.HasSuffix(value, "m") {
				return result, fmt.Errorf("storage and memory quotas require whole bytes")
			}
			p.Quota[resource] = value
		}
		bindings, ok := values["bindings"].([]any)
		if !ok || len(bindings) == 0 || len(bindings) > 16 {
			return result, fmt.Errorf("allocation requires 1..16 bindings")
		}
		seen := map[string]bool{}
		for _, v := range bindings {
			b, ok := v.(map[string]any)
			if !ok {
				return result, fmt.Errorf("invalid allocation binding")
			}
			for key := range b {
				switch key {
				case "role", "external_role", "service_account", "namespace":
				default:
					return result, fmt.Errorf("unknown allocation binding field")
				}
			}
			role, _ := b["role"].(string)
			external, _ := b["external_role"].(string)
			sa, _ := b["service_account"].(string)
			ns, _ := b["namespace"].(string)
			if !label.MatchString(sa) || (ns != "control" && ns != "allocated") || (role == "") == (external == "") || (role != "" && roles[role] == "") || (external != "" && !resourceName.MatchString(external)) {
				return result, fmt.Errorf("invalid allocation binding target")
			}
			if role != "" && roles[role] == "cluster" && ns != "allocated" {
				return result, fmt.Errorf("allocated cluster permissions require an allocated ServiceAccount")
			}
			key := role + "/" + external + "/" + sa + "/" + ns
			if seen[key] {
				return result, fmt.Errorf("duplicate allocation binding")
			}
			seen[key] = true
			p.Bindings = append(p.Bindings, allocationBinding{role, external, sa, ns})
		}
		result.Profiles = append(result.Profiles, p)
	}
	workers, err := configList(ctx, "workers")
	if err != nil {
		return result, err
	}
	for _, w := range workers {
		v, ok := w.(map[string]any)
		if !ok {
			continue
		}
		value, exists := v["namespace_allocator"]
		if !exists {
			continue
		}
		enabled, ok := value.(bool)
		if !ok {
			return result, fmt.Errorf("namespace_allocator must be a boolean")
		}
		if enabled {
			if result.Allocator != "" {
				return result, fmt.Errorf("declare one namespace allocator")
			}
			name, _ := v["name"].(string)
			if !label.MatchString(name) || len(ctx.ServiceName)+len(name)+1 > 50 {
				return result, fmt.Errorf("invalid allocator worker name")
			}
			if v["kubernetes_api"] != true {
				return result, fmt.Errorf("allocator requires kubernetes_api")
			}
			if _, present := v["kubernetes_permissions"]; present {
				return result, fmt.Errorf("allocator permissions are generated from its profiles")
			}
			result.Allocator = ctx.ServiceName + "-" + name
		}
	}
	if result.Allocator == "" {
		return result, fmt.Errorf("allocation profiles require an allocator worker")
	}
	totalBindings := 0
	for _, p := range result.Profiles {
		for _, b := range p.Bindings {
			totalBindings++
			if b.Namespace == "control" && b.ServiceAccount == result.Allocator {
				return result, fmt.Errorf("allocator cannot receive an allocated role")
			}
		}
	}
	if totalBindings > 32 {
		return result, fmt.Errorf("allocation permits at most 32 bindings")
	}
	return result, nil
}

//go:embed allocation.go.tmpl
var allocationSource string

func allocationFiles(ctx gen.Context) ([]gen.File, error) {
	config, err := allocationConfig(ctx)
	if err != nil || len(config.Profiles) == 0 {
		return nil, err
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	data := struct{ Kubernetes, Configuration string }{path.Join(ctx.ModuleName, ctx.OutDirName, ctx.PeerNamespaces["kubernetes-client"]), string(encoded)}
	tmpl, err := template.New("allocation").Parse(allocationSource)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err = tmpl.Execute(&b, data); err != nil {
		return nil, err
	}
	code, err := format.Source(b.Bytes())
	if err != nil {
		return nil, err
	}
	return []gen.File{{Path: path.Join(ctx.OutputNamespace, "allocation/allocation.go"), Content: code}}, nil
}

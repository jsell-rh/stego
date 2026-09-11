package kubernetesservice

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/jsell-rh/stego/internal/gen"
)

type rpcProcess struct {
	Name, Namespace, Process string
	Context                  gen.Context
}

func rpcProcesses(ctx gen.Context) ([]rpcProcess, error) {
	entries, err := configList(ctx, "rpc_processes")
	if err != nil {
		return nil, err
	}
	dirs, err := sources(ctx)
	if err != nil {
		return nil, err
	}
	allowed := map[string]bool{}
	for _, dir := range dirs {
		if dir != ctx.OutDirName {
			allowed[dir] = true
		}
	}
	seen := map[string]bool{}
	workers, err := configList(ctx, "workers")
	if err != nil {
		return nil, err
	}
	for _, entry := range workers {
		if values, ok := entry.(map[string]any); ok {
			if name, ok := values["name"].(string); ok {
				seen[name] = true
			}
		}
	}
	var result []rpcProcess
	for _, entry := range entries {
		values, ok := entry.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("RPC deployment must be an object")
		}
		name, _ := values["name"].(string)
		component, _ := values["component"].(string)
		process, _ := values["process"].(string)
		namespace := ctx.PeerNamespaces[component]
		if !label.MatchString(name) || len(ctx.ServiceName)+len(name)+1 > 50 || seen[name] {
			return nil, fmt.Errorf("RPC deployment requires a distinct DNS label and a combined service name of at most 50 bytes")
		}
		seen[name] = true
		if (component != "grpc-application" && component != "grpc-processes") || namespace == "" || gen.ValidateGoPackageNamespace(namespace) != nil || process == "" {
			return nil, fmt.Errorf("RPC deployment requires a resolved RPC component and process")
		}
		declarations, err := configList(gen.Context{ComponentConfig: ctx.PeerConfigs[component]}, "processes")
		if err != nil {
			return nil, err
		}
		matches := 0
		for _, declaration := range declarations {
			value, ok := declaration.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid RPC process declaration")
			}
			if value["name"] != process {
				continue
			}
			matches++
			factory, ok := value["factory_package"].(string)
			if !ok || gen.ValidateGoPackageNamespace(factory) != nil || !allowed[strings.Split(factory, "/")[0]] {
				return nil, fmt.Errorf("RPC factory requires an allowed source directory")
			}
		}
		if matches != 1 || gen.ValidateGoPackageNamespace(process) != nil || strings.Contains(process, "/") {
			return nil, fmt.Errorf("RPC deployment must select exactly one declared process")
		}
		child := ctx
		child.ServiceName = ctx.ServiceName + "-" + name
		child.ComponentConfig = map[string]any{}
		for _, key := range []string{"dns_namespace", "dns_port"} {
			if value, ok := ctx.ComponentConfig[key]; ok {
				child.ComponentConfig[key] = value
			}
		}
		for key, value := range values {
			switch key {
			case "name", "component", "process":
			case "env_secret", "files_secret":
				s, ok := value.(string)
				if !ok || !label.MatchString(s) {
					return nil, fmt.Errorf("RPC %s must be a DNS label", key)
				}
				child.ComponentConfig[key] = s
			case "network_peers", "external_endpoints":
				child.ComponentConfig[key] = value
			default:
				return nil, fmt.Errorf("unknown RPC deployment setting %q", key)
			}
		}
		if _, err := networkRulesForPorts(child, map[int]bool{9090: true}); err != nil {
			return nil, err
		}
		result = append(result, rpcProcess{name, namespace, process, child})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func rpcProcessFiles(ctx gen.Context) ([]gen.File, error) {
	declarations, err := rpcProcesses(ctx)
	if err != nil {
		return nil, err
	}
	var files []gen.File
	for _, p := range declarations {
		namespace := path.Join(ctx.OutputNamespace, "rpc", p.Name)
		files = append(files, imageFiles(ctx, namespace, path.Join(ctx.OutDirName, p.Namespace, "processes", p.Process), "rpc")...)
		env := []any{object{"name": "GOMEMLIMIT", "value": "384MiB"}, object{"name": "STEGO_DATABASE_ALLOW_INSECURE_LOOPBACK", "value": "0"}, object{"name": "STEGO_RPC_MONITOR_ADDR", "value": "127.0.0.1:9082"}, object{"name": "STEGO_GRPC_ADDR", "value": "0.0.0.0:9090"}, object{"name": "STEGO_GRPC_TLS_CERT", "value": "/var/run/stego/tls.crt"}, object{"name": "STEGO_GRPC_TLS_KEY", "value": "/var/run/stego/tls.key"}, object{"name": "OTEL_SERVICE_NAME", "value": p.Context.ServiceName}}
		pod, container := workloadPod(p.Context, env)
		container["name"] = "rpc"
		container["ports"] = []any{object{"name": "grpc", "containerPort": 9090}}
		probe := func(mode string, period, threshold int) object {
			return object{"exec": object{"command": []string{"/rpc", "--stego-probe=" + mode}}, "timeoutSeconds": 2, "periodSeconds": period, "failureThreshold": threshold}
		}
		container["startupProbe"], container["readinessProbe"], container["livenessProbe"] = probe("ready", 2, 15), probe("ready", 3, 2), probe("live", 10, 3)
		metadata := object{"name": p.Context.ServiceName, "namespace": "{{.Namespace}}"}
		labels := object{"app.kubernetes.io/name": p.Context.ServiceName}
		rules, _ := networkRulesForPorts(p.Context, map[int]bool{9090: true})
		items := []any{
			object{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": metadata, "automountServiceAccountToken": false},
			object{"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy", "metadata": metadata, "spec": rules},
			object{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": metadata, "spec": object{"replicas": 1, "revisionHistoryLimit": 2, "progressDeadlineSeconds": 180, "strategy": object{"type": "RollingUpdate", "rollingUpdate": object{"maxUnavailable": 0, "maxSurge": 1}}, "selector": object{"matchLabels": labels}, "template": object{"metadata": object{"labels": labels}, "spec": pod}}},
			object{"apiVersion": "v1", "kind": "Service", "metadata": metadata, "spec": object{"type": "ClusterIP", "selector": labels, "ports": []any{object{"name": "grpc", "port": 9090, "targetPort": "grpc"}}}},
		}
		manifest, err := json.MarshalIndent(object{"apiVersion": "v1", "kind": "List", "items": items}, "", "  ")
		if err != nil {
			return nil, err
		}
		manifest = bytes.ReplaceAll(manifest, []byte(`"{{.FSGroup}}"`), []byte(`{{.FSGroup}}`))
		files = append(files, gen.File{Path: path.Join(ctx.OutputNamespace, "render", "rpc-"+p.Name+".json.tmpl"), Content: manifest})
	}
	return files, nil
}

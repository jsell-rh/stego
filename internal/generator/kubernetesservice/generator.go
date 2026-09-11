// Package kubernetesservice generates a restricted service deployment.
package kubernetesservice

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"go/format"
	"path"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/jsell-rh/stego/internal/gen"
)

//go:embed render.go.tmpl
var renderer []byte

type Generator struct{}
type object = map[string]any

var label = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
var directory = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,62}$`)
var labelValue = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9_.-]{0,61}[A-Za-z0-9])?$`)

func (*Generator) MinimumGoVersion() string { return "1.26.8" }

func configList(ctx gen.Context, key string) ([]any, error) {
	v, exists := ctx.ComponentConfig[key]
	if !exists {
		return nil, nil
	}
	if v == nil || reflect.TypeOf(v).Kind() != reflect.Slice {
		return nil, fmt.Errorf("%s must be a list", key)
	}
	list := reflect.ValueOf(v)
	if list.Len() > 32 {
		return nil, fmt.Errorf("%s permits at most 32 entries", key)
	}
	result := make([]any, list.Len())
	for i := range result {
		result[i] = list.Index(i).Interface()
	}
	return result, nil
}

func sources(ctx gen.Context) ([]string, error) {
	entries, err := configList(ctx, "source_directories")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{ctx.OutDirName: true}
	result := []string{ctx.OutDirName}
	for _, entry := range entries {
		s, ok := entry.(string)
		if !ok || !directory.MatchString(s) || seen[s] {
			return nil, fmt.Errorf("source_directories must contain distinct top-level directory names")
		}
		seen[s] = true
		result = append(result, s)
	}
	sort.Strings(result)
	return result, nil
}

func setting(ctx gen.Context, key, fallback string) string {
	s, ok := ctx.ComponentConfig[key].(string)
	if !ok {
		return fallback
	}
	return s
}

func (*Generator) ValidateContext(ctx gen.Context) error {
	if err := gen.ValidateNamespace(ctx.OutputNamespace, nil); err != nil {
		return err
	}
	if !label.MatchString(ctx.ServiceName) || len(ctx.ServiceName) > 50 {
		return fmt.Errorf("kubernetes-service requires a DNS label service name of at most 50 bytes")
	}
	if !directory.MatchString(ctx.OutDirName) {
		return fmt.Errorf("kubernetes-service requires a top-level output directory")
	}
	if ctx.GoVersion != "1.26.8" {
		return fmt.Errorf("kubernetes-service supports Go 1.26.8 with its pinned builder")
	}
	if ctx.PeerNamespaces["health-check"] == "" || (ctx.PeerNamespaces["http-application"] == "" && ctx.PeerNamespaces["rest-api"] == "") {
		return fmt.Errorf("kubernetes-service requires health-check and an HTTP component")
	}
	for key, value := range ctx.ComponentConfig {
		switch key {
		case "source_directories", "network_peers":
		case "env_secret", "files_secret", "dns_namespace":
			s, ok := value.(string)
			if !ok || !label.MatchString(s) {
				return fmt.Errorf("%s must be a DNS label", key)
			}
		case "dns_port":
			n, ok := value.(int)
			if !ok || n < 1 || n > 65535 {
				return fmt.Errorf("dns_port must be in 1..65535")
			}
		default:
			return fmt.Errorf("unknown kubernetes-service setting %q", key)
		}
	}
	if _, err := sources(ctx); err != nil {
		return err
	}
	_, err := networkRules(ctx)
	return err
}

func networkRules(ctx gen.Context) (object, error) {
	peers, err := configList(ctx, "network_peers")
	if err != nil {
		return nil, err
	}
	ingress, egress := []any{}, []any{}
	for _, entry := range peers {
		peer, ok := entry.(map[string]any)
		if !ok || len(peer) != 6 {
			return nil, fmt.Errorf("network peer requires direction, namespace, pod_label, pod_value, port, and protocol")
		}
		direction, _ := peer["direction"].(string)
		namespace, _ := peer["namespace"].(string)
		key, _ := peer["pod_label"].(string)
		value, _ := peer["pod_value"].(string)
		protocol, _ := peer["protocol"].(string)
		port, ok := peer["port"].(int)
		if (direction != "ingress" && direction != "egress") || (!label.MatchString(namespace) && namespace != "self") || !labelValue.MatchString(key) || !labelValue.MatchString(value) || !ok || port < 1 || port > 65535 || (protocol != "TCP" && protocol != "UDP") {
			return nil, fmt.Errorf("invalid network peer")
		}
		if direction == "ingress" && (protocol != "TCP" || (port != 8443 && (port != 9090 || ctx.PeerNamespaces["grpc-application"] == ""))) {
			return nil, fmt.Errorf("ingress peer must select an enabled service port")
		}
		if namespace == "self" {
			namespace = "{{.Namespace}}"
		}
		selector := object{"namespaceSelector": object{"matchLabels": object{"kubernetes.io/metadata.name": namespace}}, "podSelector": object{"matchLabels": object{key: value}}}
		rule := object{"ports": []any{object{"protocol": protocol, "port": port}}}
		if direction == "ingress" {
			rule["from"] = []any{selector}
			ingress = append(ingress, rule)
		} else {
			rule["to"] = []any{selector}
			egress = append(egress, rule)
		}
	}
	dnsPort := 53
	if v, ok := ctx.ComponentConfig["dns_port"].(int); ok {
		dnsPort = v
	}
	egress = append(egress, object{"to": []any{object{"namespaceSelector": object{"matchLabels": object{"kubernetes.io/metadata.name": setting(ctx, "dns_namespace", "kube-system")}}}}, "ports": []any{object{"protocol": "UDP", "port": dnsPort}, object{"protocol": "TCP", "port": dnsPort}}})
	return object{"podSelector": object{"matchLabels": object{"app.kubernetes.io/name": ctx.ServiceName}}, "policyTypes": []string{"Ingress", "Egress"}, "ingress": ingress, "egress": egress}, nil
}

func (g *Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	if err := g.ValidateContext(ctx); err != nil {
		return nil, nil, err
	}
	dirs, _ := sources(ctx)
	var docker, ignore strings.Builder
	docker.WriteString("# Code generated by STEGO. DO NOT EDIT.\nFROM docker.io/library/golang@sha256:2d54f6c8c6ea532a321e0b4c69553b2ed3637608d4f4357dbed37939fe2620cc AS build\nWORKDIR /src\nENV GOTOOLCHAIN=local GOWORK=off CGO_ENABLED=0\nCOPY go.mod go.sum ./\nRUN go mod download\n")
	ignore.WriteString("**\n!go.mod\n!go.sum\n")
	for _, dir := range dirs {
		fmt.Fprintf(&docker, "COPY %s/ ./%s/\n", dir, dir)
		fmt.Fprintf(&ignore, "!%s/\n!%s/**\n", dir, dir)
	}
	// Source trees must not supply hidden files or private keys to the build.
	ignore.WriteString("**/.*/**\n**/.*\n**/*.key\n**/*.pem\n**/*.p12\n**/*.pfx\n")
	fmt.Fprintf(&docker, "RUN go mod verify && go build -mod=readonly -trimpath -buildvcs=false -o /service ./%s\nFROM scratch\nCOPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt\nCOPY --from=build --chmod=0555 /service /service\nUSER 65532:65532\nEXPOSE 8443\nENTRYPOINT [\"/service\"]\n", ctx.OutDirName)
	rules, _ := networkRules(ctx)
	metadata := func(name string) object { return object{"name": name, "namespace": "{{.Namespace}}"} }
	labels := object{"app.kubernetes.io/name": ctx.ServiceName}
	env := []any{object{"name": "GOMEMLIMIT", "value": "384MiB"}, object{"name": "PORT", "value": "8443"}, object{"name": "STEGO_HTTP_REQUIRE_TLS", "value": "1"}, object{"name": "STEGO_HTTP_TLS_CERT", "value": "/var/run/stego/tls.crt"}, object{"name": "STEGO_HTTP_TLS_KEY", "value": "/var/run/stego/tls.key"}, object{"name": "STEGO_DATABASE_ALLOW_INSECURE_LOOPBACK", "value": "0"}}
	ports := []any{object{"name": "https", "containerPort": 8443}}
	servicePorts := []any{object{"name": "https", "port": 8443, "targetPort": "https"}}
	if ctx.PeerNamespaces["grpc-application"] != "" {
		env = append(env, object{"name": "STEGO_GRPC_ADDR", "value": "0.0.0.0:9090"}, object{"name": "STEGO_GRPC_TLS_CERT", "value": "/var/run/stego/tls.crt"}, object{"name": "STEGO_GRPC_TLS_KEY", "value": "/var/run/stego/tls.key"})
		ports = append(ports, object{"name": "grpc", "containerPort": 9090})
		servicePorts = append(servicePorts, object{"name": "grpc", "port": 9090, "targetPort": "grpc"})
	}
	probe := func(url string, period, threshold int) object {
		return object{"httpGet": object{"path": url, "port": "https", "scheme": "HTTPS"}, "timeoutSeconds": 2, "periodSeconds": period, "failureThreshold": threshold}
	}
	pod := object{"serviceAccountName": ctx.ServiceName, "automountServiceAccountToken": false, "terminationGracePeriodSeconds": 60, "securityContext": object{"runAsNonRoot": true, "fsGroup": "{{.FSGroup}}", "seccompProfile": object{"type": "RuntimeDefault"}}, "containers": []any{object{
		"name": "service", "image": "{{.Image}}", "imagePullPolicy": "IfNotPresent", "ports": ports, "env": env, "envFrom": []any{object{"secretRef": object{"name": setting(ctx, "env_secret", ctx.ServiceName+"-runtime")}}},
		"securityContext": object{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": object{"drop": []string{"ALL"}}},
		"resources":       object{"requests": object{"cpu": "100m", "memory": "128Mi", "ephemeral-storage": "32Mi"}, "limits": object{"cpu": "1", "memory": "512Mi", "ephemeral-storage": "128Mi"}},
		"startupProbe":    probe("/livez", 2, 30), "readinessProbe": probe("/readyz", 3, 2), "livenessProbe": probe("/livez", 10, 3),
		"volumeMounts": []any{object{"name": "files", "mountPath": "/var/run/stego", "readOnly": true}, object{"name": "tmp", "mountPath": "/tmp"}},
	}}, "volumes": []any{object{"name": "files", "secret": object{"secretName": setting(ctx, "files_secret", ctx.ServiceName+"-files"), "defaultMode": 288}}, object{"name": "tmp", "emptyDir": object{"sizeLimit": "64Mi"}}}}
	items := []any{
		object{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": metadata(ctx.ServiceName), "automountServiceAccountToken": false},
		object{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": metadata(ctx.ServiceName), "spec": object{"replicas": 1, "revisionHistoryLimit": 2, "progressDeadlineSeconds": 180, "strategy": object{"type": "RollingUpdate", "rollingUpdate": object{"maxUnavailable": 0, "maxSurge": 1}}, "selector": object{"matchLabels": labels}, "template": object{"metadata": object{"labels": labels}, "spec": pod}}},
		object{"apiVersion": "v1", "kind": "Service", "metadata": metadata(ctx.ServiceName), "spec": object{"type": "ClusterIP", "selector": labels, "ports": servicePorts}},
		object{"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy", "metadata": metadata(ctx.ServiceName), "spec": rules},
	}
	manifest, err := json.MarshalIndent(object{"apiVersion": "v1", "kind": "List", "items": items}, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	manifest = bytes.ReplaceAll(manifest, []byte(`"{{.FSGroup}}"`), []byte(`{{.FSGroup}}`))
	code, err := format.Source(renderer)
	if err != nil {
		return nil, nil, err
	}
	files := []gen.File{{Path: path.Join(ctx.OutputNamespace, "Containerfile"), Content: []byte(docker.String())}, {Path: path.Join(ctx.OutputNamespace, "Containerfile.dockerignore"), Content: []byte(ignore.String())}, {Path: path.Join(ctx.OutputNamespace, "render/main.go"), Content: code}, {Path: path.Join(ctx.OutputNamespace, "render/manifest.json.tmpl"), Content: manifest}}
	return files, nil, gen.ValidateNamespace(ctx.OutputNamespace, files)
}

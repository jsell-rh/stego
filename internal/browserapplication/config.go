// Package browserapplication checks the shared private application declaration.
package browserapplication

import (
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/jsell-rh/stego/internal/gen"
)

type Config struct {
	Port                                                          int
	Image, ListenEnv, PortEnv, EnvSecret, FilesSecret, HealthPath string
}

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
var envName = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
var imageName = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[1-9][0-9]{0,4})?(/[a-z0-9]+([._-][a-z0-9]+)*)+@sha256:[a-f0-9]{64}$`)
var healthPath = regexp.MustCompile(`^/[A-Za-z0-9_./-]+$`)

// Resolve reads the same declaration for the browser, health, and deployment
// generators. It does not change the context or any resolved declaration.
func Resolve(ctx gen.Context, deployment map[string]any) (*Config, error) {
	raw, present := deployment["local_applications"]
	if !present {
		return nil, nil
	}
	entries, ok := raw.([]any)
	if !ok || len(entries) != 1 {
		return nil, fmt.Errorf("local_applications requires exactly one application")
	}
	values, ok := entries[0].(map[string]any)
	if !ok || len(values) != 7 {
		return nil, fmt.Errorf("local_application requires port, image, listen_env, port_env, env_secret, files_secret, and health_path")
	}
	c := &Config{}
	c.Port, ok = values["port"].(int)
	if !ok || c.Port < 1024 || c.Port > 65535 || c.Port == 8443 {
		return nil, fmt.Errorf("local application requires a distinct unprivileged port")
	}
	c.Image, _ = values["image"].(string)
	if !validImage(c.Image) {
		return nil, fmt.Errorf("local application requires an image with an explicit registry and SHA-256 digest")
	}
	c.ListenEnv, _ = values["listen_env"].(string)
	c.PortEnv, _ = values["port_env"].(string)
	if !validEnv(c.ListenEnv) || !validEnv(c.PortEnv) || c.ListenEnv == c.PortEnv {
		return nil, fmt.Errorf("local application requires distinct listener environment names")
	}
	c.EnvSecret, _ = values["env_secret"].(string)
	c.FilesSecret, _ = values["files_secret"].(string)
	if !dnsLabel.MatchString(c.EnvSecret) || !dnsLabel.MatchString(c.FilesSecret) {
		return nil, fmt.Errorf("local application requires named configuration and file secrets")
	}
	backendEnv, _ := deployment["env_secret"].(string)
	if backendEnv == "" {
		backendEnv = ctx.ServiceName + "-runtime"
	}
	backendFiles, _ := deployment["files_secret"].(string)
	if backendFiles == "" {
		backendFiles = ctx.ServiceName + "-files"
	}
	for _, secret := range []string{c.EnvSecret, c.FilesSecret} {
		if secret == backendEnv || secret == backendFiles {
			return nil, fmt.Errorf("local application cannot share browser secrets")
		}
	}
	c.HealthPath, _ = values["health_path"].(string)
	if len(c.HealthPath) > 256 || !healthPath.MatchString(c.HealthPath) || path.Clean(c.HealthPath) != c.HealthPath || strings.Contains(c.HealthPath, "//") {
		return nil, fmt.Errorf("local application requires a canonical health path")
	}
	for _, name := range []string{"browser-backend", "health-check", "postgres-adapter", "otel-tracing", "kubernetes-service"} {
		if ctx.PeerNamespaces[name] == "" {
			return nil, fmt.Errorf("local application requires %s", name)
		}
	}
	for _, name := range []string{"rest-api", "http-application", "grpc-application", "jwt-auth", "rh-sso-auth"} {
		if ctx.PeerNamespaces[name] != "" {
			return nil, fmt.Errorf("local application must be separate from bearer-token services")
		}
	}
	browser := ctx.PeerConfigs["browser-backend"]
	_, assets := browser["assets"]
	_, bundle := browser["asset_bundle"]
	if assets == bundle {
		return nil, fmt.Errorf("local application requires one captured browser asset declaration")
	}
	if database, _ := ctx.PeerConfigs["health-check"]["database"].(bool); !database {
		return nil, fmt.Errorf("local application readiness requires its session database")
	}
	if api, _ := deployment["kubernetes_api"].(bool); api {
		return nil, fmt.Errorf("local application cannot use a Kubernetes API identity")
	}
	for _, key := range []string{"kubernetes_permissions", "workers", "rpc_processes", "allocation_roles", "allocation_profiles"} {
		if values, present := deployment[key]; present && values != nil {
			return nil, fmt.Errorf("local application does not permit %s", key)
		}
	}
	return c, nil
}

func validEnv(value string) bool {
	if !envName.MatchString(value) || strings.HasPrefix(value, "STEGO_") {
		return false
	}
	switch value {
	case "PATH", "HOME", "TMPDIR", "LD_PRELOAD", "LD_LIBRARY_PATH", "GOMEMLIMIT", "GOMAXPROCS":
		return false
	}
	return true
}
func validImage(value string) bool {
	if len(value) > 2048 || !imageName.MatchString(value) {
		return false
	}
	registry, _, _ := strings.Cut(value, "/")
	host, port, hasPort := strings.Cut(registry, ":")
	if len(host) > 253 || (host != "localhost" && !strings.Contains(host, ".") && !hasPort) {
		return false
	}
	for _, part := range strings.Split(host, ".") {
		if !dnsLabel.MatchString(part) {
			return false
		}
	}
	if hasPort {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return false
		}
	}
	return true
}

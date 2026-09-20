package workload

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func widget() Deployment {
	return Deployment{Name: "widget", Namespace: "tenant-one", ServiceAccount: "widget-runtime", OwnerLabels: map[string]string{"example.org/owner": "one"}, PodLabels: map[string]string{"example.org/workload": "widget"}, Selector: map[string]string{"example.org/workload": "widget"}, Replicas: 3, Strategy: "RollingUpdate", FSGroup: 2000, TerminationGraceSeconds: 30,
		Containers: []Container{{Name: "server", Image: "registry.example.org/team/widget@sha256:" + strings.Repeat("a", 64), Args: []string{"serve"}, Env: []Env{{Name: "MODE", Value: "production"}, {Name: "DATABASE_URL", Secret: "widget-database", Key: "uri"}}, Ports: []Port{{Name: "http", Number: 8080}}, Mounts: []Mount{{Name: "config", Path: "/etc/widget"}, {Name: "tls", Path: "/etc/widget-tls"}, {Name: "tmp", Path: "/tmp"}}, Requests: Resources{CPUMilli: 100, MemoryMi: 256, EphemeralMi: 32}, Limits: Resources{CPUMilli: 500, MemoryMi: 512, EphemeralMi: 256}, Startup: HTTPProbe{Path: "/live", Port: "http", PeriodSeconds: 2, TimeoutSeconds: 1, FailureThreshold: 60}, Readiness: HTTPProbe{Path: "/ready", Port: "http", PeriodSeconds: 2, TimeoutSeconds: 1, FailureThreshold: 3}, Liveness: HTTPProbe{Path: "/live", Port: "http", PeriodSeconds: 10, TimeoutSeconds: 1, FailureThreshold: 3}}},
		Volumes:    []Volume{{Name: "config", ConfigMap: "widget-config"}, {Name: "tls", Secret: "widget-tls"}, {Name: "tmp", EmptyMi: 64}}, Dependencies: []Dependency{{Name: "database", Data: map[string][]byte{"uri": []byte("private-database-value")}}, {Name: "config", Data: map[string][]byte{"mode": []byte("production")}}}, ServicePorts: []ServicePort{{Name: "https", Target: "http", Port: 443}}}
}
func render(t *testing.T, d Deployment) []Resource {
	t.Helper()
	out, e := Build(d)
	if e != nil {
		t.Fatal(e)
	}
	return out
}
func pod(t *testing.T, out []Resource) Object {
	t.Helper()
	return out[0].Object["spec"].(Object)["template"].(Object)["spec"].(Object)
}
func TestWidgetSecurityAndService(t *testing.T) {
	d := widget()
	out := render(t, d)
	if len(out) != 2 || out[0].Collection != "/apis/apps/v1/namespaces/tenant-one/deployments" || out[1].Collection != "/api/v1/namespaces/tenant-one/services" {
		t.Fatal("resource scope differs")
	}
	p := pod(t, out)
	if p["automountServiceAccountToken"] != false || p["serviceAccountName"] != "widget-runtime" {
		t.Fatal("unexpected API credentials")
	}
	want := Object{"runAsNonRoot": true, "fsGroup": uint32(2000), "seccompProfile": Object{"type": "RuntimeDefault"}}
	if !reflect.DeepEqual(p["securityContext"], want) {
		t.Fatal("Pod security differs")
	}
	c := p["containers"].([]Object)[0]
	want = Object{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": Object{"drop": []string{"ALL"}}}
	if !reflect.DeepEqual(c["securityContext"], want) {
		t.Fatal("container security differs")
	}
	for _, key := range []string{"startupProbe", "readinessProbe", "livenessProbe"} {
		probe := c[key].(Object)
		if probe["timeoutSeconds"] != 1 || probe["successThreshold"] != 1 || probe["httpGet"].(Object)["host"] != nil {
			t.Fatal("probe policy differs")
		}
	}
	volumes := p["volumes"].([]Object)
	if volumes[1]["secret"].(Object)["defaultMode"] != int32(0440) {
		t.Fatal("Secret access differs")
	}
	mounts := c["volumeMounts"].([]Object)
	if mounts[0]["readOnly"] != true || mounts[1]["readOnly"] != true || mounts[2]["readOnly"] != false {
		t.Fatal("mount access differs")
	}
	spec := out[1].Object["spec"].(Object)
	if spec["type"] != "ClusterIP" || spec["ports"].([]Object)[0]["targetPort"] != "http" {
		t.Fatal("service differs")
	}
	body, e := json.Marshal(out)
	if e != nil {
		t.Fatal(e)
	}
	if bytes.Contains(body, []byte("private-database-value")) {
		t.Fatal("dependency data exposed")
	}
	for _, key := range []string{"hostNetwork", "hostPID", "hostIPC", "hostPath", "privileged", "subPath", "exec"} {
		if bytes.Contains(body, []byte(`"`+key+`"`)) {
			t.Fatalf("unexpected field %s", key)
		}
	}
}
func TestWidgetInputsAreNotAliased(t *testing.T) {
	d := widget()
	out := render(t, d)
	before, _ := json.Marshal(out)
	d.OwnerLabels["example.org/owner"] = "changed"
	d.PodLabels["example.org/workload"] = "changed"
	d.Selector["example.org/workload"] = "changed"
	d.Containers[0].Args[0] = "changed"
	d.Dependencies[0].Data["uri"][0] = 'X'
	after, _ := json.Marshal(out)
	if !bytes.Equal(before, after) {
		t.Fatal("input mutation changed result")
	}
	out[0].Object["metadata"].(Object)["labels"].(Object)["example.org/owner"] = "another"
	if out[1].Object["metadata"].(Object)["labels"].(Object)["example.org/owner"] != "one" {
		t.Fatal("resource labels are aliased")
	}
	out[0].Object["spec"].(Object)["selector"].(Object)["matchLabels"].(Object)["example.org/workload"] = "other"
	if out[1].Object["spec"].(Object)["selector"].(Object)["example.org/workload"] != "widget" {
		t.Fatal("selectors are aliased")
	}
}
func TestWidgetMultipleContainersAndProfiles(t *testing.T) {
	d := widget()
	d.Replicas = 1001
	d.Strategy = "Recreate"
	d.RunAsUser = 1000
	d.RunAsGroup = 1000
	d.KubernetesAPI = true
	d.ImagePullSecrets = []string{"registry-access"}
	second := d.Containers[0]
	second.Name = "helper"
	second.Ports = []Port{{Name: "helper", Number: 8081}}
	second.Startup.Port = "helper"
	second.Readiness.Port = "helper"
	second.Liveness.Port = "helper"
	d.Containers = append(d.Containers, second)
	out := render(t, d)
	p := pod(t, out)
	if len(p["containers"].([]Object)) != 2 || p["automountServiceAccountToken"] != true || p["securityContext"].(Object)["runAsUser"] != uint32(1000) {
		t.Fatal("declared profile differs")
	}
	if out[0].Object["spec"].(Object)["replicas"] != uint32(1001) {
		t.Fatal("application capacity was limited")
	}
	d.Replicas = 0
	render(t, d)
}
func TestConfigurationDigestChangesOnlyWithContents(t *testing.T) {
	a := []Dependency{{Name: "first", Data: map[string][]byte{"one": []byte("ab"), "two": []byte("c")}}, {Name: "second", Data: map[string][]byte{"three": []byte("value")}}}
	digest, e := ConfigurationDigest(a)
	if e != nil {
		t.Fatal(e)
	}
	b := []Dependency{a[1], a[0]}
	same, e := ConfigurationDigest(b)
	if e != nil || digest != same || len(digest) != 64 {
		t.Fatal("order changed digest")
	}
	b[1].Data = map[string][]byte{"one": []byte("a"), "two": []byte("bc")}
	changed, e := ConfigurationDigest(b)
	if e != nil || digest == changed {
		t.Fatal("value boundaries lost")
	}
	b[1].Data = map[string][]byte{"one": []byte("ab"), "two": []byte("c")}
	b[1].Name = "renamed"
	changed, e = ConfigurationDigest(b)
	if e != nil || digest == changed {
		t.Fatal("dependency identity lost")
	}
	empty, _ := ConfigurationDigest([]Dependency{{Name: "one", Data: map[string][]byte{"empty": {}}}})
	nilValue, _ := ConfigurationDigest([]Dependency{{Name: "one", Data: map[string][]byte{"empty": nil}}})
	if empty != nilValue {
		t.Fatal("equivalent empty contents differ")
	}
}
func TestConfigurationDigestRejectsInvalidInput(t *testing.T) {
	for name, input := range map[string][]Dependency{"duplicate": {{Name: "one"}, {Name: "one"}}, "name": {{Name: "../escape"}}, "key": {{Name: "one", Data: map[string][]byte{"bad/key": {}}}}, "large": {{Name: "one", Data: map[string][]byte{"value": make([]byte, (1<<20)+1)}}}, "count": make([]Dependency, 65)} {
		t.Run(name, func(t *testing.T) {
			digest, e := ConfigurationDigest(input)
			if !errors.Is(e, ErrDeclaration) || digest != "" {
				t.Fatal("invalid dependency accepted")
			}
		})
	}
}
func TestWorkloadRejectsUnsafeOrAmbiguousInput(t *testing.T) {
	cases := map[string]func(*Deployment){
		"namespace":         func(d *Deployment) { d.Namespace = "../foreign" },
		"name":              func(d *Deployment) { d.Name = "bad/name" },
		"default account":   func(d *Deployment) { d.ServiceAccount = "default" },
		"empty account":     func(d *Deployment) { d.ServiceAccount = "" },
		"root group":        func(d *Deployment) { d.FSGroup = 0 },
		"user overflow":     func(d *Deployment) { d.RunAsUser = 1 << 31 },
		"replica overflow":  func(d *Deployment) { d.Replicas = 1 << 31 },
		"strategy":          func(d *Deployment) { d.Strategy = "unknown" },
		"no labels":         func(d *Deployment) { d.OwnerLabels = nil },
		"invalid label":     func(d *Deployment) { d.OwnerLabels["bad/key/key"] = "one" },
		"no selector":       func(d *Deployment) { d.Selector = nil },
		"selector mismatch": func(d *Deployment) { d.Selector["example.org/workload"] = "other" },
		"image tag":         func(d *Deployment) { d.Containers[0].Image = "registry.example.org/widget:latest" },
		"image path": func(d *Deployment) {
			d.Containers[0].Image = "https://registry.example.org/widget@sha256:" + strings.Repeat("a", 64)
		},
		"missing digest":      func(d *Deployment) { d.Containers[0].Image = "registry.example.org/widget@sha256:bad" },
		"duplicate container": func(d *Deployment) { d.Containers = append(d.Containers, d.Containers[0]) },
		"no containers":       func(d *Deployment) { d.Containers = nil },
		"too many containers": func(d *Deployment) { d.Containers = make([]Container, 17) },
		"argument bytes":      func(d *Deployment) { d.Containers[0].Args = []string{strings.Repeat("x", 8193)} },
		"argument nul":        func(d *Deployment) { d.Containers[0].Args = []string{"bad\x00arg"} },
		"CPU zero":            func(d *Deployment) { d.Containers[0].Requests.CPUMilli = 0 },
		"CPU excess":          func(d *Deployment) { d.Containers[0].Requests.CPUMilli = 501 },
		"memory excess":       func(d *Deployment) { d.Containers[0].Requests.MemoryMi = 513 },
		"storage excess":      func(d *Deployment) { d.Containers[0].Requests.EphemeralMi = 257 },
		"negative limit":      func(d *Deployment) { d.Containers[0].Limits.CPUMilli = -1 },
		"duplicate env":       func(d *Deployment) { d.Containers[0].Env = append(d.Containers[0].Env, d.Containers[0].Env[0]) },
		"env ambiguity":       func(d *Deployment) { d.Containers[0].Env[1].Value = "private-value" },
		"env missing key":     func(d *Deployment) { d.Containers[0].Env[1].Key = "" },
		"env nul":             func(d *Deployment) { d.Containers[0].Env[0].Value = "bad\x00value" },
		"duplicate port":      func(d *Deployment) { d.Containers[0].Ports = append(d.Containers[0].Ports, d.Containers[0].Ports[0]) },
		"privileged port":     func(d *Deployment) { d.Containers[0].Ports[0].Number = 80 },
		"missing probe port":  func(d *Deployment) { d.Containers[0].Readiness.Port = "other" },
		"probe authority":     func(d *Deployment) { d.Containers[0].Startup.Path = "//other/path" },
		"probe query":         func(d *Deployment) { d.Containers[0].Startup.Path = "/health?credential=value" },
		"probe encoding":      func(d *Deployment) { d.Containers[0].Startup.Path = "/%2fother" },
		"probe newline":       func(d *Deployment) { d.Containers[0].Startup.Path = "/health\n" },
		"probe timeout":       func(d *Deployment) { d.Containers[0].Startup.TimeoutSeconds = 0 },
		"probe period":        func(d *Deployment) { d.Containers[0].Startup.PeriodSeconds = 0 },
		"probe threshold":     func(d *Deployment) { d.Containers[0].Startup.FailureThreshold = 0 },
		"mount root":          func(d *Deployment) { d.Containers[0].Mounts[0].Path = "/" },
		"mount traversal":     func(d *Deployment) { d.Containers[0].Mounts[0].Path = "/etc/../tmp" },
		"mount proc":          func(d *Deployment) { d.Containers[0].Mounts[0].Path = "/proc" },
		"mount token":         func(d *Deployment) { d.Containers[0].Mounts[0].Path = "/var/run/secrets" },
		"mount overlap":       func(d *Deployment) { d.Containers[0].Mounts[1].Path = "/etc/widget/tls" },
		"mount duplicate": func(d *Deployment) {
			d.Containers[0].Mounts = append(d.Containers[0].Mounts, d.Containers[0].Mounts[0])
		},
		"unknown volume":         func(d *Deployment) { d.Containers[0].Mounts[0].Name = "unknown" },
		"volume ambiguity":       func(d *Deployment) { d.Volumes[0].Secret = "other" },
		"volume duplicate":       func(d *Deployment) { d.Volumes = append(d.Volumes, d.Volumes[0]) },
		"volume unused":          func(d *Deployment) { d.Volumes = append(d.Volumes, Volume{Name: "unused", Secret: "unused"}) },
		"volume negative":        func(d *Deployment) { d.Volumes[2].EmptyMi = -1 },
		"volume storage excess":  func(d *Deployment) { d.Volumes[2].EmptyMi = 257 },
		"pull secret duplicate":  func(d *Deployment) { d.ImagePullSecrets = []string{"one", "one"} },
		"missing service target": func(d *Deployment) { d.ServicePorts[0].Target = "other" },
		"service duplicate":      func(d *Deployment) { d.ServicePorts = append(d.ServicePorts, d.ServicePorts[0]) },
		"service port":           func(d *Deployment) { d.ServicePorts[0].Port = 0 },
		"termination deadline":   func(d *Deployment) { d.TerminationGraceSeconds = 0 },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			d := widget()
			change(&d)
			out, e := Build(d)
			if !errors.Is(e, ErrDeclaration) || out != nil || e.Error() != "invalid workload declaration" {
				t.Fatal("invalid declaration returned resources or private error data")
			}
		})
	}
}

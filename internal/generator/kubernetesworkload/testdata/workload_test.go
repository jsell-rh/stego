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
		Volumes:    []Volume{{Name: "config", ConfigMap: "widget-config"}, {Name: "tls", Secret: "widget-tls"}, {Name: "tmp", EmptyMi: 64}}, Dependencies: []Dependency{{Kind: "Secret", Name: "widget-database", Data: map[string][]byte{"uri": []byte("private-database-value")}}, {Kind: "ConfigMap", Name: "widget-config", Data: map[string][]byte{"mode": []byte("production")}}, {Kind: "Secret", Name: "widget-tls", Data: map[string][]byte{"tls.crt": []byte("verified-certificate")}}}, ServicePorts: []ServicePort{{Name: "https", Target: "http", Port: 443}}}
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
	if mounts[0]["readOnly"] != true || mounts[1]["readOnly"] != true || mounts[2]["readOnly"] != nil {
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
	a := []Dependency{{Kind: "Secret", Name: "first", Data: map[string][]byte{"one": []byte("ab"), "two": []byte("c")}}, {Kind: "Secret", Name: "second", Data: map[string][]byte{"three": []byte("value")}}}
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
	empty, emptyErr := ConfigurationDigest([]Dependency{{Kind: "Secret", Name: "one", Data: map[string][]byte{"empty": {}}}})
	nilValue, nilErr := ConfigurationDigest([]Dependency{{Kind: "Secret", Name: "one", Data: map[string][]byte{"empty": nil}}})
	if emptyErr != nil || nilErr != nil || empty != nilValue {
		t.Fatal("equivalent empty contents differ")
	}
}
func TestConfigurationDigestRejectsInvalidInput(t *testing.T) {
	for name, input := range map[string][]Dependency{"duplicate": {{Kind: "Secret", Name: "one"}, {Kind: "Secret", Name: "one"}}, "name": {{Kind: "Secret", Name: "../escape"}}, "key": {{Kind: "Secret", Name: "one", Data: map[string][]byte{"bad/key": {}}}}, "large": {{Kind: "Secret", Name: "one", Data: map[string][]byte{"value": make([]byte, (1<<20)+1)}}}, "count": make([]Dependency, 65)} {
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
		"missing dependency": func(d *Deployment) { d.Dependencies = d.Dependencies[1:] },
		"extra dependency": func(d *Deployment) {
			d.Dependencies = append(d.Dependencies, Dependency{Kind: "Secret", Name: "unused"})
		},
		"wrong dependency kind":   func(d *Deployment) { d.Dependencies[0].Kind = "ConfigMap" },
		"missing environment key": func(d *Deployment) { delete(d.Dependencies[0].Data, "uri") },
		"invalid registry port": func(d *Deployment) {
			d.Containers[0].Image = "registry.example.org:65536/widget@sha256:" + strings.Repeat("a", 64)
		},
		"invalid registry host": func(d *Deployment) { d.Containers[0].Image = "bad..example/widget@sha256:" + strings.Repeat("a", 64) },
		"namespace":             func(d *Deployment) { d.Namespace = "../foreign" },
		"name":                  func(d *Deployment) { d.Name = "bad/name" },
		"default account":       func(d *Deployment) { d.ServiceAccount = "default" },
		"empty account":         func(d *Deployment) { d.ServiceAccount = "" },
		"root group":            func(d *Deployment) { d.FSGroup = 0 },
		"user overflow":         func(d *Deployment) { d.RunAsUser = 1 << 31 },
		"replica overflow":      func(d *Deployment) { d.Replicas = 1 << 31 },
		"strategy":              func(d *Deployment) { d.Strategy = "unknown" },
		"no labels":             func(d *Deployment) { d.OwnerLabels = nil },
		"invalid label":         func(d *Deployment) { d.OwnerLabels["bad/key/key"] = "one" },
		"no selector":           func(d *Deployment) { d.Selector = nil },
		"selector mismatch":     func(d *Deployment) { d.Selector["example.org/workload"] = "other" },
		"image tag":             func(d *Deployment) { d.Containers[0].Image = "registry.example.org/widget:latest" },
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

func TestWidgetDependencyRollout(t *testing.T) {
	d := widget()
	before := render(t, d)
	d.Dependencies[0].Data["uri"] = []byte("rotated-private-database-value")
	after := render(t, d)
	template := func(out []Resource) Object { return out[0].Object["spec"].(Object)["template"].(Object) }
	a := template(before)
	b := template(after)
	secondDigest := b["metadata"].(Object)["annotations"].(Object)[ConfigurationAnnotation]
	if a["metadata"].(Object)["annotations"].(Object)[ConfigurationAnnotation] == b["metadata"].(Object)["annotations"].(Object)[ConfigurationAnnotation] {
		t.Fatal("Secret rotation did not change rollout")
	}
	delete(a["metadata"].(Object), "annotations")
	delete(b["metadata"].(Object), "annotations")
	if !reflect.DeepEqual(a, b) {
		t.Fatal("Secret rotation changed the workload contract")
	}
	data, _ := json.Marshal(after)
	if bytes.Contains(data, []byte("rotated-private-database-value")) {
		t.Fatal("rotated Secret leaked")
	}
	d.Dependencies[1].Data["mode"] = []byte("changed")
	latest := render(t, d)
	if template(latest)["metadata"].(Object)["annotations"].(Object)[ConfigurationAnnotation] == secondDigest {
		t.Fatal("ConfigMap change did not change rollout")
	}
}

func TestConfigurationDigestSeparatesObjectKinds(t *testing.T) {
	a, e := ConfigurationDigest([]Dependency{{Kind: "Secret", Name: "one", Data: map[string][]byte{"key": []byte("value")}}})
	if e != nil {
		t.Fatal(e)
	}
	b, e := ConfigurationDigest([]Dependency{{Kind: "ConfigMap", Name: "one", Data: map[string][]byte{"key": []byte("value")}}})
	if e != nil || a == b {
		t.Fatal("object kind lost")
	}
	if _, e = ConfigurationDigest([]Dependency{{Kind: "Other", Name: "one"}}); !errors.Is(e, ErrDeclaration) {
		t.Fatal("unknown dependency kind accepted")
	}
}

// Kubernetes omits empty lists, empty environment values, and false mount flags.
// Desired fields must survive this response format to avoid repeated patches.
// See kubernetes/api v0.35.0 core/v1/types.go: PodSpec, Container, EnvVar, VolumeMount.
func TestWidgetOptionalFieldsSurviveAPISerialization(t *testing.T) {
	d := widget()
	d.Containers[0].Args = nil
	d.Containers[0].Env = nil
	d.Containers[0].Mounts = nil
	d.Volumes = nil
	d.Dependencies = nil
	p := pod(t, render(t, d))
	c := p["containers"].([]Object)[0]
	for _, name := range []string{"volumes", "imagePullSecrets"} {
		if _, exists := p[name]; exists {
			t.Fatal("empty optional Pod field", name)
		}
	}
	for _, name := range []string{"args", "env", "volumeMounts"} {
		if _, exists := c[name]; exists {
			t.Fatal("empty optional container field", name)
		}
	}
	d.Containers[0].Env = []Env{{Name: "EMPTY", Value: ""}}
	c = pod(t, render(t, d))["containers"].([]Object)[0]
	raw, err := json.Marshal(c["env"])
	if err != nil {
		t.Fatal(err)
	}
	// These public API JSON fields deliberately use the upstream omit rules.
	var env []struct {
		Name  string `json:"name"`
		Value string `json:"value,omitempty"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	roundTrip, err := json.Marshal(env)
	if err != nil || !bytes.Equal(raw, roundTrip) {
		t.Fatal("empty environment value changes after serialization")
	}
	p = pod(t, render(t, widget()))
	c = p["containers"].([]Object)[0]
	for _, mount := range c["volumeMounts"].([]Object) {
		raw, err := json.Marshal(mount)
		if err != nil {
			t.Fatal(err)
		}
		var typed struct {
			Name     string `json:"name"`
			Path     string `json:"mountPath"`
			ReadOnly bool   `json:"readOnly,omitempty"`
		}
		if err := json.Unmarshal(raw, &typed); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(typed)
		if err != nil {
			t.Fatal(err)
		}
		var before, after Object
		if json.Unmarshal(raw, &before) != nil || json.Unmarshal(encoded, &after) != nil || !reflect.DeepEqual(before, after) {
			t.Fatal("mount changes after serialization")
		}
	}
}

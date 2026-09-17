package deployment_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"reflect"
	"strings"
	"testing"

	deployment "example.com/widget/out/deploy"
)

func options() deployment.Options {
	return deployment.Options{
		Image:     "registry.example.test/team/widget@sha256:" + strings.Repeat("a", 64),
		Namespace: "test", FSGroup: 10001,
		Egress: []deployment.EndpointBinding{{Name: "kubernetes", Address: netip.MustParseAddrPort("10.0.0.1:443")}},
	}
}

func TestExistingServiceAccountPreservesWorkload(t *testing.T) {
	o := options()
	o.RPCProcess, o.Scope, o.Egress = "records", "namespace", nil
	o.OwnerLabels = map[string]string{"example.test/instance": "one"}
	o.ImagePullSecrets = []string{"registry-auth"}
	before, err := deployment.Resources(o)
	if err != nil {
		t.Fatal(err)
	}
	original := ""
	want := []deployment.Resource{}
	for _, item := range before {
		if item.Kind == "ServiceAccount" {
			original = item.Name
			continue
		}
		want = append(want, item)
	}
	if original == "" {
		t.Fatal("fixture has no generated account")
	}
	o.ExistingServiceAccount = "sa-" + strings.Repeat("a", 32) + "-" + strings.Repeat("b", 26)
	after, err := deployment.Resources(o)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range after {
		if item.Kind == "ServiceAccount" {
			t.Fatal("externally managed account was emitted")
		}
		if item.Kind != "Deployment" {
			continue
		}
		pod := item.Object["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
		if pod["serviceAccountName"] != o.ExistingServiceAccount {
			t.Fatal("selected account was lost")
		}
		pod["serviceAccountName"] = original
		found = true
	}
	if !found || !reflect.DeepEqual(want, after) {
		t.Fatal("account selection changed other workload fields")
	}
	first, err := deployment.Render(o)
	if err != nil {
		t.Fatal(err)
	}
	second, err := deployment.Render(o)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatal("account output is not stable", err)
	}
	// A caller's object edits must not enter another render.
	for _, item := range after {
		item.Object["kind"] = "changed"
	}
	third, err := deployment.Render(o)
	if err != nil || !bytes.Equal(first, third) {
		t.Fatal("returned object changed the next render", err)
	}
	o.OwnerLabels = nil
	wantBytes, err := deployment.Render(o)
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"--image", o.Image, "--namespace", o.Namespace, "--fs-group", "10001", "--scope", "namespace", "--rpc-process", "records", "--image-pull-secret", "registry-auth", "--existing-service-account", o.ExistingServiceAccount}
	var command bytes.Buffer
	if err := deployment.RenderCommand(args, &command); err != nil || !bytes.Equal(wantBytes, command.Bytes()) {
		t.Fatal("command account selection differs", err)
	}
}

func TestExistingServiceAccountRejectsAmbiguousAuthority(t *testing.T) {
	for _, workload := range []string{"api", "queue", "metadata"} {
		o := options()
		o.Scope, o.ExistingServiceAccount = "namespace", "external-account"
		if workload != "api" {
			o.Worker = workload
		}
		if data, err := deployment.Render(o); err == nil || data != nil {
			t.Fatal("generated RBAC accepted an external identity", workload)
		}
	}
	for _, name := range []string{"../account", "account.other", "Account", "-", strings.Repeat("a", 64)} {
		o := options()
		o.Scope, o.RPCProcess, o.Egress, o.ExistingServiceAccount = "namespace", "records", nil, name
		if data, err := deployment.Render(o); err == nil || data != nil {
			t.Fatal("invalid account emitted output", name)
		}
	}
	for _, scope := range []string{"", "all", "cluster"} {
		o := options()
		o.Scope, o.RPCProcess, o.Egress, o.ExistingServiceAccount = scope, "records", nil, "external-account"
		if data, err := deployment.Render(o); err == nil || data != nil {
			t.Fatal("external account accepted a non-namespace scope", scope)
		}
	}
}

func TestTypedRendererMatchesCommand(t *testing.T) {
	for _, scope := range []string{"", "all", "cluster", "namespace"} {
		o := options()
		o.Scope = scope
		want, err := deployment.Render(o)
		if err != nil {
			t.Fatal(err)
		}
		args := []string{"--image", o.Image, "--namespace", o.Namespace, "--fs-group", "10001", "--egress", "kubernetes=10.0.0.1:443"}
		if scope != "" {
			args = append(args, "--scope", scope)
		}
		var output bytes.Buffer
		if err := deployment.RenderCommand(args, &output); err != nil || !bytes.Equal(want, output.Bytes()) {
			t.Fatal("command and typed output differ", err)
		}
		if err := deployment.RenderCommand(args, shortWriter{}); err != io.ErrShortWrite {
			t.Fatal("short write was not reported", err)
		}
	}
}

type shortWriter struct{}

func (shortWriter) Write(b []byte) (int, error) { return len(b) - 1, nil }

func TestOwnedResources(t *testing.T) {
	o := options()
	o.OwnerLabels = map[string]string{"example.test/instance": "one"}
	resources, err := ownedWorkloads(o)
	if err != nil || len(resources) == 0 {
		t.Fatal("owned resources failed", err)
	}
	collections := map[string]string{
		"ServiceAccount": "/api/v1/namespaces/test/serviceaccounts", "Service": "/api/v1/namespaces/test/services",
		"Deployment": "/apis/apps/v1/namespaces/test/deployments", "NetworkPolicy": "/apis/networking.k8s.io/v1/namespaces/test/networkpolicies",
		"Role": "/apis/rbac.authorization.k8s.io/v1/namespaces/test/roles", "RoleBinding": "/apis/rbac.authorization.k8s.io/v1/namespaces/test/rolebindings",
		"ClusterRole": "/apis/rbac.authorization.k8s.io/v1/clusterroles", "ClusterRoleBinding": "/apis/rbac.authorization.k8s.io/v1/clusterrolebindings",
		"ValidatingAdmissionPolicy":        "/apis/admissionregistration.k8s.io/v1/validatingadmissionpolicies",
		"ValidatingAdmissionPolicyBinding": "/apis/admissionregistration.k8s.io/v1/validatingadmissionpolicybindings",
	}
	seen := map[string]bool{}
	for _, r := range resources {
		seen[r.Kind] = true
		if r.Collection != collections[r.Kind] || r.Name == "" || r.APIVersion == "" {
			t.Fatal("resource identity differs", r.Kind)
		}
		meta := r.Object["metadata"].(map[string]any)
		if meta["name"] != r.Name || meta["labels"].(map[string]any)["example.test/instance"] != "one" {
			t.Fatal("resource owner differs")
		}
		if strings.Contains(r.Collection, "/namespaces/") && r.Namespace != "test" {
			t.Fatal("resource namespace differs")
		}
		if r.Kind == "Deployment" {
			spec := r.Object["spec"].(map[string]any)
			pod := spec["template"].(map[string]any)
			group := pod["spec"].(map[string]any)["securityContext"].(map[string]any)["fsGroup"]
			if group != json.Number("10001") {
				t.Fatalf("number lost its type: %T", group)
			}
			labels := pod["metadata"].(map[string]any)["labels"].(map[string]any)
			if _, exists := labels["example.test/instance"]; exists {
				t.Fatal("owner changed Pod selectors")
			}
		}
	}
	for kind := range collections {
		if !seen[kind] {
			t.Fatal("missing resource kind", kind)
		}
	}
	before, _ := json.Marshal(resources)
	o.OwnerLabels["example.test/instance"] = "two"
	after, _ := json.Marshal(resources)
	if !bytes.Equal(before, after) {
		t.Fatal("options changed returned resources")
	}
	resources[0].Object["metadata"].(map[string]any)["labels"].(map[string]any)["example.test/instance"] = "changed"
	if len(resources) > 1 && resources[1].Object["metadata"].(map[string]any)["labels"].(map[string]any)["example.test/instance"] != "one" {
		t.Fatal("resource labels share storage")
	}
	o.OwnerLabels["example.test/instance"] = "one"
	repeated, err := ownedWorkloads(o)
	encoded, _ := json.Marshal(repeated)
	if err != nil || !bytes.Equal(before, encoded) {
		t.Fatal("returned mutation changed next render", err)
	}
}

func ownedWorkloads(o deployment.Options) ([]deployment.Resource, error) {
	resources, err := deployment.Resources(o)
	if err != nil {
		return nil, err
	}
	o.Worker = "queue"
	worker, err := deployment.Resources(o)
	if err != nil {
		return nil, err
	}
	return append(resources, worker...), nil
}

func TestTypedRendererRejectsInvalidOptions(t *testing.T) {
	mutations := map[string]func(*deployment.Options){
		"group":              func(o *deployment.Options) { o.FSGroup = 0 },
		"scope":              func(o *deployment.Options) { o.Scope = "unknown" },
		"namespace":          func(o *deployment.Options) { o.Namespace = "../test" },
		"image":              func(o *deployment.Options) { o.Image = "registry.example.test/team/widget:latest" },
		"workload":           func(o *deployment.Options) { o.Worker = "missing" },
		"both workloads":     func(o *deployment.Options) { o.Worker = "queue"; o.RPCProcess = "records" },
		"endpoint count":     func(o *deployment.Options) { o.Egress = make([]deployment.EndpointBinding, 33) },
		"endpoint absent":    func(o *deployment.Options) { o.Egress = nil },
		"endpoint address":   func(o *deployment.Options) { o.Egress[0].Address = netip.AddrPort{} },
		"endpoint loopback":  func(o *deployment.Options) { o.Egress[0].Address = netip.MustParseAddrPort("127.0.0.1:443") },
		"endpoint name":      func(o *deployment.Options) { o.Egress[0].Name = "kubernetes=1.2.3.4:443" },
		"endpoint duplicate": func(o *deployment.Options) { o.Egress = append(o.Egress, o.Egress[0]) },
		"owner count": func(o *deployment.Options) {
			o.OwnerLabels = map[string]string{}
			for i := 0; i < 9; i++ {
				o.OwnerLabels[fmt.Sprintf("example.test/owner%d", i)] = "one"
			}
		},
	}
	for _, key := range []string{"plain", "stego.dev/owner", "k8s.io/owner", "app.kubernetes.io/name", "Bad..test/owner", "example.test/path/extra"} {
		mutations["owner key "+key] = func(o *deployment.Options) { o.OwnerLabels = map[string]string{key: "one"} }
	}
	for _, value := range []string{"", "../one", strings.Repeat("a", 64)} {
		mutations["owner value "+value] = func(o *deployment.Options) { o.OwnerLabels = map[string]string{"example.test/owner": value} }
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			o := options()
			mutate(&o)
			if result, err := deployment.Render(o); err == nil || result != nil {
				t.Fatal("invalid options emitted output")
			}
		})
	}
	if result, err := deployment.Resources(options()); err == nil || result != nil {
		t.Fatal("controller resources lack an owner")
	}
}

func TestIndependentRenders(t *testing.T) {
	for i := 0; i < 4; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			t.Parallel()
			o := options()
			o.Namespace = fmt.Sprintf("target-%d", i)
			o.Scope = "namespace"
			o.OwnerLabels = map[string]string{"example.test/instance": o.Namespace}
			resources, err := deployment.Resources(o)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range resources {
				if r.Namespace != o.Namespace || r.Object["metadata"].(map[string]any)["labels"].(map[string]any)["example.test/instance"] != o.Namespace {
					t.Fatal("renders share data")
				}
			}
		})
	}
}

func TestImagePullSecrets(t *testing.T) {
	for _, workload := range []string{"service", "queue", "records"} {
		o := options()
		o.OwnerLabels = map[string]string{"example.test/instance": "one"}
		if workload == "queue" {
			o.Worker = workload
		}
		if workload == "records" {
			o.RPCProcess = workload
			o.Egress = nil
		}
		before, err := deployment.Resources(o)
		if err != nil {
			t.Fatal(err)
		}
		o.ImagePullSecrets = []string{"second.registry", "first-registry"}
		after, err := deployment.Resources(o)
		if err != nil {
			t.Fatal(err)
		}
		seen := false
		for _, resource := range after {
			if resource.Kind != "Deployment" {
				continue
			}
			pod := resource.Object["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
			want := []any{map[string]any{"name": "first-registry"}, map[string]any{"name": "second.registry"}}
			if !reflect.DeepEqual(pod["imagePullSecrets"], want) {
				t.Fatal("pull references differ", pod["imagePullSecrets"])
			}
			delete(pod, "imagePullSecrets")
			seen = true
		}
		if !seen || !reflect.DeepEqual(before, after) || o.ImagePullSecrets[0] != "second.registry" {
			t.Fatal("pull references changed other resources or caller input")
		}
		commandOptions := o
		commandOptions.OwnerLabels = nil
		data, err := deployment.Render(commandOptions)
		if err != nil {
			t.Fatal(err)
		}
		args := []string{"--image", o.Image, "--namespace", o.Namespace, "--fs-group", "10001", "--image-pull-secret", "first-registry", "--image-pull-secret", "second.registry"}
		if len(o.Egress) != 0 {
			args = append(args, "--egress", "kubernetes=10.0.0.1:443")
		}
		if o.Worker != "" {
			args = append(args, "--worker", o.Worker)
		}
		if o.RPCProcess != "" {
			args = append(args, "--rpc-process", o.RPCProcess)
		}
		var output bytes.Buffer
		if err := deployment.RenderCommand(args, &output); err != nil || !bytes.Equal(data, output.Bytes()) {
			t.Fatal("pull Secret command differs", err)
		}
	}
	for _, names := range [][]string{{""}, {"a", "a"}, {"../credential"}, {"a..b"}, {"Upper"}, {strings.Repeat("a", 254)}, make([]string, 9)} {
		o := options()
		o.ImagePullSecrets = names
		if data, err := deployment.Render(o); err == nil || data != nil {
			t.Fatal("invalid pull references emitted output")
		}
		args := []string{"--image", o.Image, "--namespace", o.Namespace, "--egress", "kubernetes=10.0.0.1:443"}
		for _, name := range names {
			args = append(args, "--image-pull-secret", name)
		}
		var output bytes.Buffer
		if err := deployment.RenderCommand(args, &output); err == nil || output.Len() != 0 {
			t.Fatal("invalid pull command emitted output")
		}
	}
}

package deployment_test

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	deployment "example.com/widget/out/deploy"
)

func TestConfigurationDigestChangesOnlyPodAnnotation(t *testing.T) {
	for _, target := range []string{"api", "queue", "records", "external-account"} {
		for _, scope := range []string{"all", "namespace"} {
			if target == "external-account" && scope == "all" {
				continue
			}
			t.Run(target+"/"+scope, func(t *testing.T) {
				o := options()
				o.Scope = scope
				args := []string{"--image", o.Image, "--namespace", o.Namespace, "--fs-group", "10001", "--scope", scope}
				switch target {
				case "queue":
					o.Worker = target
					args = append(args, "--worker", target)
				case "records", "external-account":
					o.RPCProcess, o.Egress = "records", nil
					args = append(args, "--rpc-process", "records")
				}
				if target == "api" || target == "queue" {
					args = append(args, "--egress", "kubernetes=10.0.0.1:443")
				}
				if target == "external-account" {
					o.ExistingServiceAccount, o.ImagePullSecrets = "allocated-widget", []string{"registry-auth"}
					args = append(args, "--existing-service-account", o.ExistingServiceAccount, "--image-pull-secret", "registry-auth")
				}
				before, err := deployment.Resources(o)
				if err != nil {
					t.Fatal(err)
				}
				base, err := deployment.Render(o)
				if err != nil {
					t.Fatal(err)
				}
				o.ConfigurationDigest = strings.Repeat("a", 64)
				first, err := deployment.Render(o)
				if err != nil {
					t.Fatal(err)
				}
				var command bytes.Buffer
				if err := deployment.RenderCommand(append(args, "--configuration-digest", o.ConfigurationDigest), &command); err != nil || !bytes.Equal(first, command.Bytes()) {
					t.Fatal("command and typed configuration differ", err)
				}
				for _, digest := range []string{strings.Repeat("a", 64), strings.Repeat("b", 64)} {
					o.ConfigurationDigest = digest
					after, err := deployment.Resources(o)
					if err != nil {
						t.Fatal(err)
					}
					found := 0
					for _, item := range after {
						if item.Kind != "Deployment" {
							continue
						}
						meta := item.Object["spec"].(map[string]any)["template"].(map[string]any)["metadata"].(map[string]any)
						annotations := meta["annotations"].(map[string]any)
						if annotations[deployment.ConfigurationAnnotation] != digest {
							t.Fatal("configuration digest did not reach the Pod template")
						}
						delete(annotations, deployment.ConfigurationAnnotation)
						if len(annotations) == 0 {
							delete(meta, "annotations")
						}
						found++
					}
					if found != 1 || !reflect.DeepEqual(before, after) {
						t.Fatal("configuration changed another resource field")
					}
				}
				o.ConfigurationDigest = strings.Repeat("a", 64)
				repeated, err := deployment.Render(o)
				if err != nil || !bytes.Equal(first, repeated) {
					t.Fatal("result edits changed later construction", err)
				}
				o.ConfigurationDigest = ""
				empty, err := deployment.Render(o)
				if err != nil || !bytes.Equal(base, empty) {
					t.Fatal("omission changed the default manifest", err)
				}
			})
		}
	}
}

func TestConfigurationDigestRejectsInvalidValues(t *testing.T) {
	for _, digest := range []string{strings.Repeat("a", 63), strings.Repeat("a", 65), strings.Repeat("A", 64), strings.Repeat("z", 64), strings.Repeat("a", 63) + "\n", "private-configuration-canary"} {
		o := options()
		o.ConfigurationDigest = digest
		if data, err := deployment.Render(o); err == nil || err.Error() != "invalid configuration digest" || data != nil {
			t.Fatal("invalid digest produced output or a public value", err)
		}
		if resources, err := deployment.Resources(o); err == nil || resources != nil {
			t.Fatal("invalid digest produced resources")
		}
		var output bytes.Buffer
		output.WriteString("existing output")
		args := []string{"--image", o.Image, "--namespace", o.Namespace, "--egress", "kubernetes=10.0.0.1:443", "--configuration-digest", digest}
		if err := deployment.RenderCommand(args, &output); err == nil || err.Error() != "invalid configuration digest" || output.String() != "existing output" {
			t.Fatal("invalid command digest changed output", err)
		}
	}
	o := options()
	o.Scope, o.ConfigurationDigest = "cluster", strings.Repeat("a", 64)
	if data, err := deployment.Render(o); err == nil || data != nil {
		t.Fatal("configuration digest accepted a scope with no workload")
	}
}

package kubernetes

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These claims are observations, not an authentication decision. The API server
// must authenticate each request and return its credential and bound Pod IDs.
type projectedClaims struct {
	Subject    string `json:"sub"`
	ID         string `json:"jti"`
	Issued     int64  `json:"iat"`
	Expires    int64  `json:"exp"`
	Kubernetes struct {
		Namespace string `json:"namespace"`
		Pod       struct {
			UID string `json:"uid"`
		} `json:"pod"`
		ServiceAccount struct{ UID, Name string } `json:"serviceaccount"`
	} `json:"kubernetes.io"`
}

func projectedIdentity(t *testing.T, file, namespace, pod string) projectedClaims {
	t.Helper()
	token, err := readToken(file)
	if err != nil {
		t.Fatal("cannot read the projected token")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(token) > 16384 {
		t.Fatal("projected token is not a bounded JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	var claims projectedClaims
	if err != nil || json.Unmarshal(payload, &claims) != nil {
		t.Fatal("projected token has invalid claims")
	}
	if claims.ID == "" || len(claims.ID) > 128 || claims.Subject != "system:serviceaccount:"+namespace+":rotation" || claims.Kubernetes.Namespace != namespace || claims.Kubernetes.Pod.UID != pod || claims.Kubernetes.ServiceAccount.Name != "rotation" || claims.Kubernetes.ServiceAccount.UID == "" || claims.Issued < 1 || claims.Expires <= claims.Issued {
		t.Fatal("projected token has an unexpected identity")
	}
	return claims
}

func extraValue(object Object, key string) string {
	values, ok := Nested(object, "status", "userInfo", "extra", key).([]any)
	if !ok || len(values) != 1 {
		return ""
	}
	value, _ := values[0].(string)
	return value
}

func TestLiveProjectedTokenRotation(t *testing.T) {
	if os.Getenv("STEGO_KUBERNETES_LIVE_ROTATION") != "1" {
		t.Skip("requires a dedicated cluster Job with a projected token")
	}
	namespace, pod := os.Getenv("STEGO_TEST_NAMESPACE"), os.Getenv("STEGO_TEST_POD_UID")
	if namespace == "" || pod == "" {
		t.Fatal("live rotation requires a namespace and Pod UID")
	}
	const tokenFile = "/var/run/stego-kubernetes/token"
	client, err := New(Options{ServerURL: "https://kubernetes.default.svc", CAFile: "/var/run/stego-kubernetes/ca.crt", TokenFile: tokenFile})
	if err != nil {
		t.Fatal("cannot create the generated Kubernetes client")
	}
	defer client.Close()
	started := time.Now()
	base, finishTelemetry := rotationTelemetry(t, context.Background())
	ctx, cancel := context.WithTimeout(base, 12*time.Minute)
	defer cancel()
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	initial, requests := "", 0
	for {
		before := projectedIdentity(t, tokenFile, namespace, pod)
		operation, stop := context.WithTimeout(ctx, 5*time.Second)
		response, code, err := client.Request(operation, http.MethodPost, "/apis/authentication.k8s.io/v1/selfsubjectreviews", Object{"apiVersion": "authentication.k8s.io/v1", "kind": "SelfSubjectReview"})
		stop()
		requests++
		if err != nil || code != http.StatusCreated {
			t.Fatal("API server did not accept a projected-token request")
		}
		after := projectedIdentity(t, tokenFile, namespace, pod)
		credential := extraValue(response, "authentication.kubernetes.io/credential-id")
		// The file can rotate while Request reads it. Accept only a token seen
		// immediately before or after this request, and confirmed by the server.
		observed := before
		if credential == "JTI="+after.ID {
			observed = after
		} else if credential != "JTI="+before.ID {
			t.Fatal("API server did not confirm the current projected credential")
		}
		if String(response, "status", "userInfo", "username") != observed.Subject || String(response, "status", "userInfo", "uid") != observed.Kubernetes.ServiceAccount.UID || extraValue(response, "authentication.kubernetes.io/pod-uid") != pod {
			t.Fatal("API server did not confirm the bound ServiceAccount and Pod")
		}
		if initial == "" {
			initial = credential
		}
		if credential != initial {
			operation, stop := context.WithTimeout(ctx, 5*time.Second)
			_, code, err := client.Request(operation, http.MethodGet, "/api/v1/namespaces/"+namespace+"/secrets", nil)
			stop()
			if err == nil || code != http.StatusForbidden {
				t.Fatal("rotation identity has unexpected Secret access")
			}
			finishTelemetry()
			report := map[string]any{"telemetry_enabled": rotationTelemetryEnabled, "rotations": 1, "server_confirmed_credentials": 2, "requests": requests, "elapsed_seconds": time.Since(started).Seconds(), "token_lifetime_seconds": observed.Expires - observed.Issued, "secret_access_denied": true, "client_restarts": 0}
			if dir := os.Getenv("STEGO_KUBERNETES_ROTATION_ARTIFACTS"); dir != "" {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal("cannot create rotation evidence directory")
				}
				data, err := json.MarshalIndent(report, "", "  ")
				if err != nil || os.WriteFile(filepath.Join(dir, "rotation.json"), append(data, '\n'), 0600) != nil {
					t.Fatal("cannot save rotation evidence")
				}
			}
			t.Log("API server confirmed a new projected credential through the same generated client; Secret access remained denied")
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("projected token did not rotate within twelve minutes")
		case <-tick.C:
		}
	}
}

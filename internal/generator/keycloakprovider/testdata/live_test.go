package keycloak

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tracing "example.com/provider/out/tracing"
)

const nativeTestSubject = "00000000-0000-4000-8000-000000000901"

const providerTestImage = "quay.io/keycloak/keycloak@sha256:ff4257d0d64efbe99ed1ddfaf07765cc3c36dc7518bf8324d41961327f441c54"

func dockerCommand(t *testing.T, budget time.Duration, args ...string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	return exec.CommandContext(ctx, "docker", args...).CombinedOutput()
}
func liveProvider(t *testing.T) (*Client, Options) {
	t.Helper()
	if os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("STEGO_REQUIRE_KEYCLOAK_PROVIDER") != "1" {
		t.Fatal("real Keycloak provider tests require their CI job")
	}
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "provider-test"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, BasicConstraintsValid: true, IsCA: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	write := func(name string, data []byte, mode os.FileMode) string {
		target := filepath.Join(dir, name)
		if err := os.WriteFile(target, data, mode); err != nil {
			t.Fatal(err)
		}
		return target
	}
	// These are disposable test credentials. Only exact files are mounted.
	ca := write("server.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0444)
	serverKey := write("server-key.pem", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}), 0444)
	secret := write("operator-secret", []byte("provider-test-admin-secret"), 0600)
	clients := []any{map[string]any{"clientId": "operator", "enabled": true, "protocol": "openid-connect", "secret": "provider-test-admin-secret", "serviceAccountsEnabled": true, "standardFlowEnabled": false, "directAccessGrantsEnabled": false, "fullScopeAllowed": true}}
	for _, item := range []struct{ name, key, value string }{{"catalog", "product.owner", "object-1"}, {"batch-worker", "pipeline.job", "run-2"}, {"foreign-service", "product.owner", "someone-else"}} {
		clients = append(clients, map[string]any{"clientId": item.name, "enabled": true, "protocol": "openid-connect", "publicClient": false, "secret": "provider-test-client-secret", "serviceAccountsEnabled": true, "standardFlowEnabled": false, "directAccessGrantsEnabled": false, "fullScopeAllowed": false, "attributes": map[string]string{item.key: item.value}})
	}
	clients = append(clients, map[string]any{"clientId": "scope-blind-operator", "enabled": true, "protocol": "openid-connect", "secret": "provider-test-scope-blind-secret", "serviceAccountsEnabled": true, "standardFlowEnabled": false, "directAccessGrantsEnabled": false, "fullScopeAllowed": true})
	clients = append(clients, map[string]any{"clientId": "realm-scope-operator", "enabled": true, "protocol": "openid-connect", "secret": "provider-test-realm-scope-secret", "serviceAccountsEnabled": true, "standardFlowEnabled": false, "directAccessGrantsEnabled": false, "fullScopeAllowed": true})
	clients = append(clients, map[string]any{"clientId": "role-operator", "enabled": true, "protocol": "openid-connect", "secret": "provider-test-role-secret", "serviceAccountsEnabled": true, "standardFlowEnabled": false, "directAccessGrantsEnabled": false, "fullScopeAllowed": true})
	users := []any{map[string]any{"username": "service-account-operator", "enabled": true, "serviceAccountClientId": "operator", "clientRoles": map[string]any{"realm-management": []string{"manage-clients", "view-clients", "manage-users", "view-users", "view-realm"}}}}
	users = append(users, map[string]any{"username": "service-account-role-operator", "enabled": true, "serviceAccountClientId": "role-operator", "clientRoles": map[string]any{"realm-management": []string{"manage-clients", "view-clients", "manage-users", "view-users"}}})
	users = append(users, map[string]any{"username": "service-account-scope-blind-operator", "enabled": true, "serviceAccountClientId": "scope-blind-operator", "clientRoles": map[string]any{"realm-management": []string{"query-clients"}}})
	users = append(users, map[string]any{"username": "service-account-realm-scope-operator", "enabled": true, "serviceAccountClientId": "realm-scope-operator", "clientRoles": map[string]any{"realm-management": []string{"manage-clients", "view-clients", "view-realm", "manage-realm"}}})
	users = append(users, map[string]any{"id": nativeTestSubject, "username": "native-user", "firstName": "Native", "lastName": "User", "email": "native@example.invalid", "emailVerified": true, "enabled": true, "requiredActions": []string{}, "credentials": []any{map[string]any{"type": "password", "value": "provider-test-native-password", "temporary": false}}})
	for _, id := range []string{"shared-catalog", "shared-pipeline"} {
		users = append(users, map[string]any{"id": id, "username": id, "enabled": true, "realmRoles": []string{"shared-global"}, "clientRoles": map[string]any{"foreign-service": []string{"foreign-view"}}, "groups": []string{"/outside"}})
	}
	realm := map[string]any{"realm": "provider-test", "enabled": true, "sslRequired": "all", "clients": clients, "users": users,
		"roles":  map[string]any{"realm": []any{map[string]any{"name": "shared-global"}, map[string]any{"name": "worker-base"}}, "client": map[string]any{"foreign-service": []any{map[string]any{"name": "foreign-view"}}}},
		"groups": []any{map[string]any{"name": "outside", "clientRoles": map[string]any{"foreign-service": []string{"foreign-view"}}}},
	}
	data, err := json.Marshal(realm)
	if err != nil {
		t.Fatal(err)
	}
	realmFile := write("provider-test-realm.json", data, 0444)
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	name := "stego-keycloak-provider-" + hex.EncodeToString(nonce)
	t.Cleanup(func() {
		if t.Failed() {
			output, _ := dockerCommand(t, 10*time.Second, "logs", "--tail", "100", name)
			t.Logf("disposable Keycloak log: %s", output)
		}
		output, err := dockerCommand(t, 20*time.Second, "rm", "--force", name)
		if err != nil && !strings.Contains(string(output), "No such container") {
			t.Errorf("remove test Keycloak: %v", err)
		}
	})
	args := []string{"run", "--detach", "--name", name, "--label", "stego.test=keycloak-provider", "--label", "stego.test.run=" + os.Getenv("GITHUB_RUN_ID"), "--memory=2g", "--cpus=2", "--pids-limit=512", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--user=1000:0", "--publish", "127.0.0.1::8443",
		"--mount", "type=bind,source=" + ca + ",target=/certs/server.pem,readonly",
		"--mount", "type=bind,source=" + serverKey + ",target=/certs/server-key.pem,readonly",
		"--mount", "type=bind,source=" + realmFile + ",target=/opt/keycloak/data/import/provider-test-realm.json,readonly",
		providerTestImage, "start-dev", "--http-enabled=false", "--hostname-strict=false", "--https-certificate-file=/certs/server.pem", "--https-certificate-key-file=/certs/server-key.pem", "--https-protocols=TLSv1.3", "--import-realm"}
	if _, err := dockerCommand(t, 2*time.Minute, args...); err != nil {
		t.Fatal("start disposable Keycloak", err)
	}
	port, err := dockerCommand(t, 10*time.Second, "port", name, "8443/tcp")
	if err != nil {
		t.Fatal(err)
	}
	address := strings.TrimSpace(string(port))
	host, _, err := net.SplitHostPort(address)
	if err != nil || host != "127.0.0.1" {
		t.Fatal("test Keycloak must have one loopback listener")
	}
	options := Options{ServerURL: "https://" + address, Realm: "provider-test", ClientID: "operator", SecretFile: secret, CAFile: ca}
	client, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for {
		if _, err = client.FindClient(ctx, "catalog"); err == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("test Keycloak did not become ready", err)
		}
		state, inspectErr := dockerCommand(t, 10*time.Second, "inspect", "--format", "{{.State.Running}}", name)
		if inspectErr != nil || strings.TrimSpace(string(state)) != "true" {
			t.Fatal("test Keycloak stopped before readiness")
		}
		select {
		case <-ctx.Done():
			t.Fatal("test Keycloak readiness deadline")
		case <-time.After(500 * time.Millisecond):
		}
	}
	return client, options
}

func TestProviderLive(t *testing.T) {
	if os.Getenv("STEGO_REQUIRE_KEYCLOAK_PROVIDER") != "1" {
		t.Skip("real Keycloak provider gate requires CI")
	}
	client, options := liveProvider(t)
	runtime, recorder := tracing.ProviderTestRuntime()
	defer runtime.Close()
	ctx := runtime.Context(context.Background())
	blindOptions := options
	blindOptions.ClientID = "scope-blind-operator"
	blindOptions.SecretFile = filepath.Join(t.TempDir(), "scope-blind-secret")
	if err := os.WriteFile(blindOptions.SecretFile, []byte("provider-test-scope-blind-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	blind, err := New(blindOptions)
	if err != nil {
		t.Fatal(err)
	}
	defer blind.Close()
	if err = blind.confirmScopeVisibility(ctx); !errors.Is(err, ErrScopePolicy) {
		t.Fatal("real hidden scope inventory was accepted", err)
	}
	t.Log("Real scope visibility check: query-only administrator cannot prove an empty scope policy")
	scopeOptions := options
	scopeOptions.ClientID = "realm-scope-operator"
	scopeOptions.SecretFile = filepath.Join(t.TempDir(), "realm-scope-secret")
	if err := os.WriteFile(scopeOptions.SecretFile, []byte("provider-test-realm-scope-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	scopeAdmin, err := New(scopeOptions)
	if err != nil {
		t.Fatal(err)
	}
	defer scopeAdmin.Close()
	testLiveClientOnlyRoles(t, ctx, options)
	testLiveRolePolicies(t, client, scopeAdmin, ctx)
	testLiveNativeClients(t, client, ctx, options.CAFile)
	for _, application := range []struct{ name, key, value string }{{"new-catalog", "stego.owner.product", "object-3"}, {"new-worker", "stego.owner.pipeline", "run-4"}} {
		binding := ClientBinding{ID: "stego-" + application.name, ClientID: application.name, Attributes: map[string]string{application.key: application.value}}
		policy := ServiceAccountPolicy{DisplayName: "Created worker", AccessTokenLifetimeSeconds: 300}
		created, err := client.CreateDisabledServiceAccount(ctx, binding, policy)
		if err != nil || created.Enabled || created.ID != binding.ID {
			t.Fatal("real disabled creation failed", err)
		}
		if _, err := client.CreateDisabledServiceAccount(ctx, binding, policy); !errors.Is(err, ErrConflict) {
			t.Fatal("real duplicate creation did not return conflict", err)
		}
		nameConflict := binding
		nameConflict.ID += "-other"
		if _, err := client.CreateDisabledServiceAccount(ctx, nameConflict, policy); !errors.Is(err, ErrConflict) {
			t.Fatal("real client-name conflict was accepted", err)
		}
		if _, err := client.GetClient(ctx, nameConflict.ID); !errors.Is(err, ErrNotFound) {
			t.Fatal("name conflict left another client", err)
		}
		credential, err := client.GetClientSecret(ctx, binding)
		if err != nil || credential.Reveal() == "" {
			t.Fatal("created client has no provider secret", err)
		}
		form := url.Values{"grant_type": {"client_credentials"}, "client_id": {binding.ClientID}, "client_secret": {credential.Reveal()}}
		denied, err := client.http.Do(ctx, http.MethodPost, "/realms/provider-test/protocol/openid-connect/token", http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}, []byte(form.Encode()))
		if err != nil || (denied.StatusCode != http.StatusUnauthorized && denied.StatusCode != http.StatusBadRequest) {
			t.Fatal("disabled client token request was not denied", err)
		}
		var rejection struct {
			Error       string `json:"error"`
			AccessToken string `json:"access_token"`
		}
		if decode(denied.Body, &rejection) != nil || rejection.AccessToken != "" || (rejection.Error != "invalid_client" && rejection.Error != "unauthorized_client") {
			t.Fatal("disabled client denial was not an authentication rejection")
		}
		if _, err := client.ResolveServiceAccountUser(ctx, binding); err != nil {
			t.Fatal("created client has no service-account user", err)
		}
		// A changed application policy exercises a real update and readback.
		policy.DisplayName = ""
		policy.AccessTokenLifetimeSeconds = 120
		if err := client.ConfigureDisabledServiceAccount(ctx, binding, policy); err != nil {
			t.Fatal("real disabled configuration failed", err)
		}
		if err := client.ConfigureDisabledServiceAccount(ctx, binding, policy); err != nil {
			t.Fatal("real converged configuration failed", err)
		}
		currentSecret, err := client.GetClientSecret(ctx, binding)
		if err != nil || currentSecret.Reveal() != credential.Reveal() {
			t.Fatal("configuration changed the provider secret", err)
		}
		client.Close()
		client, err = New(options)
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		if _, err := client.InspectDisabledServiceAccount(ctx, binding, policy); err != nil {
			t.Fatal("created configuration did not survive client reconstruction", err)
		}
		if err := client.DeleteClient(ctx, binding); err != nil {
			t.Fatal("created client cleanup failed", err)
		}
	}
	page, err := client.ListClients(ctx, Page{Size: 100})
	if err != nil || len(page) < 4 {
		t.Fatal("real provider inventory failed", err)
	}
	foreign, err := client.FindClient(ctx, "foreign-service")
	if err != nil {
		t.Fatal(err)
	}
	rejected := ClientBinding{ID: foreign.ID, ClientID: foreign.ClientID, Attributes: map[string]string{"product.owner": "object-1"}}
	if err := client.DisableClient(ctx, rejected); !errors.Is(err, ErrOwnership) {
		t.Fatal("foreign disablement was accepted", err)
	}
	if err := client.DeleteClient(ctx, rejected); !errors.Is(err, ErrOwnership) {
		t.Fatal("foreign deletion was accepted", err)
	}
	for _, policy := range []struct{ name, key, value string }{{"catalog", "product.owner", "object-1"}, {"batch-worker", "pipeline.job", "run-2"}} {
		record, err := client.FindClient(ctx, policy.name)
		if err != nil {
			t.Fatal(err)
		}
		binding := ClientBinding{ID: record.ID, ClientID: policy.name, Attributes: map[string]string{policy.key: policy.value}}
		credential, err := client.GetClientSecret(ctx, binding)
		if err != nil || credential.Reveal() != "provider-test-client-secret" {
			t.Fatal("real credential read failed", err)
		}
		subject, err := client.ResolveServiceAccountUser(ctx, binding)
		if err != nil || subject.ID == "" {
			t.Fatal("real service-account resolution failed", err)
		}
		if err := client.DisableClient(ctx, binding); err != nil {
			t.Fatal("real disablement failed", err)
		}
		if err := client.DisableClient(ctx, binding); err != nil {
			t.Fatal("repeated disablement failed", err)
		}
		client.Close()
		client, err = New(options)
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		current, err := client.InspectClient(ctx, binding)
		if err != nil || current.Enabled {
			t.Fatal("disablement did not survive client reconstruction", err)
		}
		if err := client.DeleteClient(ctx, binding); err != nil {
			t.Fatal("real deletion failed", err)
		}
		if _, err := client.GetClient(ctx, binding.ID); !errors.Is(err, ErrNotFound) {
			t.Fatal("deleted client remains", err)
		}
		if err := client.DeleteClient(ctx, binding); err != nil {
			t.Fatal("repeated deletion failed", err)
		}
	}
	if current, err := client.GetClient(ctx, foreign.ID); err != nil || !current.Enabled {
		t.Fatal("foreign client changed", err)
	}
	spans := recorder.Ended()
	if len(spans) < 10 {
		t.Fatal("real provider calls have no telemetry")
	}
	for _, span := range spans {
		data, err := json.Marshal(span.Attributes())
		if err != nil {
			t.Fatal(err)
		}
		for _, private := range []string{"provider-test-admin-secret", "provider-test-client-secret", foreign.ID, options.ServerURL, "shared-catalog", "shared-pipeline", "catalog-read", "pipeline-execute"} {
			if strings.Contains(string(data), private) {
				t.Fatal("real provider telemetry exposed request data")
			}
		}
	}
	t.Log("Real Keycloak: two ownership policies, disabled creation, conflict rejection, configuration, foreign-client denial, credential reads, service-account resolution, disablement, client reconstruction, confirmed deletion, and trace privacy passed")
}

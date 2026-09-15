package allocation

import (
	"context"
	"encoding/json"
	"errors"
	kube "example.com/widget/out/kubernetes"
	"fmt"
	"strings"
	"testing"
	"time"
)

const endpointEnv = "STEGO_ALLOCATION_NETWORK_ENDPOINTS"
const endpointValues = `{"kubernetes":["192.0.2.10:443"],"provider":["[2001:db8::1]:5432","192.0.2.10:443"]}`

func TestAllocationEndpointLifecycle(t *testing.T) {
	t.Setenv(endpointEnv, endpointValues)
	a, s := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := a.Ensure(ctx, "tenant", networkName, "owner-1"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	policy := s.objects[networkPath]
	rules := policy["spec"].(map[string]any)["egress"].([]any)
	if len(rules) != 2 {
		t.Fatal("endpoint aliases were not deduplicated", rules)
	}
	for i, pair := range [][2]string{{"192.0.2.10/32", "443"}, {"2001:db8::1/128", "5432"}} {
		r := rules[i].(map[string]any)
		peer := r["to"].([]any)[0].(map[string]any)
		port := r["ports"].([]any)[0].(map[string]any)
		if kube.String(peer, "ipBlock", "cidr") != pair[0] || fmt.Sprint(port["port"]) != pair[1] || port["protocol"] != "TCP" {
			t.Fatal("incorrect endpoint rule", r)
		}
	}
	uid := kube.String(policy, "metadata", "uid")
	s.mu.Unlock()
	// A new operator configuration changes the expected policy after restart.
	t.Setenv(endpointEnv, `{"kubernetes":["192.0.2.11:443"],"provider":["192.0.2.12:5432"]}`)
	next, err := New(a.client, "control")
	if err != nil {
		t.Fatal(err)
	}
	if err = next.RequireNamespace(ctx, "tenant", networkName, "owner-1"); err == nil {
		t.Fatal("old endpoints permitted work")
	}
	if err = next.Ensure(ctx, "tenant", networkName, "owner-1"); !errors.Is(err, ErrPending) {
		t.Fatal(err)
	}
	if err = next.Ensure(ctx, "tenant", networkName, "owner-1"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.writes) != 7 || kube.String(s.objects[networkPath], "metadata", "uid") != uid {
		t.Fatal("endpoint update changed identity or repeated writes")
	}
	data := fmt.Sprint(s.objects[networkPath]["spec"])
	if strings.Contains(data, "192.0.2.10") || strings.Contains(data, "2001:db8") {
		t.Fatal("retired endpoints remain", data)
	}
}
func TestAllocationEndpointRejectsInvalidEnvironment(t *testing.T) {
	t.Setenv(endpointEnv, endpointValues)
	a, s := fixture(t)
	for _, input := range []string{
		"", "null", "[]", "{}", strings.Repeat(" ", 8193),
		`{"kubernetes":[],"provider":["192.0.2.10:443"]}`,
		`{"kubernetes":["192.0.2.10:443"]}`,
		`{"kubernetes":["192.0.2.10:443"],"provider":["127.0.0.1:443"]}`,
		`{"kubernetes":["192.0.2.10:443"],"provider":["0.0.0.0:443"]}`,
		`{"kubernetes":["192.0.2.10:443"],"provider":["169.254.1.1:443"]}`,
		`{"kubernetes":["192.0.2.10:443"],"provider":["[::ffff:192.0.2.10]:443"]}`,
		`{"kubernetes":["192.0.2.10:443"],"provider":["[2001:db8::1%eth0]:443"]}`,
		`{"kubernetes":["192.0.2.10:443"],"provider":["database.example:5432"]}`,
		`{"kubernetes":["192.0.2.10:443"],"provider":["192.0.2.12:0"]}`,
		`{"kubernetes":["192.0.2.10:443"],"provider":["192.0.2.12:5432","192.0.2.12:5432"]}`,
		`{"kubernetes":["192.0.2.10:443"],"provider":["192.0.2.12:5432"],"provider":["192.0.2.13:5432"]}`,
		`{"kubernetes":["192.0.2.10:443"],"provider":["192.0.2.12:5432"],"unknown":["192.0.2.13:5432"]}`,
		endpointValues + "{}",
	} {
		t.Setenv(endpointEnv, input)
		if _, err := New(a.client, "control"); err == nil {
			t.Fatal("invalid endpoint configuration accepted", input)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.requests != 0 {
		t.Fatal("invalid startup configuration contacted Kubernetes")
	}
}

func TestAllocationEndpointAddressBound(t *testing.T) {
	t.Setenv(endpointEnv, endpointValues)
	a, _ := fixture(t)
	values := map[string][]string{"kubernetes": {"192.0.2.10:443"}}
	for i := 1; i <= 32; i++ {
		values["provider"] = append(values["provider"], fmt.Sprintf("192.0.2.%d:5432", i))
	}
	for _, count := range []int{31, 32} {
		selected := map[string][]string{"kubernetes": values["kubernetes"], "provider": values["provider"][:count]}
		data, err := json.Marshal(selected)
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv(endpointEnv, string(data))
		_, err = New(a.client, "control")
		if count == 31 && err != nil {
			t.Fatal("32 total endpoints rejected", err)
		}
		if count == 32 && err == nil {
			t.Fatal("33 total endpoints accepted")
		}
	}
}

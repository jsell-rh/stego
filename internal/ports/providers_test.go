package ports

import (
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/registry"
)

func component(name string, provides, requires []string) *registry.Component {
	c := &registry.Component{Name: name, Stage: "ga"}
	for _, port := range provides {
		c.Provides = append(c.Provides, registry.Port{Name: port})
	}
	for _, port := range requires {
		c.Requires = append(c.Requires, registry.Port{Name: port})
	}
	return c
}

func set(components ...*registry.Component) map[string]*registry.Component {
	active := map[string]*registry.Component{}
	for _, c := range components {
		active[c.Name] = c
	}
	return active
}

func TestPortProviders(t *testing.T) {
	apiA := component("api-a", nil, []string{"store-port", "auth-port"})
	storeA := component("store-a", []string{"store-port"}, nil)
	storeB := component("store-b", []string{"store-port"}, nil)
	authA := component("auth-a", []string{"auth-port"}, nil)

	t.Run("ST-SUP-SP02-T A port has one provider only when exactly one component of the active component set provides it", func(t *testing.T) {
		if err := HasOneProvider(set(apiA, authA), "store-port"); err == nil || !strings.Contains(err.Error(), "store-port") || !strings.Contains(err.Error(), "no provider") {
			t.Fatalf("the set with no provider: %v", err)
		}
		if err := HasOneProvider(set(apiA, storeA, authA), "store-port"); err != nil {
			t.Fatalf("the set with store-a alone: %v", err)
		}
		err := HasOneProvider(set(apiA, storeA, storeB, authA), "store-port")
		if err == nil {
			t.Fatal("the set with two providers holds")
		}
		for _, word := range []string{"store-port", "store-a", "store-b"} {
			if !strings.Contains(err.Error(), word) {
				t.Fatalf("the report does not name %s: %v", word, err)
			}
		}
		only := set(apiA, storeA, authA)
		for _, port := range []string{"store-port", "auth-port"} {
			if err := HasOneProvider(only, port); err != nil {
				t.Fatalf("%s over the set with store-a alone: %v", port, err)
			}
		}
	})

	t.Run("ST-SUP-SP03-T A set answers its ports only when every port that a component of it requires has a provider in it", func(t *testing.T) {
		if err := AnswersPorts(set(apiA, storeA, authA)); err != nil {
			t.Fatalf("the full set: %v", err)
		}
		err := AnswersPorts(set(apiA, storeA))
		if err == nil || !strings.Contains(err.Error(), "auth-port") || strings.Contains(err.Error(), "store-port") {
			t.Fatalf("the set with no provider of auth-port: %v", err)
		}
		err = AnswersPorts(set(apiA))
		if err == nil || !strings.Contains(err.Error(), "auth-port") || !strings.Contains(err.Error(), "store-port") {
			t.Fatalf("the set of api-a alone: %v", err)
		}
	})
}

package registry

import (
	"bytes"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ServiceUse is one row of the component consumers read model (ST-SUP-RM03):
// a recorded service built from one component at one version.
type ServiceUse struct {
	Service  string
	Checkout string
	// Component and Version are what the service's generation record names.
	Component string
	Version   string
}

// committedService is the part of a service's committed records that the
// read model reads: the name in its declaration and the components of its
// last generation record.
type committedService struct {
	Name        string `yaml:"name"`
	LastApplied struct {
		Components map[string]struct {
			Version string `yaml:"version"`
		} `yaml:"components"`
	} `yaml:"last_applied"`
}

// ComponentConsumers fills the component consumers read model from the
// committed records of service checkouts: the service name in service.yaml
// and the components in .stego/state.yaml, each as the checkout's HEAD holds
// it. It reads with git show and writes nothing in any checkout.
func ComponentConsumers(checkouts []string) (*ConsumersReader, error) {
	var uses []ServiceUse
	for _, checkout := range checkouts {
		declaration, err := committedFile(checkout, "service.yaml")
		if err != nil {
			return nil, err
		}
		var service committedService
		if err := yaml.Unmarshal(declaration, &service); err != nil {
			return nil, fmt.Errorf("checkout %s: service.yaml: %w", checkout, err)
		}
		if service.Name == "" {
			return nil, fmt.Errorf("checkout %s: service.yaml names no service", checkout)
		}
		state, err := committedFile(checkout, ".stego/state.yaml")
		if err != nil {
			return nil, err
		}
		if err := yaml.Unmarshal(state, &service); err != nil {
			return nil, fmt.Errorf("checkout %s: .stego/state.yaml: %w", checkout, err)
		}
		for component, record := range service.LastApplied.Components {
			uses = append(uses, ServiceUse{Service: service.Name, Checkout: checkout, Component: component, Version: record.Version})
		}
	}
	slices.SortFunc(uses, func(a, b ServiceUse) int {
		return strings.Compare(a.Component+"\x00"+a.Service, b.Component+"\x00"+b.Service)
	})
	return &ConsumersReader{uses: uses}, nil
}

// committedFile reads a file as the HEAD commit of a checkout holds it.
func committedFile(checkout, name string) ([]byte, error) {
	command := exec.Command("git", "show", "HEAD:"+name)
	command.Dir = checkout
	var stderr bytes.Buffer
	command.Stderr = &stderr
	data, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("checkout %s: reading the committed %s: %v: %s", checkout, name, err, strings.TrimSpace(stderr.String()))
	}
	return data, nil
}

// ListConsumers answers ST-SUP-QRY03: every recorded service that uses one
// component, with the version it was built from, in service order.
func ListConsumers(reader *ConsumersReader, component string) []ServiceUse {
	var found []ServiceUse
	for _, use := range reader.Uses() {
		if use.Component == component {
			found = append(found, use)
		}
	}
	return found
}

// MissedSunset is a service that still uses a component past its sunset.
type MissedSunset struct {
	ServiceUse
	Stage  string
	Sunset string
}

// MissedSunsets answers ST-SUP-QRY04: every recorded service that still uses
// a component of the active registry that is past its sunset on the given
// day (ST-SUP-SP04), with the component and its sunset date.
func MissedSunsets(reader *ConsumersReader, active *Registry, today time.Time) []MissedSunset {
	var missed []MissedSunset
	for _, use := range reader.Uses() {
		component := active.Component(use.Component)
		if PastSunset(component, today) {
			missed = append(missed, MissedSunset{ServiceUse: use, Stage: string(component.Stage), Sunset: component.Sunset})
		}
	}
	return missed
}

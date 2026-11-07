package fleet

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"time"

	"github.com/jsell-rh/stego/internal/lifecycle"
)

// The OpenVEX statuses a statement of the sweep takes.
const (
	StatusAffected           = "affected"
	StatusFixed              = "fixed"
	StatusNotAffected        = "not_affected"
	StatusUnderInvestigation = "under_investigation"
)

// VEXDocument is one OpenVEX document (https://openvex.dev/ns/v0.2.0).
type VEXDocument struct {
	Context    string         `json:"@context"`
	ID         string         `json:"@id"`
	Author     string         `json:"author"`
	Timestamp  string         `json:"timestamp"`
	Version    int            `json:"version"`
	Statements []VEXStatement `json:"statements"`
}

// VEXStatement is one statement: one advisory and one service.
type VEXStatement struct {
	Vulnerability   VEXVulnerability `json:"vulnerability"`
	Products        []VEXProduct     `json:"products"`
	Status          string           `json:"status"`
	Justification   string           `json:"justification,omitempty"`
	ImpactStatement string           `json:"impact_statement,omitempty"`
	ActionStatement string           `json:"action_statement,omitempty"`
}

// VEXVulnerability names the advisory.
type VEXVulnerability struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// VEXProduct names the service, or the checkout of an unreadable one.
type VEXProduct struct {
	ID string `json:"@id"`
}

// advisory is one supersession or withdrawal of the registry, as a statement reads it.
type advisory struct {
	id, summary string
	subject     lifecycle.Subject
	successor   *lifecycle.Subject
}

func advisories(lifecycles []lifecycle.Lifecycle) []advisory {
	var result []advisory
	for _, l := range lifecycles {
		if s := l.Supersession; s != nil {
			successor := s.Successor
			result = append(result, advisory{id: s.AdvisoryID, summary: s.Summary, subject: l.Subject, successor: &successor})
		}
		if w := l.Withdrawal; w != nil {
			result = append(result, advisory{id: w.AdvisoryID, summary: w.Reason, subject: l.Subject})
		}
	}
	return result
}

// ServiceProduct is the product identifier of a recorded service in a VEX statement.
func ServiceProduct(name string) string {
	return "pkg:generic/" + name
}

// CheckoutProduct is the product identifier of a checkout without a readable record.
func CheckoutProduct(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		absolute = path
	}
	return "file://" + filepath.ToSlash(absolute)
}

// statement gives the status of one service for one advisory, from its pins alone.
func statement(a advisory, service Service) VEXStatement {
	result := VEXStatement{Vulnerability: VEXVulnerability{Name: a.id, Description: a.summary},
		Products: []VEXProduct{{ID: ServiceProduct(service.Name)}}}
	if a.subject.Kind != lifecycle.ComponentPackage {
		result.Status = StatusUnderInvestigation
		return result
	}
	holdsComponent := false
	for _, pin := range service.Pins {
		switch {
		case pin.Component == a.subject.Name && pin.Digest == a.subject.Digest:
			result.Status = StatusAffected
			if a.successor != nil {
				result.ActionStatement = "Take the successor " + a.successor.String() + " with a registry pin bump and a new run of stego apply."
			} else {
				result.ActionStatement = "Move off the withdrawn digest " + a.subject.String() + " with a registry pin bump and a new run of stego apply."
			}
			return result
		case a.successor != nil && pin.Component == a.successor.Name && pin.Digest == a.successor.Digest:
			result.Status = StatusFixed
			return result
		case pin.Component == a.subject.Name:
			holdsComponent = true
		}
	}
	result.Status = StatusNotAffected
	result.Justification = "component_not_present"
	if holdsComponent {
		result.Justification = "vulnerable_code_not_present"
		result.ImpactStatement = "The service pins another digest of " + a.subject.Name + "."
	}
	return result
}

// VEX is the query List the VEX statements of the fleet (ST-DIS-QRY06): from one sweep, one OpenVEX statement per
// service and advisory of the registry, with the status affected, fixed or not_affected from the committed records
// and the advisories alone, and under_investigation for a checkout without a readable record. It writes nothing.
func VEX(reader *SweepReader, lifecycles []lifecycle.Lifecycle, timestamp time.Time) VEXDocument {
	sweep := reader.Sweep()
	document := VEXDocument{Context: "https://openvex.dev/ns/v0.2.0", Author: "STEGO fleet sweep",
		Timestamp: timestamp.UTC().Format(time.RFC3339), Version: 1, Statements: []VEXStatement{}}
	for _, a := range advisories(lifecycles) {
		for _, service := range sweep.Services {
			document.Statements = append(document.Statements, statement(a, service))
		}
		for _, unknown := range sweep.Unknown {
			document.Statements = append(document.Statements, VEXStatement{
				Vulnerability: VEXVulnerability{Name: a.id, Description: a.summary},
				Products:      []VEXProduct{{ID: CheckoutProduct(unknown.Path)}},
				Status:        StatusUnderInvestigation})
		}
	}
	statements, _ := json.Marshal(document.Statements)
	sum := sha256.Sum256(statements)
	document.ID = "https://openvex.dev/docs/stego/fleet-" + hex.EncodeToString(sum[:16])
	return document
}

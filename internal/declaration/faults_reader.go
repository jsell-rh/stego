package declaration

import (
	"encoding/json"
	"fmt"

	"github.com/jsell-rh/stego/internal/types"
)

// FaultsReader holds the faults that one check built in memory and hands them to the query handler
// (ST-DECL-RM01-RD). The handler reads nothing else, and nothing is kept between runs.
type FaultsReader struct {
	faults   []ValidationError
	settings []Setting
	warnings []ValidationError
}

// Setting is one setting of the declaration as checking resolved it (ST-DECL-RM01): its path in the declaration,
// its value, and whether checking filled the value in by its default because the declaration omits it.
type Setting struct {
	Path    string `json:"path"`
	Value   any    `json:"value"`
	Default bool   `json:"default"`
}

// ResolvedSettings gives each setting of the bound settings of the declaration, with every value that the
// declaration omits filled in by its default, in a fixed order.
func ResolvedSettings(service *types.ServiceDeclaration) []Setting {
	var settings []Setting
	settings = append(settings, idempotencySettings(service)...)
	settings = append(settings, tenancySettings(service)...)
	settings = append(settings, auditSettings(service)...)
	settings = append(settings, entitySettings(service)...)
	settings = append(settings, slotSettings(service)...)
	settings = append(settings, modeSettings(service)...)
	settings = append(settings, commandSettings(service)...)
	settings = append(settings, workerSettings(service)...)
	settings = append(settings, observationSettings(service)...)
	settings = append(settings, schemaChangeSettings(service)...)
	return settings
}

// FaultsDocument is the structured form of the faults of one declaration (ST-DECL-RM01): every fault in the
// order the check found it, whether the declaration is acceptable, which is when there is no fault, and the
// settings as checking resolved them, each default marked.
type FaultsDocument struct {
	Accepted bool              `json:"accepted"`
	Faults   []ValidationError `json:"faults"`
	Settings []Setting         `json:"settings"`
	// Warnings are the notices about accepted parts of the declaration, each with the parts of a fault. They do
	// not change Accepted (decision on ST-DECL-Q15).
	Warnings []ValidationError `json:"warnings"`
}

// NewFaultsReader reads the faults and the resolved settings of one check.
func NewFaultsReader(result *ValidationResult) FaultsReader {
	reader := FaultsReader{faults: []ValidationError{}, settings: []Setting{}, warnings: []ValidationError{}}
	if result == nil {
		return reader
	}
	reader.settings = append(reader.settings, result.Settings...)
	reader.warnings = append(reader.warnings, result.Warnings...)
	for _, fault := range result.Errors {
		if fault.Rule == "" {
			fault.Rule = fault.Category
		}
		if fault.Path == "" {
			fault.Path = "service.yaml"
		}
		reader.faults = append(reader.faults, fault)
	}
	return reader
}

// Document gives the faults as one structured document.
func (r FaultsReader) Document() FaultsDocument {
	return FaultsDocument{Accepted: len(r.faults) == 0, Faults: append([]ValidationError{}, r.faults...), Settings: append([]Setting{}, r.settings...), Warnings: append([]ValidationError{}, r.warnings...)}
}

// JSON gives the structured document as JSON, for an agent to read.
func (r FaultsReader) JSON() ([]byte, error) {
	return json.MarshalIndent(r.Document(), "", "  ")
}

// Prose gives the faults for a person, each rendered from its structured fields, and then the warnings.
func (r FaultsReader) Prose() string {
	prose := FormatValidation(&ValidationResult{Errors: r.faults})
	if len(r.warnings) > 0 {
		prose += fmt.Sprintf("\n%d warning(s):\n\n", len(r.warnings))
		for _, warning := range r.warnings {
			prose += "  " + warning.Prose() + "\n"
		}
	}
	return prose
}

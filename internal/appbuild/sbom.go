package appbuild

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
)

// SBOMFormat is the format of one software bill of materials: exactly SPDX or
// CycloneDX, each in its standard machine-readable JSON form (ST-VO49-C01).
type SBOMFormat string

const (
	SPDX      SBOMFormat = "SPDX"
	CycloneDX SBOMFormat = "CycloneDX"
)

// Validate refuses a format that is not exactly SPDX or CycloneDX.
func (format SBOMFormat) Validate() error {
	switch format {
	case SPDX, CycloneDX:
		return nil
	}
	return errors.New("an SBOM format must be exactly SPDX or CycloneDX")
}

// FileName is the name of the SBOM file of this format beside image.json.
func (format SBOMFormat) FileName() string {
	if format == SPDX {
		return "sbom.spdx.json"
	}
	return "sbom.cdx.json"
}

// PredicateType is the in-toto predicate type of a signed attestation that
// carries an SBOM of this format.
func (format SBOMFormat) PredicateType() string {
	if format == SPDX {
		return "https://spdx.dev/Document/v2.3"
	}
	return "https://cyclonedx.org/bom/v1.5"
}

// SBOM is one software bill of materials of an executable: its format, its
// document bytes, and the digest of those bytes.
type SBOM struct {
	Format   SBOMFormat
	Document []byte
	Digest   string
}

// sbomFormats is the fixed order in which the SBOMs are written and checked.
var sbomFormats = []SBOMFormat{SPDX, CycloneDX}

type sbomPackage struct {
	name, version, purl string
}

// sbomPackages reads the module inventory of the record into packages in the
// record's order. The record orders its modules by path, and validateRecord
// refuses another order. A replaced module is named by its replacement, and a
// local module has no version and no package URL version.
func sbomPackages(record *Record) []sbomPackage {
	packages := make([]sbomPackage, 0, len(record.Modules))
	for _, module := range record.Modules {
		selected := module
		if module.Replacement != nil {
			selected = *module.Replacement
		}
		purl := "pkg:golang/" + selected.Path
		if selected.Version != "" {
			purl += "@" + selected.Version
		}
		packages = append(packages, sbomPackage{name: module.Path, version: selected.Version, purl: purl})
	}
	return packages
}

type spdxChecksum struct {
	Algorithm     string `json:"algorithm"`
	ChecksumValue string `json:"checksumValue"`
}

type spdxReference struct {
	ReferenceCategory string `json:"referenceCategory"`
	ReferenceType     string `json:"referenceType"`
	ReferenceLocator  string `json:"referenceLocator"`
}

type spdxPackage struct {
	Name             string          `json:"name"`
	SPDXID           string          `json:"SPDXID"`
	VersionInfo      string          `json:"versionInfo,omitempty"`
	DownloadLocation string          `json:"downloadLocation"`
	FilesAnalyzed    bool            `json:"filesAnalyzed"`
	Checksums        []spdxChecksum  `json:"checksums,omitempty"`
	ExternalRefs     []spdxReference `json:"externalRefs,omitempty"`
}

type spdxRelationship struct {
	Element string `json:"spdxElementId"`
	Type    string `json:"relationshipType"`
	Related string `json:"relatedSpdxElement"`
}

type spdxDocument struct {
	SPDXVersion       string `json:"spdxVersion"`
	DataLicense       string `json:"dataLicense"`
	SPDXID            string `json:"SPDXID"`
	Name              string `json:"name"`
	DocumentNamespace string `json:"documentNamespace"`
	CreationInfo      struct {
		Created  string   `json:"created"`
		Creators []string `json:"creators"`
	} `json:"creationInfo"`
	Packages      []spdxPackage      `json:"packages"`
	Relationships []spdxRelationship `json:"relationships"`
}

type cdxHash struct {
	Algorithm string `json:"alg"`
	Content   string `json:"content"`
}

type cdxComponent struct {
	Type    string    `json:"type"`
	Ref     string    `json:"bom-ref"`
	Name    string    `json:"name"`
	Version string    `json:"version,omitempty"`
	PURL    string    `json:"purl,omitempty"`
	Hashes  []cdxHash `json:"hashes,omitempty"`
}

type cdxDependency struct {
	Ref       string   `json:"ref"`
	DependsOn []string `json:"dependsOn,omitempty"`
}

type cdxDocument struct {
	BOMFormat   string `json:"bomFormat"`
	SpecVersion string `json:"specVersion"`
	Version     int    `json:"version"`
	Metadata    struct {
		Component cdxComponent `json:"component"`
	} `json:"metadata"`
	Components   []cdxComponent  `json:"components"`
	Dependencies []cdxDependency `json:"dependencies"`
}

// WriteSBOMs writes the SPDX and the CycloneDX SBOM of the executable that the
// build record names, from the record alone: its target, its executable digest
// and its module inventory. It reads no clock, file or network, so the same
// record always gives the same bytes, in the fixed order SPDX, then CycloneDX
// (ST-BLD-DS01).
func WriteSBOMs(record *Record) ([]SBOM, error) {
	if record == nil {
		return nil, errors.New("SBOMs need a build record")
	}
	if err := validateRecord(record); err != nil {
		return nil, err
	}
	packages := sbomPackages(record)
	documents := make([]SBOM, 0, len(sbomFormats))
	for _, format := range sbomFormats {
		var value any
		switch format {
		case SPDX:
			value = spdxFromPackages(record, packages)
		case CycloneDX:
			value = cycloneDXFromPackages(record, packages)
		}
		data, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return nil, err
		}
		data = append(data, '\n')
		documents = append(documents, SBOM{Format: format, Document: data, Digest: digest(data)})
	}
	return documents, nil
}

func spdxFromPackages(record *Record, packages []sbomPackage) spdxDocument {
	var document spdxDocument
	document.SPDXVersion = "SPDX-2.3"
	document.DataLicense = "CC0-1.0"
	document.SPDXID = "SPDXRef-DOCUMENT"
	document.Name = record.Target
	// The namespace is unique to the executable and needs no clock or random value.
	document.DocumentNamespace = "https://stego.invalid/sbom/spdx/sha256/" + record.Artifact.SHA256
	document.CreationInfo.Created = "1970-01-01T00:00:00Z"
	document.CreationInfo.Creators = []string{"Tool: stego"}
	document.Packages = []spdxPackage{{Name: record.Target, SPDXID: "SPDXRef-Application", DownloadLocation: "NOASSERTION", Checksums: []spdxChecksum{{"SHA256", record.Artifact.SHA256}}}}
	document.Relationships = []spdxRelationship{{"SPDXRef-DOCUMENT", "DESCRIBES", "SPDXRef-Application"}}
	for index, item := range packages {
		id := "SPDXRef-Module-" + strconv.Itoa(index+1)
		document.Packages = append(document.Packages, spdxPackage{Name: item.name, SPDXID: id, VersionInfo: item.version, DownloadLocation: "NOASSERTION", ExternalRefs: []spdxReference{{"PACKAGE-MANAGER", "purl", item.purl}}})
		document.Relationships = append(document.Relationships, spdxRelationship{"SPDXRef-Application", "DEPENDS_ON", id})
	}
	return document
}

func cycloneDXFromPackages(record *Record, packages []sbomPackage) cdxDocument {
	var document cdxDocument
	document.BOMFormat = "CycloneDX"
	document.SpecVersion = "1.5"
	document.Version = 1
	document.Metadata.Component = cdxComponent{Type: "application", Ref: "application", Name: record.Target, Hashes: []cdxHash{{"SHA-256", record.Artifact.SHA256}}}
	document.Components = []cdxComponent{}
	application := cdxDependency{Ref: "application"}
	for index, item := range packages {
		ref := "module-" + strconv.Itoa(index+1)
		document.Components = append(document.Components, cdxComponent{Type: "library", Ref: ref, Name: item.name, Version: item.version, PURL: item.purl})
		application.DependsOn = append(application.DependsOn, ref)
	}
	document.Dependencies = []cdxDependency{application}
	return document
}

// writeSBOMFiles writes the SBOMs of the record into new files in root, one
// per format, and refuses to replace a file that exists.
func writeSBOMFiles(root string, record *Record) ([]SBOM, error) {
	sboms, err := WriteSBOMs(record)
	if err != nil {
		return nil, err
	}
	for _, sbom := range sboms {
		file, err := os.OpenFile(filepath.Join(root, sbom.Format.FileName()), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return nil, err
		}
		_, writeErr := file.Write(sbom.Document)
		closeErr := file.Close()
		if writeErr != nil {
			return nil, writeErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}
	return sboms, nil
}

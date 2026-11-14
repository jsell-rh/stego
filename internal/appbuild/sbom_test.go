package appbuild

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func moduleSum(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return "h1:" + base64.StdEncoding.EncodeToString(sum[:])
}

func sbomFixtureRecord() Record {
	record := fixtureRecord()
	record.Modules = []Module{
		{Path: "example.test/app", LocalPath: "."},
		{Path: "golang.org/x/mod", Version: "v0.41.0", Sum: moduleSum("A"), GoModSum: moduleSum("B")},
		{Path: "golang.org/x/net", Version: "v0.60.0", Sum: moduleSum("C"), GoModSum: moduleSum("D"), Replacement: &Module{Path: "example.test/net", Version: "v1.0.0", Sum: moduleSum("E"), GoModSum: moduleSum("F")}},
	}
	return record
}

func TestSBOMsAreWrittenFromTheModuleInventoryInFixedOrder(t *testing.T) {
	t.Run("ST-BLD-DS01-T01 The SBOMs are the same bytes from the same record and name each module of it", func(t *testing.T) {
		record := sbomFixtureRecord()
		if err := validateRecord(&record); err != nil {
			t.Fatal(err)
		}
		first, err := WriteSBOMs(&record)
		if err != nil {
			t.Fatal(err)
		}
		second, err := WriteSBOMs(&record)
		if err != nil {
			t.Fatal(err)
		}
		if len(first) != 2 || first[0].Format != SPDX || first[1].Format != CycloneDX {
			t.Fatal("the SBOMs are not SPDX, then CycloneDX")
		}
		for index := range first {
			if !bytes.Equal(first[index].Document, second[index].Document) || first[index].Digest != digest(first[index].Document) {
				t.Fatal("the same record gave different SBOM bytes")
			}
			text := string(first[index].Document)
			for _, want := range []string{"example.test/app", "pkg:golang/golang.org/x/mod@v0.41.0", "pkg:golang/example.test/net@v1.0.0", record.Artifact.SHA256} {
				if !strings.Contains(text, want) {
					t.Fatalf("the %s SBOM does not name %s", first[index].Format, want)
				}
			}
			if strings.Contains(text, "golang.org/x/net@v0.60.0") {
				t.Fatalf("the %s SBOM names a replaced module version", first[index].Format)
			}
		}
		changed := sbomFixtureRecord()
		changed.Modules[1].Version = "v0.42.0"
		other, err := WriteSBOMs(&changed)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(other[0].Document, first[0].Document) || bytes.Equal(other[1].Document, first[1].Document) {
			t.Fatal("a changed module inventory gave the same SBOMs")
		}
		if _, err := WriteSBOMs(nil); err == nil {
			t.Fatal("SBOMs were written with no build record")
		}
		invalid := sbomFixtureRecord()
		invalid.Modules[0], invalid.Modules[1] = invalid.Modules[1], invalid.Modules[0]
		if _, err := WriteSBOMs(&invalid); err == nil {
			t.Fatal("SBOMs were written from a module inventory out of order")
		}
	})
}

func TestSBOMsNameTheSourceOfTheRuntimePin(t *testing.T) {
	t.Run("ST-BLD-DS01-T02 The SBOMs name the runtime folder as the source of the runtime pin, and no source for other modules", func(t *testing.T) {
		for source, comment := range map[string]string{
			RuntimeFolderSource:   "Supplied by source:registry/runtime, the runtime folder of the source revision, not by https://proxy.golang.org (ST-BLD-UC02-R05).",
			RuntimeCheckoutSource: "Supplied by checkout:registry/runtime, the runtime folder of the STEGO checkout that --runtime-folder named, which hashes to the runtime pin that the stego binary carries, not by https://proxy.golang.org (ST-BLD-UC02-R05).",
		} {
			record := sbomFixtureRecord()
			runtimeModule := Module{Path: RuntimeModule, Version: "v0.33.0", Sum: moduleSum("G"), GoModSum: moduleSum("H"), Source: source}
			record.Modules = append([]Module{record.Modules[0], runtimeModule}, record.Modules[1:]...)
			if err := validateRecord(&record); err != nil {
				t.Fatal(err)
			}
			sboms, err := WriteSBOMs(&record)
			if err != nil {
				t.Fatal(err)
			}
			var spdx spdxDocument
			if err := json.Unmarshal(sboms[0].Document, &spdx); err != nil {
				t.Fatal(err)
			}
			for _, pkg := range spdx.Packages {
				want := ""
				if pkg.Name == RuntimeModule {
					want = comment
				}
				if pkg.Comment != want {
					t.Errorf("the SPDX package %s has the comment %q; expected %q", pkg.Name, pkg.Comment, want)
				}
			}
			var cdx cdxDocument
			if err := json.Unmarshal(sboms[1].Document, &cdx); err != nil {
				t.Fatal(err)
			}
			for _, component := range cdx.Components {
				var want []cdxProperty
				if component.Name == RuntimeModule {
					want = []cdxProperty{{sbomSourceProperty, source}}
				}
				if len(component.Properties) != len(want) || len(want) == 1 && component.Properties[0] != want[0] {
					t.Errorf("the CycloneDX component %s has the properties %v; expected %v", component.Name, component.Properties, want)
				}
			}
		}
		plain := sbomFixtureRecord()
		before, err := WriteSBOMs(&plain)
		if err != nil {
			t.Fatal(err)
		}
		for _, sbom := range before {
			if strings.Contains(string(sbom.Document), "comment") || strings.Contains(string(sbom.Document), "properties") {
				t.Fatalf("the %s SBOM of a record with no module source names a source", sbom.Format)
			}
		}
	})
}

func TestSBOMFormatIsSPDXOrCycloneDX(t *testing.T) {
	t.Run("ST-VO49-C01-T An SBOM format is exactly SPDX or CycloneDX in its standard form", func(t *testing.T) {
		for _, format := range []SBOMFormat{"", "spdx", "CycloneDX ", "SWID", "SPDX,CycloneDX"} {
			if format.Validate() == nil {
				t.Fatalf("the SBOM format %q was accepted", format)
			}
		}
		record := sbomFixtureRecord()
		sboms, err := WriteSBOMs(&record)
		if err != nil {
			t.Fatal(err)
		}
		for _, sbom := range sboms {
			if err := sbom.Format.Validate(); err != nil {
				t.Fatal(err)
			}
			var document map[string]any
			if err := json.Unmarshal(sbom.Document, &document); err != nil {
				t.Fatal(err)
			}
			switch sbom.Format {
			case SPDX:
				if document["spdxVersion"] != "SPDX-2.3" || document["SPDXID"] != "SPDXRef-DOCUMENT" || document["dataLicense"] != "CC0-1.0" {
					t.Fatal("the SPDX SBOM is not an SPDX 2.3 JSON document")
				}
			case CycloneDX:
				if document["bomFormat"] != "CycloneDX" || document["specVersion"] != "1.5" {
					t.Fatal("the CycloneDX SBOM is not a CycloneDX 1.5 JSON document")
				}
			}
		}
	})
}

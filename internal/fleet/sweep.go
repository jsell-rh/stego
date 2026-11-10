// Package fleet is the fleet knowledge of Distributing (ST-DIS-RM01): every service whose committed record a sweep
// reads in the checkouts a platform engineer names, with each component it pins. The committed record is the
// durable form of Generating's event Service build applied, and its format version is the contract the sweep reads
// (ST-DIS-UC02-R01): the sweep reads files only, with no compiler, no network and no service to operate.
package fleet

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"

	"gopkg.in/yaml.v3"
)

// RecordPath is where a service checkout keeps its committed record.
const RecordPath = ".stego/state.yaml"

// OutputFolder is the folder of a checkout that holds generated output.
const OutputFolder = "out"

// RecordFormats are the record formats the sweep reads: format version 2 names each component's digest, and format
// version 3 adds the archetype with its runtime library pin, and format version 4 adds the data path, which the sweep
// does not read.
var RecordFormats = []int{2, 3, 4}

// maxRecordBytes bounds one record.
const maxRecordBytes = 8 << 20

var componentDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Pin is one component a service pins, with the version and the component digest it holds.
type Pin struct {
	Component string `json:"component"`
	Version   string `json:"version"`
	Digest    string `json:"digest"`
}

// Runtime is the runtime library version the service's archetype pinned at its last run (ST-DIS-RM01, Runtime
// version): its module path, tagged version and go.sum hash.
type Runtime struct {
	Module  string `json:"module" yaml:"module"`
	Version string `json:"version" yaml:"version"`
	Sum     string `json:"sum" yaml:"sum"`
}

// Service is one checkout whose record the sweep read.
type Service struct {
	Name string `json:"service"`
	Path string `json:"path"`
	Pins []Pin  `json:"pins"`
	// Runtime is nil for a record of format version 2, which holds no archetype.
	Runtime *Runtime `json:"runtime,omitempty"`
}

// Unknown is one checkout without a readable record, with the reason. It is never left out of an answer.
type Unknown struct {
	Path string `json:"path"`
	// FormatVersion is the format version of a record the sweep does not read; zero when there is none.
	FormatVersion int    `json:"format_version,omitempty"`
	Reason        string `json:"reason"`
}

// Sweep is the read model Fleet as one run builds it in memory (ST-DIS-RM01-TB): each named checkout, in the
// order given, as a recorded service or as unknown. Nothing is kept between runs.
type Sweep struct {
	Services []Service
	Unknown  []Unknown
	// Order is each checkout path in the order the engineer named them.
	Order []string
}

// record is the part of the committed record that the sweep reads, by the contract of its format version.
type record struct {
	FormatVersion int `yaml:"format_version"`
	LastApplied   *struct {
		ServiceName string `yaml:"service_name"`
		Components  map[string]struct {
			Version string `yaml:"version"`
			SHA     string `yaml:"sha"`
		} `yaml:"components"`
		Archetype *struct {
			Runtime *Runtime `yaml:"runtime"`
		} `yaml:"archetype"`
	} `yaml:"last_applied"`
}

// Project sweeps the checkouts once: it reads the committed record of each one, and nothing else.
func Project(checkouts []string) Sweep {
	var sweep Sweep
	for _, checkout := range checkouts {
		sweep.Order = append(sweep.Order, checkout)
		service, unknown := readCheckout(checkout)
		if unknown != nil {
			sweep.Unknown = append(sweep.Unknown, *unknown)
			continue
		}
		sweep.Services = append(sweep.Services, service)
	}
	return sweep
}

func readCheckout(checkout string) (Service, *Unknown) {
	unknown := func(format int, reason string, args ...any) (Service, *Unknown) {
		return Service{}, &Unknown{Path: checkout, FormatVersion: format, Reason: fmt.Sprintf(reason, args...)}
	}
	data, err := readBounded(filepath.Join(checkout, RecordPath))
	if errors.Is(err, os.ErrNotExist) {
		if info, statErr := os.Stat(filepath.Join(checkout, OutputFolder)); statErr == nil && info.IsDir() {
			return unknown(0, "it holds generated output under %s and no record %s", OutputFolder, RecordPath)
		}
		return unknown(0, "it holds no record %s", RecordPath)
	}
	if err != nil {
		return unknown(0, "its record cannot be read: %v", err)
	}
	var head struct {
		FormatVersion int `yaml:"format_version"`
	}
	if err := yaml.Unmarshal(data, &head); err != nil {
		return unknown(0, "its record is not valid YAML: %v", err)
	}
	if !slices.Contains(RecordFormats, head.FormatVersion) {
		return unknown(head.FormatVersion, "its record has format version %d, and the sweep reads only format versions 2, 3 and 4, which name each component digest", head.FormatVersion)
	}
	var r record
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&r); err != nil {
		return unknown(r.FormatVersion, "its record does not follow format version %d: %v", head.FormatVersion, err)
	}
	if r.LastApplied == nil || r.LastApplied.ServiceName == "" {
		return unknown(r.FormatVersion, "its record names no service")
	}
	service := Service{Name: r.LastApplied.ServiceName, Path: checkout}
	for name, component := range r.LastApplied.Components {
		if !componentDigest.MatchString(component.SHA) {
			return unknown(r.FormatVersion, "its record gives component %s no component digest of 64 lower-case hexadecimal digits", name)
		}
		service.Pins = append(service.Pins, Pin{Component: name, Version: component.Version, Digest: component.SHA})
	}
	if r.FormatVersion >= 3 {
		if r.LastApplied.Archetype == nil || r.LastApplied.Archetype.Runtime == nil || r.LastApplied.Archetype.Runtime.Version == "" {
			return unknown(r.FormatVersion, "its record of format version %d names no runtime library pin", r.FormatVersion)
		}
		service.Runtime = r.LastApplied.Archetype.Runtime
	}
	sort.Slice(service.Pins, func(i, j int) bool { return service.Pins[i].Component < service.Pins[j].Component })
	return service, nil
}

func readBounded(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxRecordBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxRecordBytes {
		return nil, fmt.Errorf("it is larger than %d bytes", maxRecordBytes)
	}
	return data, nil
}

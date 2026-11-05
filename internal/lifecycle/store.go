package lifecycle

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"strconv"
	"strings"
	"time"
)

// Folder is the folder of the registry that holds the lifecycle records, one OSV advisory per file.
const Folder = "lifecycle"

// readme is the one file of the lifecycle folder that is not an advisory.
const readme = "README.md"

// maxAdvisoryBytes bounds one advisory file.
const maxAdvisoryBytes = 1 << 20

// osvSchemaVersion is the OSV schema version the advisories follow.
const osvSchemaVersion = "1.6.0"

// The OSV ecosystems of the two kinds of package.
const (
	componentEcosystem = "STEGO"
	runtimeEcosystem   = "Go"
)

// The kinds of advisory, under database_specific.
const (
	supersessionKind = "supersession"
	withdrawalKind   = "withdrawal"
)

var advisoryID = regexp.MustCompile(`^STEGO-([0-9]{4})-([0-9]{4,})$`)

// Store is the repository of digest lifecycles (ST-DIS-REPO01): the lifecycle folder of one registry checkout.
// It reads every advisory in the folder, and it adds an advisory as a new file; it never changes or removes one.
type Store struct {
	dir string
}

// NewStore gives the store of the registry checkout at registryDir.
func NewStore(registryDir string) *Store {
	return &Store{dir: filepath.Join(registryDir, Folder)}
}

// Dir is the lifecycle folder.
func (s *Store) Dir() string {
	return s.dir
}

// ErrNoFolder tells that the registry holds no lifecycle folder, so no standing can be read from it.
var ErrNoFolder = errors.New("the registry holds no lifecycle folder")

// All reads every lifecycle the folder records. A missing folder, a file that is not a valid advisory, or two
// advisories that give one digest two supersessions or two withdrawals make the whole folder unreadable.
func (s *Store) All() ([]Lifecycle, error) {
	advisories, err := s.read()
	if err != nil {
		return nil, err
	}
	return lifecycles(advisories)
}

// Get reads the lifecycle of one subject; a subject with no advisory has an empty lifecycle. A missing folder
// holds no advisory.
func (s *Store) Get(subject Subject) (Lifecycle, error) {
	all, err := s.All()
	if errors.Is(err, ErrNoFolder) {
		return Lifecycle{Subject: subject}, nil
	}
	if err != nil {
		return Lifecycle{}, err
	}
	for _, l := range all {
		if l.Subject == subject {
			return l, nil
		}
	}
	return Lifecycle{Subject: subject}, nil
}

// read reads and checks every advisory file, in name order.
func (s *Store) read() ([]Advisory, error) {
	info, err := os.Lstat(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s does not exist", ErrNoFolder, s.dir)
	}
	if err != nil {
		return nil, fmt.Errorf("reading the lifecycle folder: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("the lifecycle folder %s is not a folder", s.dir)
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("reading the lifecycle folder: %w", err)
	}
	var advisories []Advisory
	for _, entry := range entries {
		name := entry.Name()
		if name == readme {
			continue
		}
		if !entry.Type().IsRegular() || !strings.HasSuffix(name, ".json") {
			return nil, fmt.Errorf("the lifecycle folder holds %s, which is not an advisory file", name)
		}
		advisory, err := readAdvisory(filepath.Join(s.dir, name))
		if err != nil {
			return nil, fmt.Errorf("the lifecycle record %s is not valid: %w", name, err)
		}
		if advisory.ID+".json" != name {
			return nil, fmt.Errorf("the lifecycle record %s holds the advisory %s; the file must be named after it", name, advisory.ID)
		}
		advisories = append(advisories, advisory)
	}
	return advisories, nil
}

func readAdvisory(path string) (Advisory, error) {
	file, err := os.Open(path)
	if err != nil {
		return Advisory{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxAdvisoryBytes+1))
	if err != nil {
		return Advisory{}, err
	}
	if len(data) > maxAdvisoryBytes {
		return Advisory{}, fmt.Errorf("it is larger than %d bytes", maxAdvisoryBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var advisory Advisory
	if err := decoder.Decode(&advisory); err != nil {
		return Advisory{}, err
	}
	if decoder.More() {
		return Advisory{}, fmt.Errorf("it holds more than one JSON value")
	}
	if _, err := advisory.entry(); err != nil {
		return Advisory{}, err
	}
	return advisory, nil
}

// lifecycles groups the advisories by subject. A retracted supersession, one whose OSV withdrawn timestamp is
// set, no longer counts.
func lifecycles(advisories []Advisory) ([]Lifecycle, error) {
	bySubject := map[Subject]*Lifecycle{}
	var order []Subject
	ids := map[string]bool{}
	for _, advisory := range advisories {
		if ids[advisory.ID] {
			return nil, fmt.Errorf("the advisory id %s is not unique", advisory.ID)
		}
		ids[advisory.ID] = true
		entry, _ := advisory.entry()
		current, held := bySubject[entry.subject]
		if !held {
			current = &Lifecycle{Subject: entry.subject}
			bySubject[entry.subject] = current
			order = append(order, entry.subject)
		}
		switch {
		case entry.supersession != nil && advisory.Withdrawn != "":
			continue
		case entry.supersession != nil:
			if current.Supersession != nil {
				return nil, fmt.Errorf("%s has two successors, in %s and %s", entry.subject, current.Supersession.AdvisoryID, advisory.ID)
			}
			current.Supersession = entry.supersession
		default:
			if current.Withdrawal != nil {
				return nil, fmt.Errorf("%s has two withdrawals, in %s and %s", entry.subject, current.Withdrawal.AdvisoryID, advisory.ID)
			}
			current.Withdrawal = entry.withdrawal
		}
	}
	result := make([]Lifecycle, 0, len(order))
	for _, subject := range order {
		result = append(result, *bySubject[subject])
	}
	return result, nil
}

// nextAdvisoryID is one past the highest advisory number of the year the record is made in.
func (s *Store) nextAdvisoryID(recordedOn time.Time) (string, error) {
	advisories, err := s.read()
	if err != nil && !errors.Is(err, ErrNoFolder) {
		return "", err
	}
	year := recordedOn.UTC().Year()
	highest := 0
	for _, advisory := range advisories {
		match := advisoryID.FindStringSubmatch(advisory.ID)
		if match == nil || match[1] != strconv.Itoa(year) {
			continue
		}
		if number, err := strconv.Atoi(match[2]); err == nil && number > highest {
			highest = number
		}
	}
	return fmt.Sprintf("STEGO-%04d-%04d", year, highest+1), nil
}

// add writes one new advisory file. It never replaces a file: the name is taken by a hard link, which fails when
// the name exists.
func (s *Store) add(advisory Advisory) error {
	data, err := json.MarshalIndent(advisory, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("creating the lifecycle folder: %w", err)
	}
	temporary, err := os.CreateTemp(s.dir, ".advisory-")
	if err != nil {
		return fmt.Errorf("writing the advisory %s: %w", advisory.ID, err)
	}
	defer os.Remove(temporary.Name())
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("writing the advisory %s: %w", advisory.ID, err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("writing the advisory %s: %w", advisory.ID, err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("writing the advisory %s: %w", advisory.ID, err)
	}
	if err := os.Chmod(temporary.Name(), 0o644); err != nil {
		return err
	}
	if err := os.Link(temporary.Name(), filepath.Join(s.dir, advisory.ID+".json")); err != nil {
		return fmt.Errorf("writing the advisory %s: %w", advisory.ID, err)
	}
	return nil
}

// Advisory is one lifecycle record as it is stored (ST-DIS-EN06-TB): an OSV entry in the lifecycle folder, named
// <id>.json. STEGO's own fields sit under database_specific.stego.
type Advisory struct {
	SchemaVersion    string           `json:"schema_version"`
	ID               string           `json:"id"`
	Modified         string           `json:"modified"`
	Published        string           `json:"published"`
	Withdrawn        string           `json:"withdrawn,omitempty"`
	Summary          string           `json:"summary"`
	Affected         []osvAffected    `json:"affected"`
	References       []osvReference   `json:"references,omitempty"`
	DatabaseSpecific databaseSpecific `json:"database_specific"`
}

type osvAffected struct {
	Package  osvPackage `json:"package"`
	Ranges   []osvRange `json:"ranges,omitempty"`
	Versions []string   `json:"versions,omitempty"`
}

type osvPackage struct {
	Ecosystem string `json:"ecosystem"`
	Name      string `json:"name"`
}

type osvRange struct {
	Type   string     `json:"type"`
	Events []osvEvent `json:"events"`
}

type osvEvent struct {
	Introduced string `json:"introduced,omitempty"`
	Fixed      string `json:"fixed,omitempty"`
}

type osvReference struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

type databaseSpecific struct {
	Stego stegoFields `json:"stego"`
}

// stegoFields are STEGO's own fields of an advisory.
type stegoFields struct {
	Kind                   string `json:"kind"`
	SuccessorName          string `json:"successor_name,omitempty"`
	FailsAfter             string `json:"fails_after"`
	NeedsDeclarationChange *bool  `json:"needs_declaration_change,omitempty"`
	NeedsFillChange        *bool  `json:"needs_fill_change,omitempty"`
}

// advisoryEntry is what one advisory states about one subject.
type advisoryEntry struct {
	subject      Subject
	supersession *Supersession
	withdrawal   *Withdrawal
}

const timeLayout = time.RFC3339

// entry checks the advisory and reads what it states.
func (a Advisory) entry() (advisoryEntry, error) {
	if a.SchemaVersion != osvSchemaVersion {
		return advisoryEntry{}, fmt.Errorf("its schema_version is %q, not %q", a.SchemaVersion, osvSchemaVersion)
	}
	if !advisoryID.MatchString(a.ID) {
		return advisoryEntry{}, fmt.Errorf("its id %q is not an advisory id such as STEGO-2026-0001", a.ID)
	}
	if strings.TrimSpace(a.Summary) == "" {
		return advisoryEntry{}, fmt.Errorf("it has no summary")
	}
	published, err := time.Parse(timeLayout, a.Published)
	if err != nil {
		return advisoryEntry{}, fmt.Errorf("its published time %q is not an RFC 3339 time", a.Published)
	}
	if _, err := time.Parse(timeLayout, a.Modified); err != nil {
		return advisoryEntry{}, fmt.Errorf("its modified time %q is not an RFC 3339 time", a.Modified)
	}
	if len(a.Affected) != 1 {
		return advisoryEntry{}, fmt.Errorf("it names %d affected packages, not one", len(a.Affected))
	}
	affected := a.Affected[0]
	subject := Subject{Name: affected.Package.Name}
	switch affected.Package.Ecosystem {
	case componentEcosystem:
		subject.Kind = ComponentPackage
	case runtimeEcosystem:
		subject.Kind = RuntimePackage
	default:
		return advisoryEntry{}, fmt.Errorf("its ecosystem %q is neither %s nor %s", affected.Package.Ecosystem, componentEcosystem, runtimeEcosystem)
	}
	fields := a.DatabaseSpecific.Stego
	failsAfter, err := ParseDate(fields.FailsAfter)
	if err != nil {
		return advisoryEntry{}, fmt.Errorf("its fail date: %w", err)
	}
	var references []string
	for _, reference := range a.References {
		references = append(references, reference.URL)
	}
	switch fields.Kind {
	case supersessionKind:
		if len(affected.Versions) != 0 || len(affected.Ranges) != 1 || affected.Ranges[0].Type != "ECOSYSTEM" || len(affected.Ranges[0].Events) != 2 ||
			affected.Ranges[0].Events[0].Introduced == "" || affected.Ranges[0].Events[0].Fixed != "" ||
			affected.Ranges[0].Events[1].Fixed == "" || affected.Ranges[0].Events[1].Introduced != "" {
			return advisoryEntry{}, fmt.Errorf("a supersession names the superseded digest as introduced and the successor as fixed, in one range")
		}
		if fields.NeedsDeclarationChange == nil || fields.NeedsFillChange == nil {
			return advisoryEntry{}, fmt.Errorf("a supersession states whether taking the successor needs a declaration change and a fill change")
		}
		subject.Digest = affected.Ranges[0].Events[0].Introduced
		successor := Subject{Kind: subject.Kind, Name: subject.Name, Digest: affected.Ranges[0].Events[1].Fixed}
		if fields.SuccessorName != "" {
			successor.Name = fields.SuccessorName
		}
		if err := subject.check("the superseded digest"); err != nil {
			return advisoryEntry{}, err
		}
		if err := successor.check("the successor"); err != nil {
			return advisoryEntry{}, err
		}
		if a.Withdrawn != "" {
			if _, err := time.Parse(timeLayout, a.Withdrawn); err != nil {
				return advisoryEntry{}, fmt.Errorf("its withdrawn time %q is not an RFC 3339 time", a.Withdrawn)
			}
		}
		return advisoryEntry{subject: subject, supersession: &Supersession{
			Successor: successor, FailsAfter: failsAfter,
			NeedsDeclarationChange: *fields.NeedsDeclarationChange, NeedsFillChange: *fields.NeedsFillChange,
			RecordedOn: published, AdvisoryID: a.ID, Summary: a.Summary, References: references,
		}}, nil
	case withdrawalKind:
		if len(affected.Ranges) != 0 || len(affected.Versions) != 1 {
			return advisoryEntry{}, fmt.Errorf("a withdrawal names the withdrawn digest as its one affected version, with no range")
		}
		if fields.SuccessorName != "" || fields.NeedsDeclarationChange != nil || fields.NeedsFillChange != nil {
			return advisoryEntry{}, fmt.Errorf("a withdrawal has no successor")
		}
		withdrawn, err := time.Parse(timeLayout, a.Withdrawn)
		if err != nil {
			return advisoryEntry{}, fmt.Errorf("a withdrawal carries the withdrawn time; %q is not an RFC 3339 time", a.Withdrawn)
		}
		subject.Digest = affected.Versions[0]
		if err := subject.check("the withdrawn digest"); err != nil {
			return advisoryEntry{}, err
		}
		return advisoryEntry{subject: subject, withdrawal: &Withdrawal{
			FailsAfter: failsAfter, Reason: a.Summary, RecordedOn: withdrawn, AdvisoryID: a.ID, References: references,
		}}, nil
	}
	return advisoryEntry{}, fmt.Errorf("its kind %q is neither %s nor %s", fields.Kind, supersessionKind, withdrawalKind)
}

func ecosystem(kind string) string {
	if kind == RuntimePackage {
		return runtimeEcosystem
	}
	return componentEcosystem
}

func references(urls []string) []osvReference {
	var result []osvReference
	for _, url := range urls {
		result = append(result, osvReference{Type: "WEB", URL: url})
	}
	return result
}

// supersessionAdvisory writes a supersession as an OSV advisory.
func supersessionAdvisory(subject Subject, s Supersession) Advisory {
	recorded := s.RecordedOn.UTC().Format(timeLayout)
	declaration, fill := s.NeedsDeclarationChange, s.NeedsFillChange
	fields := stegoFields{Kind: supersessionKind, FailsAfter: FormatDate(s.FailsAfter), NeedsDeclarationChange: &declaration, NeedsFillChange: &fill}
	if s.Successor.Name != subject.Name {
		fields.SuccessorName = s.Successor.Name
	}
	return Advisory{
		SchemaVersion: osvSchemaVersion, ID: s.AdvisoryID, Modified: recorded, Published: recorded, Summary: s.Summary,
		Affected: []osvAffected{{
			Package: osvPackage{Ecosystem: ecosystem(subject.Kind), Name: subject.Name},
			Ranges:  []osvRange{{Type: "ECOSYSTEM", Events: []osvEvent{{Introduced: subject.Digest}, {Fixed: s.Successor.Digest}}}},
		}},
		References:       references(s.References),
		DatabaseSpecific: databaseSpecific{Stego: fields},
	}
}

// withdrawalAdvisory writes a withdrawal as an OSV advisory; its withdrawn time is the time it was recorded.
func withdrawalAdvisory(subject Subject, w Withdrawal) Advisory {
	recorded := w.RecordedOn.UTC().Format(timeLayout)
	return Advisory{
		SchemaVersion: osvSchemaVersion, ID: w.AdvisoryID, Modified: recorded, Published: recorded, Withdrawn: recorded, Summary: w.Reason,
		Affected: []osvAffected{{
			Package:  osvPackage{Ecosystem: ecosystem(subject.Kind), Name: subject.Name},
			Versions: []string{subject.Digest},
		}},
		References:       references(w.References),
		DatabaseSpecific: databaseSpecific{Stego: stegoFields{Kind: withdrawalKind, FailsAfter: FormatDate(w.FailsAfter)}},
	}
}

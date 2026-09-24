package gen

// DatabaseObject declares the access needed by one generated component.
// Names identify existing objects. This declaration does not authorize DDL.
// An empty sequence privilege set requires the absence of direct access.
type DatabaseObject struct {
	Schema     string   `json:"schema"`
	Name       string   `json:"name"`
	Kind       string   `json:"kind"`
	Privileges []string `json:"privileges"`
}

const DatabaseAccessNamespace = "contracts/databaseaccess"

// BackupObject names one database object that a backup must include so the
// restored database stays usable. Kinds follow DatabaseObject with two
// additions: "function" and "trigger" cover the generated guard objects
// that live inside the same schemas.
type BackupObject struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
}

const BackupContractNamespace = "contracts/backup"

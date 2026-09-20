package gen

// DatabaseObject declares the access needed by one generated component.
// Names identify existing objects. This declaration does not authorize DDL.
type DatabaseObject struct {
	Schema     string   `json:"schema"`
	Name       string   `json:"name"`
	Kind       string   `json:"kind"`
	Privileges []string `json:"privileges"`
}

const DatabaseAccessNamespace = "contracts/databaseaccess"

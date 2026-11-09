// Package staging holds the key of the authorization runtime's constructor of a staging principal
// (ST-SUP-RP06-LC19). Only NewStaging makes a key that is not zero. Only the command runtime's RunConsumer may call
// NewStaging: the import check of the runtime module (TestStagingImporters in the authz package) refuses every other
// package that calls it, and every importer other than the command runtime and the authorization runtime, which
// names the type and never calls NewStaging. The zero Staging opens nothing.
package staging

// stagingData is the data of a Staging key. Only NewStaging makes one.
type stagingData struct {
	made bool
}

// Staging is the key that authz.StagingPrincipal takes.
type Staging struct {
	data *stagingData
}

// NewStaging makes the staging key, for the command runtime's RunConsumer only.
func NewStaging() Staging { return Staging{data: &stagingData{made: true}} }

// Valid reports whether key is one that NewStaging made.
func Valid(key Staging) bool { return key.data != nil && key.data.made }

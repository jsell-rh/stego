package types

// FillModule is one module that the declaration lets the service's fills
// import, besides their slot contracts and the Go standard library
// (ST-DECL-EN08), with why the fills need it, so a reviewer can judge it.
type FillModule struct {
	Module string `yaml:"module"`
	Reason string `yaml:"reason"`
}

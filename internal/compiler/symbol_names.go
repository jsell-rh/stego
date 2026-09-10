package compiler

import "go/types"

// Imports must not hide language names or declarations in generated startup.
// Keep the startup names covered by the generated-program test when templates
// add declarations. Each assembly receives its own mutable allocation state.
func reservedImportNames() map[string]bool {
	names := make(map[string]bool)
	for _, name := range types.Universe.Names() {
		names[name] = true
	}
	for _, name := range []string{"init", "main", "run", "stegoHTTPServer", "stegoHTTPServerWithErrorLog", "stegoHTTPDiagnostics", "stegoNewHTTPErrorLog", "stegoCloseHTTPDiagnostics", "stegoServeHTTP", "stegoHTTPError", "stegoTask", "stegoRunTasks", "stegoServiceFailure", "stegoReportFailure", "stegoTaskFailure", "stegoTaskNames"} {
		names[name] = true
	}
	return names
}

func predeclaredName(name string) bool {
	return types.Universe.Lookup(name) != nil
}

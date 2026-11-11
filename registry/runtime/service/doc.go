// Package service is the service constructor of the runtime library (ST-SUP-RP14): Settings, which the process
// environment gives by fixed names; New, which wires the runtimes of one service from its settings and its declared
// parts; and Migrate, which runs the migration job of the service in one fixed order. Only the package paths, the
// names and the grants vary with the declaration, and generated code passes them to this package
// (ST-STD-RUNTIME-R05).
//
// It is a library, not a framework (ST-STD-RUNTIME-R01): generated code calls its functions and types, and the
// package calls back into no generated code and owns no main loop.
package service

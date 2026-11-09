// Package query is the query handle of the runtime library (ST-SUP-RP11): the handle for slots bound to one read-only
// store transaction, tenant and principal, and Run, which calls a query fill and ends the transaction.
//
// It is a library, not a framework (ST-STD-RUNTIME-R01): generated code calls its functions and types, and the
// package calls back into no generated code and owns no main loop.
package query

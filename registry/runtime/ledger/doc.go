// Package ledger is the ledger of the runtime library (ST-SUP-RP08): the balance derivation, the normal-balance
// arithmetic, the two-phase state machine and its errors, the idempotent replay of a transfer, the reversal rules,
// the snapshot and ledger_check jobs and the text of the guard trigger.
//
// It is a library, not a framework (ST-STD-RUNTIME-R01): generated code calls its functions and types, and the
// package calls back into no generated code and owns no main loop.
package ledger

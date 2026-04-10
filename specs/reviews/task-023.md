# Review: Task 023 — RFC 9457 Problem Details Error Handling

## Round 1

- [ ] **Error code prefix derivation produces wrong result.** `deriveErrorPrefix` (`generator.go:801-804`) removes hyphens and uppercases, producing `HYPERFLEETAPI` from `hyperfleet-api`. The spec (rest-crud spec §Error Handling, spec.md §Error Handling) and task description both state `hyperfleet-api` → `HYPERFLEET`. The function's own doc comment says `"hyperfleet-api" → "HYPERFLEET"` but the implementation contradicts it. Tests (`generator_test.go:4706`) assert the wrong value (`HYPERFLEETAPI`). All generated error codes (NTF, VAL, AUT, AUZ, CNF, INT) carry the incorrect prefix.

- [ ] **`handleError` does not populate `trace_id` from OpenTelemetry span context.** The task description states: "Populates `trace_id` from OpenTelemetry span context when available." The spec's example JSON includes `"trace_id": "4bf92f3577b34da6a3ce929d0e0e4736"`. The `TraceID` field exists on `ServiceError` (`generator.go:830`) but `handleError` (`generator.go:946-952`) never sets it. The generated function populates `Instance` and `Timestamp` but skips `TraceID`.

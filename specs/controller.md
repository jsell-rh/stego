
`RunKeyedWatches` accepts 1..16 sources for one keyed controller. All watches
open before scans or actions start. Receivers share the bounded key queue and
retain at most one pending key each. A source failure cancels and joins every
receiver before reconnecting. Scans run in source order and share the scan
limit. Prefix keys with their resource kind when IDs can occur in more than one
source. The existing `RunKeyedWatch` API uses the same runtime with one source.

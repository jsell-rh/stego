# Review: Task 017 — Git-Based Registry Resolution

## Round 1

- [ ] **`stego init` silently uses `STEGO_REGISTRY` without printing the required warning.** AC #4 states: "A warning must be printed to stderr whenever `STEGO_REGISTRY` is used." The `initRegistryDir` function (`cmd/stego/main.go:211-216`) reads `STEGO_REGISTRY` and returns the value without printing any warning to stderr. All other code paths that use `STEGO_REGISTRY` go through `ResolveRegistry` which does print the warning, but `stego init` bypasses `ResolveRegistry` entirely and uses this silent helper instead.

- [ ] **Git clone error message omits the ref.** AC #8 requires: "the error message identifies the URL, the ref, and the underlying git error." The clone failure error at `internal/registry/resolve.go:143` formats as `"git clone %s failed: %w\n%s"` with only the URL and git error. The ref is not included. Compare with the checkout error on line 151 which correctly includes both URL and ref.

- [ ] **No-git-binary error message omits the URL and ref.** AC #8 again. The error at `internal/registry/resolve.go:129` formats as `"git binary not found: %w (required for git-based registry resolution)"`. The URL and ref that triggered the resolution are not surfaced. Since `resolveGitRegistry` receives both `url` and `ref` as parameters, they should be included so the user knows which registry entry failed.

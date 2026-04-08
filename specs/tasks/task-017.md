# Task 017: Git-Based Registry Resolution

**Spec Reference:** "Registry", "Compiler Process (Reconciler Pattern)"

**Status:** `ready-for-review`

## Description

The spec defines the registry as a git repo resolved via `.stego/config.yaml`, but the current implementation bypasses config.yaml entirely and loads the registry from a hardcoded local directory path (`STEGO_REGISTRY` env var or `./registry`). The `url` and `ref` fields in config.yaml are parsed and validated but never used for actual resolution. This task replaces the local-directory-only approach with proper git-based registry resolution as described in the spec.

### What changes

**Registry resolution via config.yaml:**
- `resolveRegistryDir` is replaced by a `ResolveRegistry` function that reads `.stego/config.yaml` and resolves the registry source.
- For git URLs (any `url` value that is not an existing local directory path), clone the repo (or fetch into a local cache) and checkout the exact SHA specified by `ref`. The cache location is `~/.cache/stego/registries/<url-hash>/<ref>/`.
- For local filesystem paths (the `url` value is an existing local directory), use the path directly. The `ref` field is still required by config validation but is recorded in state only — no git operations are performed. This supports testing and development workflows.

**`STEGO_REGISTRY` env var becomes a development override:**
- When `STEGO_REGISTRY` is set, it overrides config.yaml resolution entirely and uses the env var value as a local directory path.
- **A warning must be printed to stderr** whenever `STEGO_REGISTRY` is used: `WARNING: using STEGO_REGISTRY override: /path/to/registry (config.yaml registry settings ignored)`
- This ensures the user always knows when they are not using the config.yaml-defined registry.

**Per-component SHA pins (parsed, not enforced):**
- The `pins:` field in config.yaml is already parsed. This task does NOT implement per-component SHA resolution (deferred to post-MVP per spec). The pins field continues to be parsed and stored in state but has no effect on loading.

**`stego init` writes config.yaml with a local path:**
- When `stego init` creates `.stego/config.yaml`, it writes a config pointing to the local registry path used during init (either `STEGO_REGISTRY` or `./registry`), with `ref: local` as a placeholder. This makes the project immediately functional without requiring a git remote.

### What does NOT change

- `registry.Load(dir string)` continues to accept a local directory path. The git resolution layer sits above it — it resolves the config to a local directory, then calls `Load()`.
- State tracking (`state.yaml`) continues to record `registry_sha` from config.yaml's `ref` field.
- No multi-registry support (deferred to post-MVP). Only `cfg.Registry[0]` is used; additional entries produce a warning.

## Spec Excerpt

> The registry is a git repo. No database, no server. Versions are git tags for discovery, but all resolution pins to SHAs for auditability.
>
> ```yaml
> # .stego/config.yaml
> registry:
>   - url: git.corp.com/platform/stego-registry
>     ref: a1b2c3d4e5f6  # pinned SHA, not a branch or tag
> ```
>
> Resolution order: pinned SHA > registry ref.

## Acceptance Criteria

1. **Config-driven resolution:** When `STEGO_REGISTRY` is not set, the registry is resolved from `.stego/config.yaml`'s first `registry` entry. If config.yaml is missing or has no registry entries, commands that need the registry return a clear error.
2. **Git clone at pinned SHA:** When `url` is a git remote (not a local directory), the registry is cloned/fetched into a cache directory and checked out at the exact `ref` SHA. Subsequent runs with the same `url`+`ref` reuse the cached checkout without re-cloning.
3. **Local path support:** When `url` is an existing local directory path, it is used directly as the registry directory. No git operations are performed. This is the primary testing/development workflow.
4. **Env var override with warning:** When `STEGO_REGISTRY` is set, a warning is printed to stderr: `WARNING: using STEGO_REGISTRY override: <path> (config.yaml registry settings ignored)`. The env var value is used as the registry directory.
5. **`stego init` writes functional config:** `stego init` creates `.stego/config.yaml` with `url` set to the local registry path used during init and `ref: local`.
6. **All existing CLI commands continue to work:** `plan`, `apply`, `validate`, `drift`, `fill create`, `registry search/inspect/fills` all use the new resolution path.
7. **Cache isolation:** Different `url`+`ref` combinations produce separate cache directories. A cache directory is reused if it exists and contains the correct checkout.
8. **Git errors are clear:** If `git clone` or `git checkout` fails (bad URL, bad SHA, no git binary), the error message identifies the URL, the ref, and the underlying git error.
9. **Tests:** Unit tests for resolution logic (local path, env var override with warning capture, cache hit/miss). Integration test with a real local git repo: init a bare repo, commit a registry tree, configure config.yaml with the repo path and commit SHA, and verify `Load` succeeds from the cached checkout.

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits

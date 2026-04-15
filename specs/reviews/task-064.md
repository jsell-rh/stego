# Review: Task 064 — Use UUID v7 Instead of v4 for Entity ID Generation

## Findings

- [-] [process-revision-complete] **Unrelated deletion of `.stego/config.yaml` from both examples.** Commit `b104d67` deletes `examples/user-management/.stego/config.yaml` and `examples/user-management-rhsso/.stego/config.yaml`. These files contain the registry configuration (`registry: - url: git.corp.com/platform/stego-registry, ref: a1b2c3d4e5f6`). The spec defines `config.yaml` as part of the `.stego/` project structure (spec.md line 176), separate from the generated `out/` directory. AC #8 calls for "Example output regenerated" — regeneration should update generated files and `state.yaml` hashes, not delete project configuration. The files must be restored.

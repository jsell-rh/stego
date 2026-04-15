#!/usr/bin/env bash
# Pre-commit check: prevent deletion of .stego/config.yaml project configuration files.
# These are authored artifacts, not generated output. Deleting them breaks example projects.
set -euo pipefail

deleted_configs=$(git diff --cached --diff-filter=D --name-only | grep '\.stego/config\.yaml$' || true)

if [[ -n "$deleted_configs" ]]; then
    echo "ERROR: Commit deletes .stego/config.yaml project configuration file(s):"
    echo "$deleted_configs" | sed 's/^/  /'
    echo ""
    echo ".stego/config.yaml is a project configuration file (registry URL, ref),"
    echo "not generated output. Regenerating examples should only modify:"
    echo "  - .stego/state.yaml  (state tracking)"
    echo "  - out/**             (generated output)"
    echo ""
    echo "Unstage the deletion with:  git restore --staged <path>"
    exit 1
fi

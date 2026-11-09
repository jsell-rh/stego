#!/usr/bin/env bash
# runtime-workspace.sh writes a Go workspace file that resolves stego.dev/runtime from this checkout's
# registry/runtime folder, for local development and tests (ST-SUP-TOOL12).
#
# Usage: scripts/runtime-workspace.sh <module folder> <work file>
#
# The work file uses the module folder (such as a generated service) and registry/runtime. Give its path as GOWORK
# to a go command in the module folder: the go command then reads the runtime packages from the folder, with no
# network and no fetch from stego.dev. A generated go.mod keeps its runtime pin as it is, with no replace directive
# (ST-GEN-INV13, ST-VO58-C01). Put the work file outside the module folder, so that no committed tree holds it, and
# do not use it for stego deps or stego build: they resolve from the pin.
set -euo pipefail

if [ "$#" -ne 2 ]; then
	echo "usage: $0 <module folder> <work file>" >&2
	exit 2
fi
case "$2" in
*.work) ;;
*)
	echo "runtime-workspace: the work file name must end in .work: $2" >&2
	exit 2
	;;
esac

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
runtime="$root/registry/runtime"
module="$(cd "$1" && pwd -P)"
if [ ! -f "$module/go.mod" ]; then
	echo "runtime-workspace: no go.mod in $module" >&2
	exit 1
fi
version="$(sed -n 's/^go \([0-9][0-9.]*\)$/\1/p' "$runtime/go.mod")"
if [ -z "$version" ]; then
	echo "runtime-workspace: no go version in $runtime/go.mod" >&2
	exit 1
fi

mkdir -p "$(dirname "$2")"
if [ "$module" = "$runtime" ]; then
	# The module folder is the runtime module itself: a work file that uses one folder twice is refused.
	printf 'go %s\n\nuse %s\n' "$version" "$runtime" >"$2"
	echo "$2"
	exit 0
fi
printf 'go %s\n\nuse (\n\t%s\n\t%s\n)\n' "$version" "$module" "$runtime" >"$2"
echo "$2"

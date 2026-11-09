#!/usr/bin/env bash
# runtime-workspace.sh writes a Go workspace file that resolves stego.dev/runtime from this checkout's
# registry/runtime folder, for local development and tests (ST-SUP-TOOL12).
#
# Usage: scripts/runtime-workspace.sh <module folder> <work file>
#
# The work file uses the module folder (such as a generated service) and registry/runtime. Give its path as GOWORK
# to a go command in the module folder: the go command then reads the runtime packages from the folder, with no
# network and no fetch from stego.dev. A generated go.mod keeps its runtime pin as it is, with no replace directive
# in it (ST-GEN-INV13, ST-VO58-C01): the only replace is the one below, in the work file. Put the work file outside
# the module folder, so that no committed tree holds it, and do not use it for stego deps or stego build: they
# resolve from the pin.
#
# When the module folder requires stego.dev/runtime at a version, the work file also replaces that one version with
# the folder. Without it, the go command fetches the go.mod of the pinned version to load the module graph as soon as
# the runtime module has a requirement of its own, and the build fails with no network.
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

pin="$(sed -n -E 's#^[[:space:]]*(require[[:space:]]+)?stego\.dev/runtime[[:space:]]+(v[^[:space:]]+).*$#\2#p' "$module/go.mod" | head -n 1)"

mkdir -p "$(dirname "$2")"
# A work file inside the module folder could be committed, and one named go.work above it is found by every go
# command run in the folder, also by stego deps and stego build. Refuse both.
work="$(cd "$(dirname "$2")" && pwd -P)/$(basename "$2")"
case "$work/" in
"$module"/*)
	echo "runtime-workspace: the work file must be outside the module folder: $2" >&2
	exit 2
	;;
esac
if [ "$(basename "$work")" = "go.work" ]; then
	case "$module/" in
	"$(dirname "$work")"/*)
		echo "runtime-workspace: a go.work above the module folder is used by every go command in it: $2" >&2
		exit 2
		;;
	esac
fi
if [ "$module" = "$runtime" ]; then
	# The module folder is the runtime module itself: a work file that uses one folder twice is refused.
	printf 'go %s\n\nuse %s\n' "$version" "$runtime" >"$2"
	echo "$2"
	exit 0
fi
{
	printf 'go %s\n\nuse (\n\t%s\n\t%s\n)\n' "$version" "$module" "$runtime"
	if [ -n "$pin" ]; then
		printf '\nreplace stego.dev/runtime %s => %s\n' "$pin" "$runtime"
	fi
} >"$2"
echo "$2"

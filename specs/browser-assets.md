# Browser asset contract

The browser backend can use a captured ZIP from a frontend build. This keeps
build tools and domain pages in the application. STEGO owns capture limits,
archive checks, generated asset serving, and the script policy.

Use `stego assets --directory build/client --output ui/assets.zip` to capture
a build. Declare `asset_bundle: ui/assets.zip` in the browser component config.
Do not also declare `assets`. Both forms require `index.html`. Other files must
be below `assets/` and have a supported extension. Generation reads only the
captured archive. It does not scan the build directory or fetch files.

The capture command sorts entries and writes fixed archive metadata. The
command validates all input before it replaces the output file. Repeated
capture with the same input and toolchain produces the same archive. Limits
are 128 files, 4 MiB per expanded file, 16 MiB in total, and 4 MiB for the ZIP.
The directory walk also has a node limit. Paths, file types, archive order,
entry sizes, and duplicate names are checked. Symbolic links are rejected.

STEGO calculates SHA-256 hashes for inline scripts in the captured HTML. The
server adds these hashes only to responses that serve `index.html`. It does
not permit `unsafe-inline`, `unsafe-eval`, or a fixed nonce. The existing
asset types, ETags, route checks, and response limits still apply.

HTML and JavaScript are application code. The compiler does not make unsafe
application code safe. HTML checks detect unsupported forms; they are not a
sanitizer. They use HTML attribute rules, including the first value when an
attribute name occurs twice. Foreign-content elements and inline event or
style attributes are rejected. Applications must test browser rendering,
accessibility, dependency security, and their domain workflow separately.

The first Hypershell build had 363 files and used about 13 MiB on disk. A
change to import only Bash and two syntax themes reduced it to 54 files and
about 3 MiB on disk. The capture command accepted the resulting 835,373-byte
archive without a limit change. The compiler contains no Hypershell names,
paths, themes, or domain rules.

The capture, archive rejection, CLI replacement, and generator tests passed
with the race detector in the jshell cluster. The generated backend runtime also passed with PostgreSQL. Its package
completed in 93.404 seconds. This includes a check that script hashes appear
only on HTML responses and that broad script permissions remain disabled.
The rendered application browser checks remain open.

## Typed compiler input limits

Browser backend 1.7.0 declares a 4 MiB read limit for each asset or captured ZIP.
The compiler uses a typed input record with a path and maximum byte count.
Protocol and callback generators still declare 1 MiB. Limits must be positive
and at most 4 MiB. Invalid limits, paths, and repeated paths fail before input
files are opened. The combined 8 MiB input limit and snapshot checks still apply.
The ZIP also retains its 128-file, 4 MiB expanded-file, and 16 MiB expanded-total
limits. Service YAML cannot change these limits.

The real upstream dashboard exposed the earlier mismatch in
[run 35109019035](https://github.com/jsell-rh/hypershell-stego/actions/runs/35109019035).
Its updated Go and JavaScript dependency checks passed. The UI built and all
eight selected router tests passed. Its split assets then exceeded the former
1 MiB ZIP limit. The source check did not pass. This change must pass full
compiler CI and that real asset check before qualification.

Small checks cover a bundle above 1 MiB, stable encoding and decoding, captured
size rejection, unchanged protocol limits, invalid declarations, the combined
input limit, and a changed large source before apply. These checks do not prove
the complete rendered dashboard workflow.

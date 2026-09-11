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
are 128 files, 4 MiB per expanded file, 16 MiB in total, and 1 MiB for the ZIP.
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

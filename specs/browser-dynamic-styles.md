# Dynamic browser styles

Status: candidate. Browser and compiler checks must pass before adoption.
No Hypershell image or compiler pin selects this candidate yet.

Some UI libraries create stylesheets and insert layout styles through HTML
strings. These are different operations. A stylesheet nonce does not authorize
style attributes. STEGO keeps raw style attributes blocked.

Set `dynamic_styles: true` on `browser-backend` to generate the common DOM
package at `<browser namespace>/dom`. This option requires captured HTML with
one explicit head element. The compiler rejects application-supplied nonce
metadata. The default remains disabled.

For each HTML response, the generated backend creates a new random 256-bit
nonce. It inserts `stego-style-nonce` metadata and the matching style policy.
It retains `no-store`, computes the ETag from the decorated response, and keeps
script policy separate. Assets do not receive the document nonce. Random-source
failure or an invalid insertion offset prevents document delivery.

`createStyleElement(document)` creates a stylesheet element with that nonce.
The caller owns its content, insertion, updates, and removal. The helper rejects
missing, malformed, or repeated nonce metadata. It does not modify native DOM
methods or authorize style elements inserted through arbitrary HTML strings.

`trustedFragment`, `replaceTrustedHTML`, and `appendTrustedHTML` accept trusted
application render output. They are not HTML or CSS sanitizers. Never give them
user-supplied HTML or CSS. Their integration points must be reviewed library
render functions, not generic DOM interception.

The helper scans tag and attribute boundaries and temporarily renames style
attributes before native parsing. Native parsing owns entities, namespaces, and
tree construction. The helper removes the private attribute, applies its value
through DOM style properties, and inserts the completed fragment. It adds no
stylesheet nodes or random class names. Repeated updates do not retain a global
style cache.

The accepted render grammar requires quoted attribute values. It permits boolean
attributes and canonical comments. It rejects repeated attributes, ambiguous
comments, raw-text and active elements, event attributes, and reserved metadata.
Input is limited to 1,048,576 UTF-16 code units and 16,384 parsed nodes. Validation
finishes before replacement or append changes the live target. Native
`TrustedHTML` values can be read, but this feature does not yet support enforced
Trusted Types at the rewritten parsing boundary.

## Evidence and remaining checks

The first [CI browser check](https://github.com/jsell-rh/stego/actions/runs/35164337463)
proved that stylesheet nonces work and raw inline styles remain blocked. Its
initial fragment implementation failed: Chromium reported style violations
during template parsing, before insertion. The revised helper moves style
attributes before parsing. The [second browser check](https://github.com/jsell-rh/stego/actions/runs/35164494930)
at `273c5cf` passed all six groups on Chrome 152.0.7977.82. This is a common
runtime result, not a deployed editor result.

The CI fixture checks computed HTML and SVG styles, incremental append and
replacement, element counts, input limits, quoted values, comments, nonce
validation, and the rejection of raw styles. It runs one browser for at most
45 seconds on a CI runner. The script refuses ordinary workstation execution.
This is a correctness check, not a performance result.

Generated backend tests must also verify nonce freshness, header and metadata
agreement, HEAD, conditional requests, default behavior, and asset responses.
The real dashboard must still prove editor geometry, input, selection, workers,
and content-policy compliance. This candidate alone does not close that gate.

The [CSP specification](https://www.w3.org/TR/CSP3/#style-src-attr) defines the
separate style-attribute control. An [upstream editor proposal](https://github.com/microsoft/vscode/pull/288813)
uses generated style nodes, but its review reports rendering, clipboard, and
cost defects. STEGO does not copy that implementation.

## Reviewed Monaco adapter

The generated DOM package includes `monaco-loader.cjs` and its source-hash
manifest. This webpack loader supports Monaco 0.52.2. It changes 15 reviewed
operations in 11 exact ESM files to call the common DOM helpers or direct DOM
style properties. It verifies each complete source file before it changes any
operation. Changed source bytes or a repeated transformation cause an error.
It retains the dependency's license text.

The adapter covers line rendering, wrapping measurements, syntax coloring,
inline completion text, sticky lines, diff rendering, and stylesheet creation.
It changes only tokenized code blocks in the editor's Markdown renderer. It does
not change the generic Markdown sanitizer or DOMPurify. Those are untrusted
content boundaries and must not use a trusted-render helper.

The application build selects this generated loader for Monaco ESM files and
maps `@stego/browser-dom` to the generated `dom/index.js`. The dependency lock
must retain the reviewed Monaco package. The source check downloads the exact
npm archive, verifies its SHA-512 integrity, checks all 11 transformed sources,
and rejects changed and already-transformed inputs. Local checks against the
saved verified archive passed. The [combined CI check](https://github.com/jsell-rh/stego/actions/runs/35164702752)
at `9792927` also passed all 11 adapter files and six browser groups. The archived
source and output hashes match the local adapter result. The generic Markdown
sanitizer paths remained unchanged.

The browser result SHA-256 is
`3a7b56f79e1608c823c7a04b2eb0b86d76386ba4f556f2a04db7f64dfa7a87da`.
The adapter result SHA-256 is
`ce2c2f2f425f48fea6e79d73418f08dfe6b93f177c95d88fb7d9e3f3d0fb4931`.
Compiler checks for `9792927` remain pending. This result does not qualify the
generated backend or the application image.

No Hypershell build currently selects this adapter. After compiler qualification,
its build must adopt the common output, rebuild the captured assets and image,
and prove the real editor. Syntax checks do not prove rendering, worker behavior,
clipboard behavior, or performance.

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
attributes before parsing. The new browser result is still required.

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

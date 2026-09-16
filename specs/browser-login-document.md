# Protected application login completion

Browser-backend version 4 changes successful OAuth callbacks for protected local
applications. The callback returns HTTP 200 with a small HTML completion page.
After that document loads, a zero-delay declarative refresh opens the validated
application return path. A link provides the same destination if the browser
does not perform the refresh. The ordinary management backend retains its
HTTP 303 callback response.

The active cookie remains Secure, HttpOnly, host-only, and SameSite=Strict.
The login cookie remains SameSite=Lax. Token exchange, nonce verification,
single-use callback state, session encryption, and session activation run before
the completion page is sent. An invalid or replayed callback cannot obtain it.
There is no additional public redirect endpoint or authentication exception.

Strict cookies can be absent from a navigation that starts at the identity
provider. An immediate HTTP redirect can therefore reach a protected document
without the new session cookie. The completion document starts the next
navigation from the application's own origin. The HTML refresh algorithm uses
the loaded document as its source and replaces its history entry. See the
[cookie rules](https://httpwg.org/http-extensions/draft-ietf-httpbis-rfc6265bis.html#section-5.6.7.1)
and [HTML refresh rules](https://html.spec.whatwg.org/multipage/semantics.html#attr-meta-http-equiv-refresh).

The page includes only the validated local return path and fixed text. It does
not copy provider tokens, callback query parameters, session IDs, or identity
claims into HTML. It requires no JavaScript or external resource. Its headers
disable caching, referrer transmission, frames, forms, and resource loading.
The opener policy is same-origin. The existing return-path validation rejects
foreign origins, authentication routes, fragments, encoded paths, control
characters, and excessive length. Template escaping protects query values.

## Evidence and limits

Hypershell run [35155975877](https://github.com/jsell-rh/hypershell-stego/actions/runs/35155975877)
passed verified HTTPS and the protected-document redirect probe. Chromium then
reached sign-in and submitted the fixture credentials. The next saved response
was HTTP 200 at `/workspaces`, with STEGO's sign-in fallback page. The upstream
Workspaces page did not appear. This is consistent with the Strict-cookie
transition problem in the generated callback. The test did not record browser
cookie exclusion reasons, so that exact browser decision remains an inference.

The focused generated-code check passed in 3.675 seconds, including return-path
bounds, HTML escaping, restrictive headers, and the manual link. It uses the
generated runtime with the race detector. The first check had an incorrect
expectation for URL normalization in the link; the corrected check compares
the parsed local path and query values. That first failure was a test error.

The common SQL tests check the real callback path, retained Strict-cookie
attributes, private-data exclusion, session access, and replay rejection.
[All six CI jobs passed](https://github.com/jsell-rh/stego/actions/runs/35157788440)
at `9cb9a674988a356ea7ec5d6daca29a1e8e7e866a`. The compiler job required
PostgreSQL and ran the generated runtime tests with the race detector. The
browser backend package passed in 253.120 seconds. The Hypershell module check
also passed, with all 83 archived source files matching its local module.

The [live Hypershell run at 55d66c2](https://github.com/jsell-rh/hypershell-stego/actions/runs/35159150673)
completed the new callback in Chromium and reached the protected upstream
Workspaces page. Workspace creation returned HTTP 201, and the detail request
returned HTTP 200. This proves the new login path in a rendered browser; a Go
cookie jar alone does not enforce browser SameSite rules.

That workflow failed later at the blank policy editor. The saved screenshot
shows the upstream global policy dialog. The same-Pod copies of its screenshot,
page text, and network record match the CI archive. All 366 generation hash
entries matched on repeat generation. Independent cleanup passed at
`2026-09-16T22:59:48.769861Z`. Editor behavior and the later recovery, access,
three-signal correlation, and deletion assertions remain unproved by this run.

Consumers with HTTP callback fixtures must handle the completion document for
protected application backends. They must not weaken their cookie assertions.
Update the compiler pin, common registry pin, generated output, and fixture
expectations together.

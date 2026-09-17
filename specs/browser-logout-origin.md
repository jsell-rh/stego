# Native browser sign-out

The generated sign-out confirmation page uses `Referrer-Policy: same-origin`.
The other backend responses keep `no-referrer`. The form must still supply one
exact Origin value and the session CSRF token. Missing, null, foreign, and
repeated Origin values are rejected.

A native form submission is a navigation request. Under `no-referrer`, the
browser sets its Origin to `null`, even when the form targets its own origin.
See the [Fetch origin header algorithm](https://fetch.spec.whatwg.org/#append-a-request-origin-header).
This caused the generated backend to reject the real Gateway dashboard's
sign-out form with HTTP 403. The failed application run was
[35167085887](https://github.com/jsell-rh/hypershell-stego/actions/runs/35167085887).
The run passed the editor, namespace replacement, and viewer access-removal
steps before that failure. It did not pass the complete application gate.

The browser CI check uses the generated handler, confirmation form, and SQL
session store. The fixture installs a session from the test login and presses
the form button. The browser supplies the POST body and request headers.
The control case restores the old policy and must receive HTTP 403 with
`Origin: null`, without session removal or token revocation. The generated
policy must permit the same-origin form, remove the stored session, revoke its
token, and reach the provider endpoint without a Referer header or cookie.
The provider endpoint is a test fixture. This check does not replace the
complete Hypershell workflow with real Keycloak.

The browser test runs only in CI. Each browser has a 30-second result deadline
and bounded process cleanup. It uses HTTPS and accepts only the fixture
certificate's public key. The result and browser logs are retained as artifacts.

# Verified CLI identity

Set `Application.IdentityPath` to enable the generated `whoami` command. The path
must select an authenticated identity endpoint on the configured API. It cannot
select another host or supply query parameters. A command declaration cannot
also use the reserved `whoami` name when this option is enabled.

The command obtains credentials through the existing token-file or OIDC session
flow. It refreshes an OIDC session when required, with the existing process lock
and uncertain-refresh rules. It then calls the identity endpoint over the
configured HTTPS transport with those credentials. Identity reporting requires
a successful API response; it does not use locally decoded claims as evidence
of identity.

The response must contain string fields `username`, `issuer`, and `subject`, and
an RFC 3339 `expires_at`. `email` is optional. These are the verified access-token
identity and expiry for that request. The default output is a JSON object with
these fields and `api_url`. Other response fields are omitted. A configured API
must enforce this endpoint contract. The CLI cannot prove that an arbitrary
server implements authentication correctly.

`--show-token` and its `-t` alias export the exact access token used for the
successful identity request. `--show-token-decoded` exports that token's JWT
payload as formatted JSON, with exact JSON numbers. These modes are exclusive.
Both require `--output-file FILE`; `--output-file -` explicitly selects stdout.
The token is never displayed by the default command. Decoding follows API
verification and does not replace signature, issuer, audience, or expiry checks
at the API. An opaque token can be exported raw but cannot be decoded as a JWT.

The output destination is reserved before credential refresh or API contact.
Existing files cause an error without those operations. Files are private and
are removed if verification or decoding fails before output. Error messages omit
remote response bodies and tokens. Input and destination rules use the existing
bounded command and HTTPS runtime. Identity responses are limited to 64 KiB.
Duplicate JSON keys, missing identity fields, invalid types, and missing expiry
cause an error without token output.

This contract requires API access, including for token export. It reports identity
at request time. It does not promise continued access, revoke an issued token, or
change the API's clock-skew policy. Default JSON output and explicit token-output
selection are intentional differences from the Hypershell reference CLI.

Generated TLS tests check API identity selection, omission of local and private
fields, protected output, exact decoded numbers, explicit stdout, existing-file
rejection, invalid definitions, conflicting flags, and failures before output.
The full compiler race suite and static checks passed. The Hypershell baseline
first rejected `whoami` as an unknown command in 3.873 seconds.

Hypershell commit `0c8092c82d962d5f9ce1c2158beee916ab5c4269` pins compiler
`c4a49b3a86e81b5a2190635505492a57cb9644d7`. It selects the current-user route
and adds verified issuer, subject, and token expiry to that response. The
application extension schema is version 1.1.0. Strict clients must update their
schema; an older server response cannot satisfy the new identity command.

Eight application workflows passed in 103.272 seconds with PostgreSQL and
Keycloak required. They covered six CLI workflows and two current-user checks.
The Gateway CLI test rejected forged and expired tokens without token output,
checked private export files and token-file rotation, and repeated identity
reporting after API restart. The OIDC test ran identity reporting beside two
resource reads during refresh and checked browser and device identities.
CLI, HTTP, and contract race tests and static checks also passed. The compiler
feature passed [CI](https://github.com/jsell-rh/stego/actions/runs/34493880749).

Post-commit `scripts/generate.sh --check` passed, and all 76 generated, dependency,
and state hashes matched. Both feature commits are on remote `main`. Local
verification covered the changed CLI and current-user contracts; it did not
repeat the full application or Kubernetes suites. This does not complete the
remaining CLI output, configuration, or enterprise requirements.

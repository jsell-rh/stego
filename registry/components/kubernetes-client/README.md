This component generates a Kubernetes resource client. Add `kubernetes-client`
to an application archetype. Bind its `application-http` requirement to
`http-application`. The client uses the generated HTTP client for verified HTTPS,
request deadlines, response limits, and connection limits.

The application supplies the API URL, CA file, and private token file. The client
reads the token for each request to permit token rotation. It does not follow
redirects or include response bodies in status errors.

`Ensure` creates an absent resource or updates an owned resource with a JSON
merge patch. The application selects the required ownership labels. All labels
must match before an update or deletion. A patch uses the observed UID and
resource version. A delete uses both values as preconditions. A conflict returns
an error; the application must read the resource again before it retries.

The client preserves caller input. It retains JSON numbers as `json.Number` to
prevent loss of integer precision. The application must check conversion errors
when it reads integer fields. `Contains` ignores extra map fields, but preserves
array order and length. Number comparisons use their exact JSON text.

`DeleteOwned` reports completion only after the resource is absent. An accepted
deletion does not mean that Kubernetes has removed the resource.

The application supplies resource definitions, placement rules, ownership labels,
readiness checks, retry policy, and RBAC. This component does not install resources
or supply a controller loop. The independent Widget test covers the common client
without Hypershell types. The Hypershell database workflow tests its use against
a Kubernetes API server.

`Observe` reads a bounded list, then watches from its resource version. It emits
`RESET` before a new list and `REPLACE` only after all pages pass validation.
Applications must stop cache-based writes after `RESET` until `REPLACE` arrives.
Changes arrive in order through one callback. The callback must return promptly.
A callback error stops observation. Normal disconnects retain the last cursor.
Expired watch history causes a new list. Access denial stops observation.
Reconnects read the token again and use a bounded delay with random variation.
HTTP rate limits can extend the delay through `Retry-After`, up to one hour plus
20 percent random variation. Resource writes are never retried by this client.

A snapshot can contain at most 10,000 objects, 64 MiB of encoded object data, and
1,000 pages. Each page can contain at most 100 objects. Each response or stream
frame is limited to 4 MiB. Each stream is limited to 64 MiB and five minutes.
The application must bound its own cache and handle observation errors.
`APIError` exposes the HTTP status and retry delay without response data.

These rules follow the Kubernetes list and watch protocol. See the
[Kubernetes API concepts](https://kubernetes.io/docs/reference/using-api/api-concepts/).
The Widget tests cover page consistency, reconnects, token rotation, expired
history, access denial, invalid paths, and callback failure.

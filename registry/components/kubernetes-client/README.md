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

# Pinned admission policies

Version 1.5.0 adds `PinnedAdmissionPolicies`. It creates Kubernetes admission
policies for one service account that must run only operator-approved resource
templates. The helper has no network calls and grants no access. It returns
policy and binding objects for the operator to install.

This supports a CI installation that must create a database or operator fixture
without permission to install cluster roles or admission policies. Template
choice and provider setup belong to the installation. The generated mechanism
contains no application or database provider names.

## Rules

Pass the service-account namespace, service-account name, and one to sixteen
`PinnedAdmissionRule` values. Each rule specifies:

- A policy `Name`, target `Namespace`, and observed `NamespaceUID`.
- `APIVersion`, `Kind`, `Resource` (plural), and exact `ResourceName`.
- `TemplateNamespace`, `TemplateName`, and observed `TemplateUID`.
- A `Mode`: `job`, `deployment`, or `spec`.
- `DeadlineSeconds` for a Job, or `LifetimeJob` for another resource.

Use DNS label names. API groups cannot contain wildcards or subresources.
Namespace and template UIDs must be canonical Kubernetes UUIDs. Names must not
conflict. One rule can select a resource type in a namespace; a second rule for
that type would deny the first target and is rejected. A target cannot be its
own template. All references to one namespace must have the same UID.

A Job has a fixed deadline from 30 through 3600 seconds, no retry, one completion,
one Pod, and immediate TTL cleanup. It cannot be suspended or use another Job
controller. Its full Pod specification must equal its template. Pod annotations
and user labels must match; Kubernetes Job identity labels can differ. The Pod
cannot have added owners or finalizers.

A Deployment must run one replica and retain its template, selector, and update
strategy. A `spec` rule applies to a namespaced custom resource with a structural
API schema; its complete specification must equal its template. Both modes
require a matching lifetime Job rule in the same namespace. Their sole owner
must name that Job. Kubernetes checks the owner UID when it collects dependents.
The caller cannot use an owner reference to block Job deletion.

Both policies and bindings have an exact namespace selector. CEL caller
conditions alone do not isolate parameter lookup failures. Missing parameters
can cause denial before those conditions run.

The policies check an operator-owned namespace identity label and restricted
Pod security. After namespace creation, the operator must set
`PinnedNamespaceUIDLabel` (`stego.dev/pinned-namespace-uid`) to the observed
namespace UID. A recreated namespace needs its new UID and new policies. Never
copy this label from an old namespace. Kubernetes does not expose the namespace
UID through `namespaceObject` in CEL, so this check relies on that operator step. They
check the parameter UID and fail if the template is absent or replaced. The
account can create and delete the exact resource. Deletion requires current UID
and resource-version preconditions, explicit foreground or background cleanup,
and at most 30 seconds of grace. Orphan and unsafe deletion are denied because
they could leave dependent workloads after the lifetime Job is gone. The account
cannot update the resource. Separate
policies deny writes through subresources, including scale and status. Existing
operator finalizers do not prevent an authorized delete request.

## Installation boundary

The operator must create and control the parameter templates. A suspended Job
and a zero-replica Deployment can serve as templates without active Pods. A
custom-resource template needs a provider-supported inactive placement or
reconciliation setting. Do not create an active template by accident.

The account must have read access to its named parameters. It must not be able
to change parameters, namespaces, quotas, admission policies, bindings, or the
roles that establish this boundary. Use exact names for read and delete grants.
Do not grant Pod creation, exec, token creation, impersonation, or arbitrary
workload permissions. The operator must inspect and approve the template images,
service accounts, volume sources, network policies, and resource limits.

Install templates first, then policies and bindings. On removal, revoke the
account's write grants first, then remove bindings before templates or namespaces.
Do not remove templates while a binding still requires them. Require successful API
server type checks and allowed/denied request probes before granting production
use. Missing policy installation is not an authorization boundary. These
policies do not provide a Lease, a namespace allocator, an image signature check,
SQL isolation, or a complete cleanup verifier. The caller must supply those
parts and preserve resource UIDs through cleanup.

The target Kubernetes server supplies schema defaults and CEL evaluation. See
[Kubernetes admission parameters](https://kubernetes.io/docs/reference/access-authn-authz/validating-admission-policy/).
The pure renderer checks pass. Live policy type checks and the unattended CNPG
workflow remain required; this source is not yet a verified CI installation.

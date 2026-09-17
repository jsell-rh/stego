# Namespace context in account admission rules

The live account gate accepted a non-allocator ServiceAccount dry-run at source
`053be41`. The request should have been denied. The unreleased account guards
used `namespaceObject` in their match conditions.

The cluster reports Kubernetes 1.35.6. Its upstream
[validator](https://github.com/kubernetes/apiserver/blob/v0.35.6/pkg/admission/plugin/policy/validating/validator.go)
calls the shared
[match-condition evaluator](https://github.com/kubernetes/apiserver/blob/v0.35.6/pkg/admission/plugin/webhook/matchconditions/matcher.go)
without a namespace object. It supplies that object to validation expressions
later. Thus, a match condition that requires a non-null namespace can skip the
policy. This source behavior explains the observed accepted request. Server
type checks alone did not detect the ineffective guard.

The account and issuer policies now select the configured namespace patterns
through `request.namespace`. Namespace labels, profile, owner, account identity,
and caller checks run in validation expressions. Missing namespace data denies
the request. Another allocator's namespace retains its own profile rules, while
the issuer guard still rejects account names from the previous installation.

The generator test rejects namespace and composed-variable dependencies in
match conditions. The complete real admission and retained-grant gate must pass
with the new generated manifests before release. The previous accepted request
remains a failed gate. No generated account component from this branch has been
released or adopted by Hypershell.

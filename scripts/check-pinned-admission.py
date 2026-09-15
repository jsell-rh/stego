#!/usr/bin/env python3
"""Check pinned policies and lifetime ownership in a dedicated namespace.

The caller holds the shared jshell Lease and removes it only after this check
confirms cleanup. Probes use one restricted service-account identity. No token
is written or printed. The renderer must come from a frozen compiler source.
"""
import argparse
import copy
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import time

IMAGE = 'docker.io/library/node@sha256:87362b5d965240a1bc79f85cec63179d4ee853741413b274a4721f2742eb8393'
LABEL = 'stego.test/pinned-admission'


def verify_policy_scope(policies, rules, namespace, actor):
    expected = {r['Name'] + suffix: r for r in rules for suffix in ['', '.subresources']}
    observed = set()
    condition = [{'name': 'selected-account-and-namespace', 'expression': 'request.userInfo.username == ' + json.dumps(actor) + ' && request.namespace == ' + json.dumps(namespace)}]
    for policy in policies:
        kind = policy.get('kind')
        name = policy.get('metadata', {}).get('name')
        key = (kind, name)
        if kind not in ['ValidatingAdmissionPolicy', 'ValidatingAdmissionPolicyBinding'] or name not in expected or key in observed:
            raise RuntimeError('The renderer returned an unexpected policy identity')
        observed.add(key)
        rule = expected[name]
        if kind == 'ValidatingAdmissionPolicy':
            spec = policy['spec']
            group, version = rule['APIVersion'].split('/')
            resource = rule['Resource'] + ('/*' if name.endswith('.subresources') else '')
            match = [{'apiGroups': [group], 'apiVersions': [version], 'operations': ['CREATE', 'UPDATE', 'DELETE'], 'resources': [resource], 'scope': 'Namespaced'}]
            if spec.get('matchConditions') != condition or spec.get('matchConstraints', {}).get('resourceRules') != match or spec.get('failurePolicy') != 'Fail':
                raise RuntimeError('The rendered policy escapes its probe identity or resource scope')
        elif policy['spec'].get('policyName') != name:
            raise RuntimeError('The renderer selected an unrelated policy')
    if len(observed) != len(expected) * 2:
        raise RuntimeError('The renderer omitted a policy or binding')


def main():
    p = argparse.ArgumentParser(description=__doc__)
    for name in ['context', 'namespace', 'lease-holder', 'lease-uid']:
        p.add_argument('--' + name, required=True)
    p.add_argument('--renderer', required=True, type=Path)
    p.add_argument('--results', required=True, type=Path)
    args = p.parse_args()
    if not re.fullmatch(r'stego-pinned-[a-z0-9]{8}', args.namespace):
        p.error('Use a new dedicated stego-pinned- namespace with an eight-character suffix')
    os.umask(0o077)
    args.results.mkdir(mode=0o700, exist_ok=False)
    created, probes = [], []
    actor = 'system:serviceaccount:' + args.namespace + ':runner'
    group = args.namespace + '.stego.test'
    def save(name, value):
        (args.results / name).write_text(json.dumps(value, indent=2) + '\n')
    def call(words, body=None, identity=None, good=True):
        command = ['oc', '--context=' + args.context, '--request-timeout=20s']
        if identity:
            command += ['--as=' + identity]
        result = subprocess.run(command + words, input=None if body is None else json.dumps(body), text=True, capture_output=True, timeout=30)
        if good and result.returncode:
            raise RuntimeError('Pinned admission request failed: ' + ' '.join(words) + '\n' + result.stderr)
        return result
    def get(kind, name, namespace=None):
        words = ['get', kind, name, '--ignore-not-found', '-o', 'json']
        if namespace:
            words += ['-n', namespace]
        raw = call(words).stdout
        return json.loads(raw) if raw.strip() else None
    lease = get('lease', 'jshell-live-test', 'stego-ci')
    if lease['metadata']['uid'] != args.lease_uid or lease['spec'].get('holderIdentity') != args.lease_holder or lease['metadata']['annotations'].get('stego.test/namespace') != args.namespace:
        raise RuntimeError('The caller must hold the shared Lease for this namespace')
    def create(item, identity=None):
        item = copy.deepcopy(item)
        item['metadata'].setdefault('labels', {})[LABEL] = args.namespace
        name, namespace = item['metadata']['name'], item['metadata'].get('namespace')
        kind = 'servers.' + group if item['kind'] == 'Server' else item['kind']
        if get(kind, name, namespace):
            raise RuntimeError('The probe refuses an existing resource: ' + kind + '/' + name)
        entry = {'kind': kind, 'name': name, 'namespace': namespace, 'state': 'requested'}
        created.append(entry); save('created.json', created)
        try:
            result = json.loads(call(['create', '-f', '-', '-o', 'json'], item, identity).stdout)
        except Exception:
            observed = get(kind, name, namespace)
            if observed and observed['metadata'].get('labels', {}).get(LABEL) == args.namespace:
                entry.update(uid=observed['metadata']['uid'], state='observed_after_error'); save('created.json', created)
            raise
        entry.update(uid=result['metadata']['uid'], state='created'); save('created.json', created)
        return result
    def item(kind, name, api='v1', namespace=args.namespace, **fields):
        metadata = {'name': name}
        if namespace:
            metadata['namespace'] = namespace
        return dict(apiVersion=api, kind=kind, metadata=metadata, **fields)
    def wait(check, seconds, message):
        end = time.monotonic() + seconds
        while not check():
            if time.monotonic() > end:
                raise RuntimeError(message)
            time.sleep(1)
    def probe(name, document, allowed, policy=None, words=None):
        result = call(words or ['create', '--dry-run=server', '-f', '-', '-o', 'json'], document, actor, good=False)
        if (result.returncode == 0) != allowed:
            raise RuntimeError('Wrong admission result: ' + name + '\n' + result.stderr)
        if not allowed and (not re.search(r'Forbidden|Invalid|forbidden|denied', result.stderr) or policy and policy not in result.stderr):
            raise RuntimeError('The probe did not reach its required policy: ' + name + '\n' + result.stderr)
        probes.append({'name': name, 'allowed': allowed, 'policy': policy}); save('probes.json', probes)
        print(('ALLOW ' if allowed else 'DENY ') + name, flush=True)
    passed = False
    try:
        namespace_object = item('Namespace', args.namespace, namespace=None)
        namespace_object['metadata']['labels'] = {'pod-security.kubernetes.io/enforce': 'restricted'}
        namespace = create(namespace_object)
        call(['label', 'namespace', args.namespace, 'stego.dev/pinned-namespace-uid=' + namespace['metadata']['uid']])
        create(item('ResourceQuota', 'limits', spec={'hard': {'pods': '2', 'limits.cpu': '50m', 'limits.memory': '64Mi', 'limits.ephemeral-storage': '32Mi', 'persistentvolumeclaims': '0', 'requests.storage': '0'}}))
        create(item('NetworkPolicy', 'deny-all', 'networking.k8s.io/v1', spec={'podSelector': {}, 'policyTypes': ['Ingress', 'Egress']}))
        create(item('ServiceAccount', 'runner', automountServiceAccountToken=False))
        pod = {'restartPolicy': 'Never', 'automountServiceAccountToken': False,
               'securityContext': {'runAsNonRoot': True, 'seccompProfile': {'type': 'RuntimeDefault'}},
               'containers': [{'name': 'sleep', 'image': IMAGE, 'command': ['/bin/sleep', '120'],
                               'securityContext': {'allowPrivilegeEscalation': False, 'readOnlyRootFilesystem': True, 'capabilities': {'drop': ['ALL']}},
                               'resources': {'requests': {'cpu': '5m', 'memory': '8Mi', 'ephemeral-storage': '1Mi'}, 'limits': {'cpu': '25m', 'memory': '32Mi', 'ephemeral-storage': '16Mi'}}}]}
        job_template = create(item('Job', 'job-template', 'batch/v1', spec={'suspend': True, 'parallelism': 1, 'completions': 1, 'backoffLimit': 0, 'activeDeadlineSeconds': 30, 'ttlSecondsAfterFinished': 0, 'template': {'metadata': {'labels': {'app': 'pinned-lifetime'}}, 'spec': pod}}))
        deployment_pod = copy.deepcopy(pod); deployment_pod['restartPolicy'] = 'Always'
        deployment_template = create(item('Deployment', 'deployment-template', 'apps/v1', spec={'replicas': 0, 'selector': {'matchLabels': {'app': 'pinned-worker'}}, 'template': {'metadata': {'labels': {'app': 'pinned-worker'}}, 'spec': deployment_pod}}))
        schema = {'type': 'object', 'properties': {'spec': {'type': 'object', 'required': ['mode', 'capacity'], 'properties': {'mode': {'type': 'string'}, 'capacity': {'type': 'integer'}}}}}
        crd = create(item('CustomResourceDefinition', 'servers.' + group, 'apiextensions.k8s.io/v1', namespace=None,
                          spec={'group': group, 'scope': 'Namespaced', 'names': {'plural': 'servers', 'singular': 'server', 'kind': 'Server'}, 'versions': [{'name': 'v1', 'served': True, 'storage': True, 'schema': {'openAPIV3Schema': schema}}]}))
        wait(lambda: any(c['type'] == 'Established' and c['status'] == 'True' for c in get('crd', crd['metadata']['name']).get('status', {}).get('conditions', [])), 30, 'The probe CRD was not established')
        server_template = create(item('Server', 'server-template', group + '/v1', spec={'mode': 'bounded', 'capacity': 1}))
        rules = []
        for mode, resource, target, template in [('job', 'jobs', 'lifetime', job_template), ('deployment', 'deployments', 'worker', deployment_template), ('spec', 'servers', 'server', server_template)]:
            rule = {'Name': args.namespace + '-' + mode, 'Namespace': args.namespace, 'NamespaceUID': namespace['metadata']['uid'],
                    'APIVersion': template['apiVersion'], 'Kind': template['kind'], 'Resource': resource, 'ResourceName': target,
                    'TemplateNamespace': args.namespace, 'TemplateName': template['metadata']['name'], 'TemplateUID': template['metadata']['uid'],
                    'Mode': mode, 'DeadlineSeconds': 30 if mode == 'job' else 0, 'LifetimeJob': '' if mode == 'job' else 'lifetime'}
            rules.append(rule)
        configuration = {'ActorNamespace': args.namespace, 'ActorName': 'runner', 'Rules': rules}
        save('renderer-input.json', configuration)
        rendered = subprocess.run([str(args.renderer.resolve())], input=json.dumps(configuration), text=True, capture_output=True, check=True, timeout=10)
        if len(rendered.stdout) > 200000:
            raise RuntimeError('The rendered policy set exceeds its limit')
        policies = json.loads(rendered.stdout)
        verify_policy_scope(policies, rules, args.namespace, actor)
        save('policies.json', policies)
        for policy in policies:
            create(policy)
        def type_checked():
            complete = True
            statuses = {}
            for policy in policies:
                if policy['kind'] != 'ValidatingAdmissionPolicy':
                    continue
                actual = get(policy['kind'], policy['metadata']['name']); status = actual.get('status', {}); statuses[policy['metadata']['name']] = status
                if status.get('observedGeneration') != actual['metadata']['generation'] or 'typeChecking' not in status:
                    complete = False
                elif status['typeChecking'].get('expressionWarnings'):
                    save('policy-status.json', statuses)
                    raise RuntimeError('Policy type check failed: ' + policy['metadata']['name'])
            save('policy-status.json', statuses)
            return complete
        wait(type_checked, 45, 'Policy type checks did not finish')
        create(item('Role', 'runner', 'rbac.authorization.k8s.io/v1', rules=[{'apiGroups': ['batch'], 'resources': ['jobs', 'jobs/status'], 'verbs': ['create', 'get', 'delete', 'update', 'patch']}, {'apiGroups': ['apps'], 'resources': ['deployments', 'deployments/scale', 'deployments/status'], 'verbs': ['create', 'get', 'delete', 'update', 'patch']}, {'apiGroups': [group], 'resources': ['servers'], 'verbs': ['create', 'get', 'delete', 'update', 'patch']}]))
        create(item('RoleBinding', 'runner', 'rbac.authorization.k8s.io/v1', roleRef={'apiGroup': 'rbac.authorization.k8s.io', 'kind': 'Role', 'name': 'runner'}, subjects=[{'kind': 'ServiceAccount', 'name': 'runner', 'namespace': args.namespace}]))
        job = item('Job', 'lifetime', 'batch/v1', spec={'suspend': False, 'parallelism': 1, 'completions': 1, 'backoffLimit': 0, 'activeDeadlineSeconds': 30, 'ttlSecondsAfterFinished': 0, 'template': {'metadata': {'labels': {'app': 'pinned-lifetime'}}, 'spec': pod}})
        probe('pinned Job', job, True)
        for marker, value in [('missing namespace identity', None), ('changed namespace identity', '11111111-2222-3333-4444-555555555555')]:
            argument = 'stego.dev/pinned-namespace-uid-' if value is None else 'stego.dev/pinned-namespace-uid=' + value
            call(['label', 'namespace', args.namespace, argument, '--overwrite'])
            probe(marker, job, False, args.namespace + '-job')
        call(['label', 'namespace', args.namespace, 'stego.dev/pinned-namespace-uid=' + namespace['metadata']['uid'], '--overwrite'])
        for name, change in [('missing deadline', lambda o: o['spec'].pop('activeDeadlineSeconds')), ('long deadline', lambda o: o['spec'].update(activeDeadlineSeconds=3600)), ('suspended Job', lambda o: o['spec'].update(suspend=True)), ('different image', lambda o: o['spec']['template']['spec']['containers'][0].update(image='example.invalid/unpinned:latest')), ('extra Pod label', lambda o: o['spec']['template']['metadata']['labels'].update(unapproved='value')), ('another Job name', lambda o: o['metadata'].update(name='other'))]:
            bad = copy.deepcopy(job); change(bad); probe(name, bad, False, args.namespace + '-job')
        owner = {'apiVersion': 'batch/v1', 'kind': 'Job', 'name': 'lifetime', 'uid': '11111111-2222-3333-4444-555555555555'}
        deployment = item('Deployment', 'worker', 'apps/v1', spec=copy.deepcopy(deployment_template['spec'])); deployment['spec']['replicas'] = 1; deployment['metadata']['ownerReferences'] = [owner]
        server = item('Server', 'server', group + '/v1', spec=copy.deepcopy(server_template['spec'])); server['metadata']['ownerReferences'] = [owner]
        probe('pinned Deployment', deployment, True)
        probe('pinned custom resource', server, True)
        for name, body, change, mode in [('ownerless Deployment', deployment, lambda o: o['metadata'].pop('ownerReferences'), 'deployment'), ('extra replica', deployment, lambda o: o['spec'].update(replicas=2), 'deployment'), ('changed server specification', server, lambda o: o['spec'].update(capacity=2), 'spec')]:
            bad = copy.deepcopy(body); change(bad); probe(name, bad, False, args.namespace + '-' + mode)
        live_job = create(job, actor); owner['uid'] = live_job['metadata']['uid']
        deployment['metadata']['ownerReferences'] = [owner]; server['metadata']['ownerReferences'] = [owner]
        live_deployment = create(deployment, actor); live_server = create(server, actor)
        probe('scale subresource', {'apiVersion': 'autoscaling/v1', 'kind': 'Scale', 'metadata': {'name': 'worker', 'namespace': args.namespace, 'resourceVersion': live_deployment['metadata']['resourceVersion']}, 'spec': {'replicas': 2}}, False, args.namespace + '-deployment.subresources', ['replace', '--raw=/apis/apps/v1/namespaces/' + args.namespace + '/deployments/worker/scale?dryRun=All', '-f', '-'])
        started = time.monotonic()
        wait(lambda: get('job', 'lifetime', args.namespace) is None and get('deployment', 'worker', args.namespace) is None and get('servers.' + group, 'server', args.namespace) is None, 120, 'The lifetime Job did not remove its owned resources')
        save('lifetime.json', {'job_uid': live_job['metadata']['uid'], 'deployment_uid': live_deployment['metadata']['uid'], 'custom_resource_uid': live_server['metadata']['uid'], 'owned_resources_absent': True, 'seconds': time.monotonic() - started})
        passed = True
    finally:
        # Remove the namespace first, then the policies and CRD. Keep ownership
        # checks even after a lost create response. Do not adopt other resources.
        ordered = sorted(created, key=lambda e: 0 if e['kind'] == 'Namespace' else 1)
        for entry in ordered:
            if entry['namespace'] is not None:
                continue
            actual = get(entry['kind'], entry['name'], entry['namespace'])
            if not actual:
                continue
            if actual['metadata']['uid'] != entry.get('uid') or actual['metadata'].get('labels', {}).get(LABEL) != args.namespace:
                raise RuntimeError('Cleanup refused a different resource owner')
            version = actual['apiVersion']; prefix = '/api/v1' if version == 'v1' else '/apis/' + version
            plurals = {'Namespace': 'namespaces', 'ResourceQuota': 'resourcequotas', 'NetworkPolicy': 'networkpolicies', 'ServiceAccount': 'serviceaccounts', 'Job': 'jobs', 'Deployment': 'deployments', 'Role': 'roles', 'RoleBinding': 'rolebindings', 'CustomResourceDefinition': 'customresourcedefinitions', 'ValidatingAdmissionPolicy': 'validatingadmissionpolicies', 'ValidatingAdmissionPolicyBinding': 'validatingadmissionpolicybindings'}
            if entry['namespace']:
                prefix += '/namespaces/' + entry['namespace']
            resource = 'servers' if entry['kind'] == 'servers.' + group else plurals[entry['kind']]
            call(['delete', '--raw=' + prefix + '/' + resource + '/' + entry['name'], '-f', '-'], {'apiVersion': 'v1', 'kind': 'DeleteOptions', 'preconditions': {'uid': entry['uid'], 'resourceVersion': actual['metadata']['resourceVersion']}, 'propagationPolicy': 'Foreground'})
            if entry['kind'] == 'Namespace':
                wait(lambda: get('namespace', args.namespace) is None, 90, 'Probe namespace cleanup is incomplete')
        wait(lambda: all(get(e['kind'], e['name'], e['namespace']) is None for e in created if e['namespace'] is None), 60, 'Probe cluster resource cleanup is incomplete')
        if get('namespace', args.namespace) is not None:
            raise RuntimeError('The probe namespace remains; retain the Lease')
        save('cleanup.json', {'namespace_absent': args.namespace, 'created_cluster_resources_absent': True})
    if passed:
        save('result.json', {'result': 'passed', 'probes': len(probes), 'renderer_sha256': hashlib.sha256(args.renderer.read_bytes()).hexdigest()})
        print('Pinned admission checks passed with cleanup.', flush=True)


if __name__ == '__main__':
    main()

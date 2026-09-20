#!/usr/bin/env python3
"""Check real application images and reject changed inputs without starting them."""
import argparse
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import tarfile


def sha(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ['compiler', 'application', 'trust-store', 'output']:
        parser.add_argument('--' + name, type=Path, required=True)
    parser.add_argument('--entrypoint', default='service')
    args = parser.parse_args()
    args.output.mkdir(mode=0o700)
    build = args.application / 'build.json'
    application = args.application / 'application'
    build_record = json.loads(build.read_text())
    cases = []

    def command(argv, *, expected=None, log=None):
        result = subprocess.run([str(arg) for arg in argv], capture_output=True, timeout=300)
        if log:
            log.write_bytes(result.stdout + result.stderr)
        if expected is None:
            assert result.returncode == 0, result.stderr.decode(errors='replace')[-4000:]
        else:
            assert result.returncode != 0 and expected in result.stderr.decode(errors='replace'), (expected, result.stderr)
        return result

    def pack(output, binary=application, trust=args.trust_store, trust_digest=None, expected=None):
        return command([args.compiler, 'image', 'build', '--build-record=' + str(build),
                        '--build-record-sha256=' + sha(build), '--application=' + str(binary),
                        '--trust-store=' + str(trust), '--trust-store-sha256=' + (trust_digest or sha(trust)),
                        '--entrypoint=' + args.entrypoint, '--output=' + str(output)], expected=expected)

    for label in ['first', 'second']:
        pack(args.output / label)
    first = args.output / 'first'
    second = args.output / 'second'
    assert (first / 'image.json').read_bytes() == (second / 'image.json').read_bytes()
    record = json.loads((first / 'image.json').read_text())
    assert record['entrypoint'] == args.entrypoint
    assert record['application'] == build_record['artifact']
    assert record['trust_store'] == {'sha256': sha(args.trust_store), 'size': args.trust_store.stat().st_size}
    assert record['build_record_sha256'] == sha(build)
    assert record['packer_compiler_artifact']['sha256'] == sha(args.compiler)
    assert record['packer_compiler']['source_state'] == 'clean'
    assert record['packer_compiler']['revision'] == os.environ['GITHUB_SHA']
    for key in ['manifest', 'config', 'layer']:
        identity = record[key]
        for root in [first, second]:
            blob = root / 'oci/blobs/sha256' / identity['sha256']
            assert sha(blob) == identity['sha256'] and blob.stat().st_size == identity['size']
    config = json.loads((first / 'oci/blobs/sha256' / record['config']['sha256']).read_text())
    assert config['architecture'] == 'amd64' and config['os'] == 'linux'
    assert config['config']['User'] == '65532:65532'
    assert config['config']['Entrypoint'] == ['/' + args.entrypoint]
    layer = first / 'oci/blobs/sha256' / record['layer']['sha256']
    with gzip.open(layer, 'rb') as stream:
        data = stream.read((130 << 20) + 1)
    assert len(data) <= 130 << 20 and hashlib.sha256(data).hexdigest() == record['layer_diff_sha256']
    with tarfile.open(fileobj=io.BytesIO(data)) as archive:
        entries = archive.getmembers()
        assert len(entries) == 5
        assert {e.name.rstrip('/') for e in entries} == {'etc', 'etc/ssl', 'etc/ssl/certs', 'etc/ssl/certs/ca-certificates.crt', args.entrypoint}
        for path, identity, mode in [(args.entrypoint, record['application'], 0o555), ('etc/ssl/certs/ca-certificates.crt', record['trust_store'], 0o444)]:
            entry = archive.getmember(path)
            assert entry.isfile() and entry.mode == mode and entry.uid == 0 and entry.gid == 0 and entry.size == identity['size']
            with archive.extractfile(entry) as stream:
                assert hashlib.file_digest(stream, 'sha256').hexdigest() == identity['sha256']
    del data

    def verify(label, image=first / 'oci', selected_record=first / 'image.json', expected=None, digest=None):
        command([args.compiler, 'image', 'verify', '--record=' + str(selected_record),
                 '--record-sha256=' + (digest or sha(selected_record)), '--image=' + str(image),
                 '--build-record=' + str(build), '--work=' + str(args.output / ('verify-' + label))], expected=expected)
        cases.append({'name': label, 'expected_success': expected is None})

    verify('original')
    verify('repeated', image=second / 'oci', selected_record=second / 'image.json')
    verify('wrong trusted digest', digest='0' * 64, expected='image record digest differs')
    changed = args.output / 'changed-record.json'
    value = json.loads((first / 'image.json').read_text())
    value['entrypoint'] = 'another'
    changed.write_text(json.dumps(value, indent=2) + '\n')
    verify('changed entry point', selected_record=changed, expected='image configuration differs')
    altered = args.output / 'altered-image'
    shutil.copytree(first / 'oci', altered)
    (altered / 'blobs/sha256' / record['layer']['sha256']).write_bytes(b'changed')
    verify('changed layer', image=altered, expected='image blob size differs')
    pack(args.output / 'wrong-ca', trust_digest='0' * 64, expected='captured image input differs')
    cases.append({'name': 'wrong CA digest', 'expected_success': False})
    key = args.output / 'private-key.txt'
    key.write_text('-----BEGIN PRIVATE KEY-----\ncHJpdmF0ZQ==\n-----END PRIVATE KEY-----\n')
    pack(args.output / 'key-image', trust=key, expected='trust store must contain only PEM certificates')
    cases.append({'name': 'private key as CA', 'expected_success': False})

    # Docker supplies an independent image and filesystem reader. Creating a
    # container does not start the application. No privileged container is used.
    export_records = []
    for label, source in [('first', first), ('second', second)]:
        work = args.output / ('export-' + label)
        command([args.compiler, 'image', 'export', '--record=' + str(source / 'image.json'),
                 '--record-sha256=' + sha(source / 'image.json'), '--image=' + str(source / 'oci'),
                 '--build-record=' + str(build), '--work=' + str(work)])
        exported = json.loads((work / 'export.json').read_text())
        archive = work / 'image.docker.tar'
        assert exported['archive'] == {'sha256': sha(archive), 'size': archive.stat().st_size}
        assert exported['image_record_sha256'] == sha(source / 'image.json')
        assert exported['source_oci_manifest_sha256'] == record['manifest']['sha256']
        assert exported['configuration_sha256'] == record['config']['sha256']
        export_records.append(exported)
    assert export_records[0] == export_records[1]
    transport = args.output / 'export-first/image.docker.tar'
    command(['docker', 'image', 'load', '--input', transport], log=args.output / 'engine-load.log')
    image_id = 'sha256:' + record['config']['sha256']
    inspected = json.loads(command(['docker', 'image', 'inspect', image_id]).stdout)
    (args.output / 'engine-image.json').write_text(json.dumps(inspected, indent=2) + '\n')
    assert len(inspected) == 1 and inspected[0]['Id'] == image_id
    assert inspected[0]['Config']['User'] == '65532:65532' and inspected[0]['Config']['Entrypoint'] == ['/' + args.entrypoint]
    container = 'stego-image-' + os.environ['GITHUB_RUN_ID'] + '-' + os.environ['GITHUB_JOB']
    created = False
    try:
        command(['docker', 'create', '--name', container, '--network=none', '--read-only', '--cap-drop=ALL',
                 '--security-opt=no-new-privileges', '--pids-limit=32', '--memory=128m', '--cpus=0.5', image_id])
        created = True
        for source, target, expected in [('/' + args.entrypoint, 'engine-application', record['application']['sha256']),
                                          ('/etc/ssl/certs/ca-certificates.crt', 'engine-ca.pem', record['trust_store']['sha256'])]:
            command(['docker', 'cp', container + ':' + source, args.output / target])
            assert sha(args.output / target) == expected
        state = json.loads(command(['docker', 'inspect', container]).stdout)[0]
        assert state['State']['Status'] == 'created' and not state['State']['Running']
    finally:
        if created:
            command(['docker', 'rm', container])
    report = {'compiler_source': os.environ['GITHUB_SHA'], 'application_source': build_record['source_revision'],
              'entrypoint': args.entrypoint, 'image_record_sha256': sha(first / 'image.json'), 'manifest': record['manifest'], 'configuration': record['config'],
              'application': record['application'], 'trust_store': record['trust_store'], 'cases': cases,
              'independent_images': 2, 'export': export_records[0], 'engine_id': image_id, 'engine_files_match': True, 'application_executed': False,
              'registry_publication_checked': False, 'records_authenticated': False}
    (args.output / 'verification.json').write_text(json.dumps(report, indent=2) + '\n')
    print(json.dumps(report))


if __name__ == '__main__':
    main()

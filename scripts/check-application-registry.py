#!/usr/bin/env python3
"""Check actual application publication through a bounded CI TLS registry."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import time


def sha(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ['compiler', 'fixture', 'image', 'output']:
        parser.add_argument('--' + name, required=True, type=Path)
    parser.add_argument('--compiler-revision', default=os.environ.get('GITHUB_SHA'))
    args = parser.parse_args()
    if os.environ.get('CI') != 'true':
        raise SystemExit('The registry gate requires CI')
    args.output.mkdir(mode=0o700)
    fixture = args.output / 'fixture'
    cases = []
    process = None
    with (args.output / 'fixture.log').open('xb') as log:
        try:
            process = subprocess.Popen([str(args.fixture), '--output=' + str(fixture)],
                                       env={'CI': 'true', 'GOMAXPROCS': '2', 'GOMEMLIMIT': '256MiB'},
                                       stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
            for attempt in range(100):
                if process.poll() is not None:
                    raise RuntimeError('The fixture stopped before it was ready')
                if (fixture / 'endpoint.json').exists():
                    break
                time.sleep(0.05)
            else:
                raise RuntimeError('The fixture did not become ready')
            repository = json.loads((fixture / 'endpoint.json').read_text())['repository']
            record = args.image / 'image.json'
            image = json.loads(record.read_text())
            assert args.compiler_revision and image['packer_compiler']['revision'] == args.compiler_revision
            base = ['--record=' + str(record), '--record-sha256=' + sha(record),
                    '--build-record=' + str(args.image / 'build.json'), '--repository=' + repository,
                    '--registry-ca=' + str(fixture / 'ca.pem'), '--registry-ca-sha256=' + sha(fixture / 'ca.pem'),
                    '--credentials=' + str(fixture / 'credentials.json')]
            def check(label, operation, replacements=None, reason=None):
                options = dict(arg.split('=', 1) for arg in base)
                options.update(replacements or {})
                work = args.output / label
                options['--work'] = str(work)
                if operation == 'publish':
                    options['--image'] = str(args.image / 'oci')
                result = subprocess.run([str(args.compiler), 'image', operation] + [key + '=' + value for key, value in options.items()],
                                        capture_output=True, timeout=180)
                if reason:
                    assert result.returncode != 0 and reason in result.stderr.decode(errors='replace'), (label, result.stderr)
                    assert not (work / 'registry.json').exists()
                else:
                    assert result.returncode == 0, (label, result.stderr)
                    receipt = json.loads((work / 'registry.json').read_text())
                    assert receipt['operation'] == operation and receipt['image_contents_verified'] is True
                    assert receipt['reference'] == repository + '@sha256:' + image['manifest']['sha256']
                    assert receipt['image_record_sha256'] == sha(record)
                    for name in ['manifest', 'config', 'layer']:
                        expected = image[name]
                        file = work / 'oci/blobs/sha256' / expected['sha256']
                        assert file.stat().st_size == expected['size'] and sha(file) == expected['sha256']
                cases.append({'name': label, 'operation': operation, 'expected_success': reason is None})
            check('published', 'publish')
            check('repeated-publication', 'publish')
            check('retrieved', 'retrieve')
            check('wrong-record', 'publish', {'--record-sha256': '0' * 64}, 'image record digest differs')
            check('wrong-registry-ca', 'retrieve', {'--registry-ca-sha256': '0' * 64}, 'registry CA digest differs')
            wrong = args.output / 'wrong-credentials.json'
            wrong.write_bytes(b'{"username":"test","password":"incorrect-fixture"}\n')
            wrong.chmod(0o600)
            check('wrong-credentials', 'publish', {'--credentials': str(wrong)}, 'registry publication failed')
            wrong.unlink()
            report = {'compiler_source': args.compiler_revision, 'image_record_sha256': sha(record),
                      'manifest_sha256': image['manifest']['sha256'], 'application': image['application'],
                      'cases': cases, 'TLS_registry_round_trip_checked': True,
                      'registry_profile': 'private CI fixture', 'production_registry_checked': False,
                      'application_executed': False}
        finally:
            if process is not None:
                process.terminate()
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=5)
                if process.returncode != 0:
                    raise RuntimeError('The fixture did not stop cleanly')
            credentials = fixture / 'credentials.json'
            if credentials.exists():
                credentials.unlink()
    report['fixture_stopped'] = True
    (args.output / 'verification.json').write_text(json.dumps(report, indent=2) + '\n')
    print(json.dumps(report, indent=2))


if __name__ == '__main__':
    main()

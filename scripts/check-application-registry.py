#!/usr/bin/env python3
"""Check actual application publication through a bounded CI TLS registry."""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import shutil
import time


def sha(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ['compiler', 'fixture', 'image', 'output']:
        parser.add_argument('--' + name, required=True, type=Path)
    parser.add_argument('--compiler-revision', default=os.environ.get('GITHUB_SHA'))
    parser.add_argument('--source', type=Path, help='Check common delivery from the complete selected source')
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
            delivery_checked = False
            if args.source is not None:
                spec = importlib.util.spec_from_file_location('delivery', Path(__file__).with_name('application-images.py'))
                delivery = importlib.util.module_from_spec(spec)
                spec.loader.exec_module(delivery)
                source = args.output / 'delivery-source'
                source.mkdir(mode=0o700)
                try:
                    build = json.loads((args.image / 'build.json').read_text())
                    for item in build['inputs']:
                        target = source / item['path']
                        target.parent.mkdir(parents=True, exist_ok=True)
                        shutil.copy2(args.source / item['path'], target)
                    selected = args.output / 'delivery-inputs'
                    selected.mkdir(mode=0o700)
                    # These CI bytes were just checked with the exact compiler.
                    # The real consumer obtains this selection from signatures.
                    item = {'name': 'application', 'module': build['module'], 'target': build['target'],
                            'entrypoint': image['entrypoint'], 'build_record_sha256': sha(args.image / 'build.json'),
                            'image_record_sha256': sha(record), 'manifest_sha256': image['manifest']['sha256']}
                    delivery.save(selected / 'images.json', {'format': 1, 'images': [item]})
                    shutil.copytree(args.image, selected / 'application')
                    token = args.output / 'delivery-token'
                    token.write_text('fixture-credential')
                    token.chmod(0o600)
                    try:
                        result = delivery.publish(selected, sha(selected / 'images.json'), source, args.compiler,
                                                  sha(args.compiler), repository.rsplit('/', 1)[0], fixture / 'ca.pem',
                                                  sha(fixture / 'ca.pem'), args.output / 'delivery',
                                                  token_file=token, username='test')
                    finally:
                        token.unlink()
                    assert result['images'] == {'application': repository + '@sha256:' + image['manifest']['sha256']}
                    assert not list((args.output / 'delivery').glob('.private-*'))
                    delivery_checked = True
                finally:
                    shutil.rmtree(source)
                    if (args.output / 'delivery-inputs').exists():
                        shutil.rmtree(args.output / 'delivery-inputs')
            report = {'compiler_source': args.compiler_revision, 'image_record_sha256': sha(record),
                      'manifest_sha256': image['manifest']['sha256'], 'application': image['application'],
                      'cases': cases, 'TLS_registry_round_trip_checked': True,
                      'registry_profile': 'private CI fixture', 'production_registry_checked': False,
                      'application_executed': False}
            if args.source is not None:
                report['common_delivery_checked'] = delivery_checked
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

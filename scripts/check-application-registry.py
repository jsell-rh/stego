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


GITHUB_ISSUER = 'https://token.actions.githubusercontent.com'


def add_signature_arguments(parser):
    # The signature check of stego image publish and image retrieve (ST-BLD-CLI06, CLI10).
    # The bundles are the CI provider's SBOM attestations about the image digest. A job
    # that has no bundles yet passes none, and the checks then show that an unsigned
    # image is neither published nor retrieved (ST-BLD-UC06-R01, ST-BLD-PO03).
    parser.add_argument('--spdx-bundle', type=Path)
    parser.add_argument('--cyclonedx-bundle', type=Path)
    parser.add_argument('--signer-issuer', default=GITHUB_ISSUER)
    parser.add_argument('--signer-subject', default=('https://github.com/' + os.environ['GITHUB_WORKFLOW_REF']) if os.environ.get('GITHUB_WORKFLOW_REF') else None)
    parser.add_argument('--gh', type=Path, default=shutil.which('gh'))
    parser.add_argument('--source-repository', default=os.environ.get('GITHUB_REPOSITORY'))


def signature_options(args):
    """Give the six signature flags as a dictionary, and whether real bundles were given."""
    if (args.spdx_bundle is None) != (args.cyclonedx_bundle is None):
        raise SystemExit('Select both attestation bundles, or neither')
    for name in ['signer_subject', 'gh', 'source_repository']:
        if not getattr(args, name):
            raise SystemExit('Select --' + name.replace('_', '-'))
    gh = Path(args.gh)
    if not gh.is_absolute():
        raise SystemExit('Select the GitHub CLI by its absolute path')
    signed = args.spdx_bundle is not None
    # Without bundles, the paths name files that do not exist, so the check
    # refuses the image as unsigned after every other check passed.
    spdx = args.spdx_bundle if signed else args.output / 'no-spdx-bundle.json'
    cyclonedx = args.cyclonedx_bundle if signed else args.output / 'no-cyclonedx-bundle.json'
    return {'--spdx-bundle': str(spdx), '--cyclonedx-bundle': str(cyclonedx),
            '--signer-issuer': args.signer_issuer, '--signer-subject': args.signer_subject,
            '--gh': str(gh), '--source-repository': args.source_repository}, signed


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ['compiler', 'fixture', 'image', 'output']:
        parser.add_argument('--' + name, required=True, type=Path)
    parser.add_argument('--compiler-revision', default=os.environ.get('GITHUB_SHA'))
    parser.add_argument('--source', type=Path, help='Check common delivery from the complete selected source')
    add_signature_arguments(parser)
    parser.add_argument('--image-signed', type=Path,
                        help='The Image signed event that stego image sign printed; publish once more with --after')
    args = parser.parse_args()
    if os.environ.get('CI') != 'true':
        raise SystemExit('The registry gate requires CI')
    args.output.mkdir(mode=0o700)
    signature, signed = signature_options(args)
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
            def check(label, operation, replacements=None, reason=None, omit=()):
                options = dict(arg.split('=', 1) for arg in base)
                # Publication checks the signature before it pushes (ST-BLD-UC06-R01), and
                # retrieval before it writes the image (ST-BLD-UC07-R01).
                options.update(signature)
                command = operation
                if operation == 'publish-after':
                    # The policy's way in (ST-BLD-PO03): the Image signed event and the image folder.
                    command = 'publish'
                    for key in ['--record', '--record-sha256', '--build-record']:
                        del options[key]
                    options['--after'] = str(args.image_signed)
                    options['--image'] = str(args.image)
                elif operation == 'publish':
                    options['--image'] = str(args.image / 'oci')
                options.update(replacements or {})
                for key in omit:
                    del options[key]
                work = args.output / label
                options['--work'] = str(work)
                result = subprocess.run([str(args.compiler), 'image', command] + [key + '=' + value for key, value in options.items()],
                                        capture_output=True, timeout=180)
                if reason:
                    assert result.returncode != 0 and reason in result.stderr.decode(errors='replace'), (label, result.stderr)
                    assert not (work / 'registry.json').exists()
                else:
                    assert result.returncode == 0, (label, result.stderr)
                    receipt = json.loads((work / 'registry.json').read_text())
                    assert receipt['operation'] == command and receipt['image_contents_verified'] is True
                    assert receipt['reference'] == repository + '@sha256:' + image['manifest']['sha256']
                    assert receipt['image_record_sha256'] == sha(record)
                    for name in ['manifest', 'config', 'layer']:
                        expected = image[name]
                        file = work / 'oci/blobs/sha256' / expected['sha256']
                        assert file.stat().st_size == expected['size'] and sha(file) == expected['sha256']
                cases.append({'name': label, 'operation': operation, 'expected_success': reason is None})
            if signed:
                check('published', 'publish')
                check('repeated-publication', 'publish')
                if args.image_signed is not None:
                    check('published-after', 'publish-after')
                check('retrieved', 'retrieve')
            else:
                # Without the provider's bundles, the image passes every other
                # check and is then refused as unsigned: it is not published,
                # so the registry holds nothing to retrieve.
                check('published-unsigned', 'publish', reason='the SPDX signature bundle')
            check('publish-missing-signature-flag', 'publish', reason='image publish requires --spdx-bundle', omit=('--spdx-bundle',))
            check('retrieve-missing-signature-flag', 'retrieve', reason='image retrieve requires --spdx-bundle', omit=('--spdx-bundle',))
            check('wrong-record', 'publish', {'--record-sha256': '0' * 64}, 'image record digest differs')
            check('wrong-registry-ca', 'retrieve', {'--registry-ca-sha256': '0' * 64}, 'registry CA digest differs')
            if signed:
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
                    if signed:
                        # Delivery publishes only a signed image (ST-BLD-UC06-R01): the
                        # provider's bundles go beside the image record.
                        for bundle, name in [(args.spdx_bundle, 'sbom.spdx.bundle.json'), (args.cyclonedx_bundle, 'sbom.cdx.bundle.json')]:
                            shutil.copyfile(bundle, selected / 'application' / name)
                    signer = {'issuer': args.signer_issuer, 'subject': args.signer_subject, 'gh': str(args.gh),
                              'source_repository': args.source_repository}
                    token = args.output / 'delivery-token'
                    token.write_text('fixture-credential')
                    token.chmod(0o600)
                    try:
                        def deliver():
                            return delivery.publish(selected, sha(selected / 'images.json'), source, args.compiler,
                                                    sha(args.compiler), repository.rsplit('/', 1)[0], fixture / 'ca.pem',
                                                    sha(fixture / 'ca.pem'), args.output / 'delivery',
                                                    token_file=token, username='test', signer=signer)
                        if signed:
                            result = deliver()
                        else:
                            # An unsigned image set is refused before any credential exists.
                            try:
                                deliver()
                            except delivery.CheckError as error:
                                assert 'signature bundle' in str(error), error
                            else:
                                raise AssertionError('Delivery published an unsigned image')
                            assert not list((args.output / 'delivery').glob('**/credentials.json'))
                    finally:
                        token.unlink()
                    if signed:
                        assert result['images'] == {'application': repository + '@sha256:' + image['manifest']['sha256']}
                    assert not list((args.output / 'delivery').glob('.private-*'))
                    delivery_checked = True
                finally:
                    shutil.rmtree(source)
                    if (args.output / 'delivery-inputs').exists():
                        shutil.rmtree(args.output / 'delivery-inputs')
            report = {'compiler_source': args.compiler_revision, 'image_record_sha256': sha(record),
                      'manifest_sha256': image['manifest']['sha256'], 'application': image['application'],
                      'cases': cases, 'TLS_registry_round_trip_checked': signed,
                      'registry_profile': 'private CI fixture', 'production_registry_checked': False,
                      'application_executed': False, 'signature_checked': signed}
            if args.source is not None:
                report['common_delivery_checked'] = delivery_checked
                report['common_delivery_signed'] = signed
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

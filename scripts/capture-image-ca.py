#!/usr/bin/env python3
"""Capture the existing publisher CA bundle from its pinned SDK image in CI.

The record includes every certificate's notAfter date and the expired roots
at capture time, so production trust-profile review does not depend on
manual inspection."""
import argparse
import base64
import datetime
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import subprocess

IMAGE = 'docker.io/library/golang@sha256:2d54f6c8c6ea532a321e0b4c69553b2ed3637608d4f4357dbed37939fe2620cc'


spec = importlib.util.spec_from_file_location('control', Path(__file__).with_name('check-compiler-artifact.py'))
control = importlib.util.module_from_spec(spec)
spec.loader.exec_module(control)


def command(args, timeout=60):
    return control.command(args, Path.cwd(), dict(os.environ, LC_ALL='C'), timeout=timeout, limit=1 << 20)


NOT_AFTER = rb'notAfter=([A-Z][a-z]{2} [ 0-9]{2} [0-9:]{8} [0-9]{4} GMT)\n'


def parse_not_after(output):
    """Convert one openssl enddate line into an aware UTC datetime."""
    parsed = re.fullmatch(NOT_AFTER, output)
    if not parsed:
        raise ValueError('unexpected notAfter output')
    return datetime.datetime.strptime(parsed.group(1).decode('ascii'), '%b %d %H:%M:%S %Y GMT').replace(tzinfo=datetime.timezone.utc)


def expired_certificates(certificates, expiries, captured):
    """Return the digests of roots whose notAfter is not after capture time."""
    if len(certificates) != len(expiries):
        raise ValueError('certificate and expiry lists differ')
    return [certificates[index] for index in range(len(certificates)) if expiries[index] <= captured]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', required=True, type=Path)
    args = parser.parse_args()
    if os.environ.get('CI') != 'true':
        raise SystemExit('The CA capture requires CI')
    args.output.mkdir(mode=0o700)
    command(['docker', 'pull', '--platform=linux/amd64', IMAGE], timeout=180)
    inspected = json.loads(command(['docker', 'image', 'inspect', IMAGE]))
    assert len(inspected) == 1 and inspected[0]['Os'] == 'linux' and inspected[0]['Architecture'] == 'amd64'
    assert any(value.endswith('@' + IMAGE.split('@')[1]) for value in inspected[0]['RepoDigests'])
    container = 'stego-ca-' + os.environ['GITHUB_RUN_ID'] + '-' + os.environ['GITHUB_RUN_ATTEMPT']
    created = False
    try:
        command(['docker', 'create', '--name', container, '--network=none', '--read-only', '--user=65532:65532',
                 '--cap-drop=ALL', '--security-opt=no-new-privileges', '--pids-limit=32', '--memory=128m', '--cpus=0.5', IMAGE, '/bin/false'])
        created = True
        bundle = args.output / 'ca-certificates.crt'
        command(['docker', 'cp', container + ':/etc/ssl/certs/ca-certificates.crt', str(bundle)])
        notice = args.output / 'ca-certificates-copyright.txt'
        command(['docker', 'cp', container + ':/usr/share/doc/ca-certificates/copyright', str(notice)])
        notice_data = notice.read_bytes()
        assert 0 < len(notice_data) <= 64 << 10
        data = bundle.read_bytes()
        assert 0 < len(data) <= 1 << 20
        pattern = rb'-----BEGIN CERTIFICATE-----\s+([A-Za-z0-9+/=\s]+?)-----END CERTIFICATE-----'
        matches = list(re.finditer(pattern, data))
        assert 1 <= len(matches) <= 512 and not re.sub(pattern, b'', data).strip()
        certificates = []
        expiries = []
        for index, match in enumerate(matches):
            der = base64.b64decode(re.sub(rb'\s+', b'', match.group(1)), validate=True)
            certificate = args.output / ('certificate-' + str(index) + '.pem')
            certificate.write_bytes(match.group(0) + b'\n')
            text = command(['openssl', 'x509', '-in', str(certificate), '-noout', '-text'])
            assert re.search(rb'X509v3 Basic Constraints:[^\n]*\n\s+CA:TRUE', text)
            expires = parse_not_after(command(['openssl', 'x509', '-in', str(certificate), '-noout', '-enddate']))
            certificates.append(hashlib.sha256(der).hexdigest())
            expiries.append(expires)
            certificate.unlink()
        captured = datetime.datetime.now(datetime.timezone.utc)
        expired = expired_certificates(certificates, expiries, captured)
        record = {'format': 1, 'compiler_source': os.environ['GITHUB_SHA'], 'source_image': IMAGE,
                  'image_id': inspected[0]['Id'], 'platform': 'linux/amd64',
                  'path': '/etc/ssl/certs/ca-certificates.crt',
                  'bundle': {'sha256': hashlib.sha256(data).hexdigest(), 'size': len(data)},
                  'CA_certificates': certificates,
                  'CA_not_after': [moment.strftime('%Y-%m-%dT%H:%M:%SZ') for moment in expiries],
                  'capture_time': captured.strftime('%Y-%m-%dT%H:%M:%SZ'),
                  'expired_CA_certificates': expired,
                  'source_notice': {'sha256': hashlib.sha256(notice_data).hexdigest(), 'size': len(notice_data)},
                  'container_started': False,
                  'production_adoption_checked': False}
    finally:
        if created:
            command(['docker', 'rm', container])
            result = subprocess.run(['docker', 'container', 'inspect', container], capture_output=True, timeout=15)
            assert result.returncode != 0
    record['container_removed'] = True
    (args.output / 'capture.json').write_text(json.dumps(record, indent=2) + '\n')
    (args.output / 'image.json').write_text(json.dumps(inspected, indent=2) + '\n')
    print(json.dumps(record, indent=2))


if __name__ == '__main__':
    main()

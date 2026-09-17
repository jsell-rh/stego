"""Check the common adapter against the exact reviewed npm source."""
import argparse
import base64
import hashlib
import io
import json
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import urllib.request

parser = argparse.ArgumentParser()
parser.add_argument('--output', type=Path, required=True)
args = parser.parse_args()
if os.environ.get('GITHUB_ACTIONS') != 'true':
    raise SystemExit('Use the saved verified source for local adapter checks')
root = Path(__file__).resolve().parents[1]
source = root / 'internal/generator/browserdom'
patches = json.loads((source / 'monaco-patches.json').read_text())
assert patches['version'] == '0.52.2'
with urllib.request.urlopen('https://registry.npmjs.org/monaco-editor/-/monaco-editor-0.52.2.tgz', timeout=30) as response:
    data = response.read(64 * 1024 * 1024 + 1)
assert len(data) <= 64 * 1024 * 1024
expected = 'GEQWEZmfkOGLdd3XK8ryrfWz3AIP8YymVXiPHEdewrUq7mh0qrKrfHLNCXcbB6sTnMLnOZ3ztSiKcciFUkIJwQ=='
assert base64.b64encode(hashlib.sha512(data).digest()).decode() == expected
args.output.parent.mkdir(parents=True, exist_ok=True)
with tempfile.TemporaryDirectory(prefix='stego-monaco-source-') as directory:
    selected = set()
    with tarfile.open(fileobj=io.BytesIO(data), mode='r:gz') as archive:
        for member in archive:
            prefix = 'package/esm/'
            if not member.name.startswith(prefix):
                continue
            name = member.name[len(prefix):]
            if name not in patches['files']:
                continue
            assert name not in selected and member.isfile() and member.size <= 4 * 1024 * 1024
            assert not Path(name).is_absolute() and '..' not in Path(name).parts
            target = Path(directory) / name
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(archive.extractfile(member).read())
            selected.add(name)
    assert selected == set(patches['files'])
    subprocess.run(['node', str(source / 'testdata/monaco-adapter.test.cjs'), directory, str(args.output)], check=True, timeout=45)
record = json.loads(args.output.read_text())
record['npm_integrity'] = 'sha512-' + expected
args.output.write_text(json.dumps(record, indent=2) + '\n')

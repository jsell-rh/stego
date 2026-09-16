"""Run the bounded DOM and content-policy checks on a CI runner."""
import argparse
import http.server
import json
import os
from pathlib import Path
import secrets
import shutil
import signal
import subprocess
import tempfile
import threading

parser = argparse.ArgumentParser()
parser.add_argument('--output', type=Path, required=True)
args = parser.parse_args()
if os.environ.get('GITHUB_ACTIONS') != 'true':
    raise SystemExit('Run browser checks in CI, not on the developer workstation')
browser = shutil.which('google-chrome') or shutil.which('chromium')
if not browser:
    raise SystemExit('A test browser is required')
source = Path(__file__).resolve().parents[1] / 'internal/generator/browserdom'
nonce = secrets.token_urlsafe(32)
finished = threading.Event()
result = None


class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_GET(self):
        if self.path == '/':
            body = ('<!doctype html><html><head><meta name="stego-style-nonce" content="' + nonce + '">'
                    '<script type="module" src="/test.js"></script></head><body></body></html>').encode()
            content_type = 'text/html; charset=utf-8'
        elif self.path in ('/runtime.js', '/test.js'):
            body = (source / ('runtime.js' if self.path == '/runtime.js' else 'testdata/browser.test.js')).read_bytes()
            content_type = 'text/javascript; charset=utf-8'
        else:
            self.send_error(404)
            return
        self.send_response(200)
        self.send_header('Content-Type', content_type)
        self.send_header('Content-Length', str(len(body)))
        self.send_header('Cache-Control', 'no-store')
        self.send_header('X-Content-Type-Options', 'nosniff')
        self.send_header('Content-Security-Policy', "default-src 'none'; script-src 'self'; connect-src 'self'; style-src 'self' 'nonce-" + nonce + "'; style-src-attr 'none'; base-uri 'none'; frame-ancestors 'none'")
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        global result
        length = int(self.headers.get('Content-Length', '0'))
        if self.path != '/result' or length < 1 or length > 16384 or finished.is_set():
            self.send_error(400)
            return
        result = json.loads(self.rfile.read(length))
        self.send_response(204)
        self.end_headers()
        finished.set()


server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
server.daemon_threads = True
thread = threading.Thread(target=server.serve_forever, daemon=True)
thread.start()
args.output.parent.mkdir(parents=True, exist_ok=True)
try:
    with tempfile.TemporaryDirectory(prefix='stego-style-browser-') as profile:
        with args.output.with_suffix('.browser.log').open('wb') as log:
            process = subprocess.Popen([browser, '--headless=new', '--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage', '--disable-background-networking', '--no-first-run', '--user-data-dir=' + profile, 'http://127.0.0.1:' + str(server.server_port)], stdout=log, stderr=log, start_new_session=True)
            try:
                if not finished.wait(45):
                    raise RuntimeError('Browser result deadline expired')
            finally:
                if process.poll() is None:
                    os.killpg(process.pid, signal.SIGTERM)
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    os.killpg(process.pid, signal.SIGKILL)
                    process.wait(timeout=5)
finally:
    server.shutdown()
    server.server_close()
if not isinstance(result, dict) or len(result.get('results', [])) != 6:
    raise SystemExit('Missing browser checks')
result['browser'] = subprocess.check_output([browser, '--version'], text=True, timeout=5).strip()
args.output.write_text(json.dumps(result, indent=2) + '\n')
print(json.dumps(result, indent=2))
if not result.get('passed') or not all(item.get('passed') is True for item in result['results']):
    raise SystemExit(1)

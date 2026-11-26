"""Verify scheduled maintenance using a compiled monitor, temporary DB, and local webhook."""
import contextlib
import json
import os
from pathlib import Path
import secrets
import socket
import sqlite3
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

binary = str(Path(sys.argv[1] if len(sys.argv) > 1 else './status-monitor').resolve())
events = []
lock = threading.Lock()


class Receiver(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_POST(self):
        data = json.loads(self.rfile.read(int(self.headers.get('Content-Length', '0'))))
        with lock:
            events.append(data)
        self.send_response(204)
        self.end_headers()


hook = ThreadingHTTPServer(('127.0.0.1', 0), Receiver)
thread = threading.Thread(target=hook.serve_forever, daemon=True)
thread.start()
with socket.socket() as port_socket:
    port_socket.bind(('127.0.0.1', 0))
    port = port_socket.getsockname()[1]
base = 'http://127.0.0.1:' + str(port)
token = secrets.token_urlsafe(24)


def call(path, method='GET', payload=None, admin=True):
    data = json.dumps(payload).encode() if payload is not None else None
    headers = {'X-Admin-Token': token} if admin else {}
    if data is not None:
        headers['Content-Type'] = 'application/json'
    request = urllib.request.Request(base + path, data=data, headers=headers, method=method)
    try:
        response = urllib.request.urlopen(request, timeout=5)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        return response.status, json.loads(response.read())


def wait_for(check, seconds=16):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        if check():
            return
        time.sleep(0.15)
    raise AssertionError('Timed out waiting for expected behavior')


def count_events():
    with lock:
        return len(events)


try:
    with tempfile.TemporaryDirectory(prefix='maintenance-live-') as folder:
        db_path = Path(folder) / 'monitor.db'
        config = {
            'server': {'addr': '127.0.0.1:' + str(port)},
            'storage': {'path': str(db_path)},
            'probe': {'interval_seconds': 10, 'timeout_seconds': 2, 'concurrency': 1},
            'admin': {'token': 'env:MAINTENANCE_TEST_ADMIN'},
            'alerts': {'enabled': True, 'webhook_url': 'http://127.0.0.1:' + str(hook.server_address[1]), 'fail_threshold': 2, 'recover_threshold': 1},
            'api': {'enabled': True, 'rate_limit_rpm': 1000, 'cache_seconds': 0},
        }
        config_path = Path(folder) / 'config.json'
        config_path.write_text(json.dumps(config))
        env = dict(os.environ, MAINTENANCE_TEST_ADMIN=token)
        log = (Path(folder) / 'monitor.log').open('w+')
        proc = None

        def start():
            global proc
            proc = subprocess.Popen([binary, '-config', str(config_path)], env=env, cwd=folder, stdout=log, stderr=log)
            def ready():
                if proc.poll() is not None:
                    log.seek(0)
                    raise AssertionError(log.read())
                try:
                    return call('/api/health')[0] == 200
                except OSError:
                    return False
            wait_for(ready, 6)

        def stop():
            if proc and proc.poll() is None:
                proc.terminate()
                proc.wait(timeout=8)

        def set_channel(enabled=True, fail=True):
            status, body = call('/api/admin/channels', 'POST', {
                'id': 'game', 'name': 'Local simulated game', 'type': 'mock', 'enabled': enabled,
                'config': {'fail_rate': 1 if fail else 0, 'latency_ms': [0, 0], 'interval_seconds': 10},
            })
            assert status == 200, body

        def probe():
            deadline = time.monotonic() + 5
            while True:
                status, body = call('/api/admin/probe/game', 'POST')
                if status != 409:
                    assert status == 200, body
                    return body
                assert time.monotonic() < deadline
                time.sleep(0.05)

        def recorded_suppressed():
            with sqlite3.connect('file:' + str(db_path) + '?mode=ro', uri=True) as db:
                return db.execute("SELECT COUNT(*) FROM probes WHERE channel_id='game' AND maintenance_id IS NOT NULL").fetchone()[0]

        try:
            start()
            set_channel(False)
            now = int(time.time())
            status, window = call('/api/admin/maintenance', 'POST', {
                'title': 'Local restart window', 'body': 'Keep probing, suppress notifications.',
                'start_at': 0, 'end_at': now + 300, 'channel_ids': ['game'],
            })
            assert status == 201 and window['state'] == 'active', window
            set_channel(True)
            wait_for(lambda: recorded_suppressed() >= 2, 16)
            assert count_events() == 0, events
            status, snapshot = call('/api/status')
            game = snapshot['channels'][0]
            assert game['maintenance']['id'] == window['id'] and game['current_status'] == 'partial'
            assert game['availability'] == 0 and game['probes'] >= 2
            assert game['buckets'][-1]['status'] == 'down'
            assert call('/api/admin/incidents')[1] == []
            print('PASS: scheduled failed probes are recorded but do not notify or create outages')

            previous = recorded_suppressed()
            stop()
            start()
            wait_for(lambda: recorded_suppressed() > previous, 6)
            assert count_events() == 0
            print('PASS: maintenance remains effective across process restart')

            assert call('/api/admin/maintenance/' + str(window['id']) + '/cancel', 'POST', {})[0] == 200
            assert probe()['ok'] is False
            assert count_events() == 0
            assert probe()['ok'] is False
            wait_for(lambda: count_events() == 1, 3)
            assert events[0]['event'] == 'down'
            assert call('/api/status')[1]['channels'][0]['maintenance'] is None
            assert call('/api/dynamics/m' + str(window['id']))[1]['state'] == 'resolved'
            set_channel(False, False)
            assert probe()['ok'] is True
            wait_for(lambda: count_events() == 2, 3)
            assert events[1]['event'] == 'recovered'
            print('PASS: early end resumes fresh failure threshold and normal recovery notification')

            current_events = count_events()
            now = int(time.time())
            status, short = call('/api/admin/maintenance', 'POST', {
                'title': 'Automatic expiry', 'start_at': now + 2, 'end_at': now + 5, 'all_channels': True,
            })
            assert status == 201 and short['state'] == 'scheduled'
            wait_for(lambda: any(m['id'] == short['id'] and m['state'] == 'active' for m in call('/api/admin/maintenance')[1]), 5)
            set_channel(False, True)
            assert probe()['ok'] is False
            assert probe()['ok'] is False
            assert count_events() == current_events
            wait_for(lambda: any(m['id'] == short['id'] and m['state'] == 'completed' for m in call('/api/admin/maintenance')[1]), 6)
            assert probe()['ok'] is False
            assert count_events() == current_events
            assert probe()['ok'] is False
            wait_for(lambda: count_events() == current_events + 1, 3)
            assert call('/api/maintenance')[1] == []
            assert call('/v1/maintenance')[1] == []
            assert call('/api/admin/maintenance', admin=False)[0] == 401
            print('PASS: future start and automatic end restore normal alerts without manual intervention')
            print('Compiled maintenance acceptance passed; only temporary local channels and local notifications were used.')
        finally:
            with contextlib.suppress(Exception):
                stop()
            if proc and proc.poll() is None:
                proc.kill()
                proc.wait(timeout=5)
            log.close()
finally:
    hook.shutdown()
    hook.server_close()
    thread.join(timeout=5)

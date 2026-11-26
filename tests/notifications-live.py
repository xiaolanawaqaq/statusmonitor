"""Run the compiled monitor against local notification receivers only."""
import base64
import hashlib
import hmac
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

binary = str(Path(sys.argv[1] if len(sys.argv) > 1 else "./status-monitor").resolve())
counts = {}
validation_errors = []
fail_dingtalk = False
lock = threading.Lock()


class Receiver(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_POST(self):
        endpoint = urllib.parse.urlsplit(self.path)
        receiver = endpoint.path.strip("/")
        if receiver == "push":
            receiver = "bark"
        if receiver == "topic":
            receiver = "ntfy"
        raw = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        try:
            payload = json.loads(raw) if receiver != "ntfy" else raw.decode()
            if receiver == "dingtalk":
                query = urllib.parse.parse_qs(endpoint.query)
                stamp = query["timestamp"][0]
                signature = base64.b64encode(hmac.new(b"local-ding-secret", (stamp + "\nlocal-ding-secret").encode(), hashlib.sha256).digest()).decode()
                assert hmac.compare_digest(query["sign"][0], signature)
                assert abs(time.time() * 1000 - int(stamp)) < 60000
                assert query["access_token"] == ["local-token"]
                assert payload["msgtype"] == "text" and payload["at"]["isAtAll"] is False
            elif receiver == "feishu":
                stamp = payload["timestamp"]
                signature = base64.b64encode(hmac.new((stamp + "\nlocal-feishu-secret").encode(), b"", hashlib.sha256).digest()).decode()
                assert hmac.compare_digest(payload["sign"], signature)
                assert payload["msg_type"] == "text"
            elif receiver == "wecom":
                assert payload["msgtype"] == "text"
            elif receiver == "discord":
                assert payload["allowed_mentions"] == {"parse": []}
            elif receiver == "slack":
                assert payload["blocks"][0]["text"]["type"] == "plain_text"
            elif receiver == "bark":
                assert payload["device_key"] == "local-device"
            elif receiver == "ntfy":
                assert self.headers["Authorization"] == "Bearer local-ntfy"
                assert "本地监控" in payload
            else:
                assert receiver == "webhook" and payload["event"] == "test"
            if receiver != "ntfy":
                assert "本地监控" in json.dumps(payload, ensure_ascii=False)
        except Exception as error:
            with lock:
                validation_errors.append(receiver + ": " + str(error))
            self.send_response(500)
            self.end_headers()
            return
        with lock:
            counts[receiver] = counts.get(receiver, 0) + 1
        responses = {
            "webhook": {}, "dingtalk": {"errcode": 310000 if fail_dingtalk else 0},
            "feishu": {"code": 0}, "wecom": {"errcode": 0}, "bark": {"code": 200},
            "ntfy": {"id": "local-message", "event": "message"},
        }
        self.send_response(204 if receiver == "discord" else 200)
        self.send_header("Content-Type", "text/plain" if receiver == "slack" else "application/json")
        self.end_headers()
        if receiver != "discord":
            self.wfile.write(b"ok" if receiver == "slack" else json.dumps(responses[receiver]).encode())


server = ThreadingHTTPServer(("127.0.0.1", 0), Receiver)
thread = threading.Thread(target=server.serve_forever, daemon=True)
thread.start()
receiver_url = "http://127.0.0.1:" + str(server.server_address[1])
with socket.socket() as port_socket:
    port_socket.bind(("127.0.0.1", 0))
    monitor_port = port_socket.getsockname()[1]
base = "http://127.0.0.1:" + str(monitor_port)
admin_token = secrets.token_urlsafe(24)


def call(path, method="GET", payload=None, admin=True):
    data = json.dumps(payload).encode() if payload is not None else None
    headers = {"X-Admin-Token": admin_token} if admin else {}
    if data is not None:
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(base + path, data=data, headers=headers, method=method)
    try:
        resp = urllib.request.urlopen(req, timeout=10)
    except urllib.error.HTTPError as error:
        resp = error
    with resp:
        return resp.status, json.loads(resp.read())


try:
    with tempfile.TemporaryDirectory(prefix="monitor-notify-") as folder:
        config = {
            "server": {"addr": "127.0.0.1:" + str(monitor_port)},
            "storage": {"path": str(Path(folder) / "test.db")},
            "admin": {"token": "env:NOTIFY_TEST_ADMIN"},
            "alerts": {
                "enabled": True, "message_prefix": "本地监控",
                "webhook_url": receiver_url + "/webhook",
                "dingtalk_webhook_url": receiver_url + "/dingtalk?access_token=local-token", "dingtalk_secret": "local-ding-secret",
                "feishu_webhook_url": receiver_url + "/feishu", "feishu_secret": "local-feishu-secret",
                "wecom_webhook_url": receiver_url + "/wecom", "discord_webhook_url": receiver_url + "/discord",
                "slack_webhook_url": receiver_url + "/slack", "bark_server_url": receiver_url, "bark_device_key": "local-device",
                "ntfy_url": receiver_url + "/topic", "ntfy_token": "local-ntfy",
            },
        }
        config_path = Path(folder) / "config.json"
        config_path.write_text(json.dumps(config), encoding="utf-8")
        env = dict(os.environ, NOTIFY_TEST_ADMIN=admin_token)
        with (Path(folder) / "monitor.log").open("w+") as log:
            proc = subprocess.Popen([binary, "-config", str(config_path)], cwd=folder, env=env, stdout=log, stderr=log)
            try:
                for _ in range(50):
                    if proc.poll() is not None:
                        log.seek(0)
                        raise RuntimeError(log.read())
                    try:
                        assert call("/api/health")[0] == 200
                        break
                    except OSError:
                        time.sleep(0.1)
                else:
                    raise RuntimeError("monitor did not start")
                code, metadata = call("/api/admin/alerts")
                assert code == 200 and len(metadata["receivers"]) == 9
                assert sum(r["configured"] for r in metadata["receivers"]) == 8
                assert "local-ding-secret" not in json.dumps(metadata)
                code, result = call("/api/admin/alerts/test", "POST", {})
                assert code == 200 and result["ok"] and len(result["results"]) == 8, result
                assert all(r["ok"] for r in result["results"])
                before = dict(counts)
                code, result = call("/api/admin/alerts/test", "POST", {"receiver": "dingtalk"})
                assert code == 200 and len(result["results"]) == 1
                assert counts["dingtalk"] == before["dingtalk"] + 1
                assert all(counts[k] == v for k, v in before.items() if k != "dingtalk")
                fail_dingtalk = True
                code, result = call("/api/admin/alerts/test", "POST", {})
                assert code == 502 and result["ok"] is False
                assert sum(r["ok"] for r in result["results"]) == 7
                assert [r["receiver"] for r in result["results"] if not r["ok"]] == ["dingtalk"]
                assert call("/api/admin/alerts", admin=False)[0] == 401
                assert call("/api/admin/alerts/test", "POST", {"receiver": "telegram"})[0] == 400
                assert not validation_errors, validation_errors
                print("Local compiled-server verification passed: 8 configured receivers, signed DingTalk/Feishu, selected test, partial failure, auth, no external notifications.")
            finally:
                proc.terminate()
                try:
                    proc.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    proc.kill()
                    proc.wait(timeout=5)
finally:
    server.shutdown()
    server.server_close()
    thread.join(timeout=5)

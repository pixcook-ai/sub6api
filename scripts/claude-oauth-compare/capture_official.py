#!/usr/bin/env python3
"""Capture an integrity-verified official Claude CLI against an offline local server."""
from __future__ import annotations
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import subprocess
import tarfile
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.request import urlopen

REGISTRY = "https://registry.npmjs.org"
SYNTHETIC_TOKEN = "sk-ant-oat01-sub6api-local-synthetic-token-not-a-real-credential"
SYNTHETIC_UUID = "00000000-0000-4000-8000-000000000001"
SENSITIVE_HEADERS = {"authorization", "x-api-key", "cookie", "set-cookie", "x-claude-code-wiggly-dove", "x-anthropic-additional-protection"}

def read_url(url):
    with urlopen(url, timeout=120) as response:
        return response.read()

def acquire(version, target):
    if platform.system() != "Darwin" or platform.machine() not in ("arm64", "aarch64"):
        raise RuntimeError("This pinned capture targets official darwin-arm64 artifacts")
    target.mkdir(parents=True, exist_ok=True)
    metadata_url = f"{REGISTRY}/@anthropic-ai%2fclaude-code-darwin-arm64/{version}"
    meta_path = target / "native-npm-manifest.json"
    manifest_bytes = meta_path.read_bytes() if meta_path.exists() else read_url(metadata_url)
    meta_path.write_bytes(manifest_bytes)
    manifest = json.loads(manifest_bytes)
    if manifest["name"] != "@anthropic-ai/claude-code-darwin-arm64" or manifest["version"] != version:
        raise RuntimeError("Unexpected package identity")
    tar_url = manifest["dist"]["tarball"]
    if not tar_url.startswith(REGISTRY + "/@anthropic-ai/"):
        raise RuntimeError("Unexpected artifact origin")
    archive = target / f"official-darwin-arm64-{version}.tgz"
    if not archive.exists():
        archive.write_bytes(read_url(tar_url))
    raw = archive.read_bytes()
    actual = "sha512-" + base64.b64encode(hashlib.sha512(raw).digest()).decode()
    if actual != manifest["dist"]["integrity"]:
        raise RuntimeError("Official registry dist.integrity mismatch")
    extracted = target / "native"
    if not extracted.exists():
        with tarfile.open(archive, "r:gz") as package:
            package.extractall(extracted, filter="data")
    candidates = [path for path in extracted.rglob("claude") if path.is_file()]
    if len(candidates) != 1:
        raise RuntimeError(f"Expected one native executable, found {len(candidates)}")
    binary = candidates[0].resolve()
    with tarfile.open(archive, "r:gz") as package:
        members = [member for member in package.getmembers() if member.isfile() and member.name.endswith("/claude")]
        if len(members) != 1:
            raise RuntimeError("Expected one executable member in verified archive")
        member_stream = package.extractfile(members[0])
        with member_stream:
            archived_binary_hash = hashlib.file_digest(member_stream, "sha256").hexdigest()
    binary_hash = hashlib.sha256(binary.read_bytes()).hexdigest()
    if binary_hash != archived_binary_hash:
        raise RuntimeError("Extracted executable differs from integrity-verified archive")
    binary.chmod(binary.stat().st_mode | 0o100)
    return binary, {"package": manifest["name"], "version": version, "metadata_url": metadata_url,
        "tarball_url": tar_url, "dist_integrity": actual, "integrity_verified": True,
        "registry_signature_verified": False,
        "tarball_sha256": hashlib.sha256(raw).hexdigest(), "tarball_bytes": len(raw),
        "executable_sha256": binary_hash, "executable_matches_verified_archive": True}

def sanitize(value, root):
    if isinstance(value, dict):
        result = {}
        for key, item in value.items():
            if key.lower() in SENSITIVE_HEADERS:
                result[key] = "[REDACTED]"
            elif key == "user_id":
                if isinstance(item, str) and item.startswith("{"):
                    try:
                        metadata = json.loads(item)
                        for field in ("device_id", "account_uuid", "session_id", "parent_session_id"):
                            if field in metadata:
                                metadata[field] = SYNTHETIC_UUID if field != "device_id" else "0" * 64
                        result[key] = json.dumps(metadata, separators=(",", ":"))
                    except ValueError:
                        result[key] = "[REDACTED_METADATA_ID]"
                else:
                    result[key] = "[REDACTED_METADATA_ID]"
            elif key.lower() in {"x-claude-code-session-id", "x-client-request-id", "x-request-id"}:
                result[key] = SYNTHETIC_UUID
            else:
                result[key] = sanitize(item, root)
        return result
    if isinstance(value, list):
        return [sanitize(item, root) for item in value]
    if isinstance(value, str):
        value = value.replace(str(root), "/controlled-runtime").replace(SYNTHETIC_TOKEN, "[REDACTED]")
        value = re.sub(r"/Users/[^/\s<>\"']+", "/controlled-home", value)
        return value
    return value

class Receiver(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def log_message(self, *args):
        pass
    def send_payload(self, status, payload, content_type="application/json"):
        body = payload if isinstance(payload, bytes) else json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Connection", "close")
        self.send_header("request-id", "req_local_official_capture")
        self.end_headers()
        self.wfile.write(body)
        self.wfile.flush()
        self.close_connection = True
    def do_CONNECT(self):
        self.server.records.append({"method": "CONNECT", "path": self.path, "blocked_proxy": True})
        self.send_payload(502, {"error": {"message": "External network disabled by local receiver"}})
    def do_GET(self):
        self.server.records.append({"method": "GET", "path": self.path})
        self.send_payload(200, {"data": [], "has_more": False})
    def do_POST(self):
        raw = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        try:
            body = json.loads(raw)
        except ValueError:
            body = {"unparsed_body_bytes": len(raw)}
        self.server.records.append({"method": "POST", "path": self.path,
            "authentication_observation": {"authorization_scheme": self.headers.get("Authorization", "").split(" ", 1)[0], "api_key_present": "X-Api-Key" in self.headers},
            "headers": dict(self.headers.items()), "body": body})
        if self.path.startswith("http://") or self.path.startswith("https://"):
            return self.send_payload(502, {"error": {"message": "External network disabled by local receiver"}})
        if "count_tokens" in self.path:
            return self.send_payload(200, {"input_tokens": 32})
        if "/messages" not in self.path:
            return self.send_payload(200, {})
        usage = {"input_tokens": 32, "output_tokens": 3, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0}
        message = {"id": "msg_local_official_capture", "type": "message", "role": "assistant", "content": [{"type": "text", "text": "CAPTURE_OK"}], "model": body.get("model", "claude-sonnet-4-6"), "stop_reason": "end_turn", "stop_sequence": None, "usage": usage}
        if not body.get("stream"):
            return self.send_payload(200, message)
        initial = dict(message, content=[], stop_reason=None, usage=dict(usage, output_tokens=0))
        events = [("message_start", {"type": "message_start", "message": initial}),
            ("content_block_start", {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}}),
            ("content_block_delta", {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": "CAPTURE_OK"}}),
            ("content_block_stop", {"type": "content_block_stop", "index": 0}),
            ("message_delta", {"type": "message_delta", "delta": {"stop_reason": "end_turn", "stop_sequence": None}, "usage": {"output_tokens": 3}}),
            ("message_stop", {"type": "message_stop"})]
        payload = "".join(f"event: {event}\ndata: {json.dumps(data)}\n\n" for event, data in events).encode()
        self.send_payload(200, payload, "text/event-stream")

def capture(binary, attribution, timeout):
    with tempfile.TemporaryDirectory(prefix="sub6-official-claude-") as temp:
        root = Path(temp)
        for directory in ("home", "config", "cwd", "tmp"):
            (root / directory).mkdir()
        server = ThreadingHTTPServer(("127.0.0.1", 0), Receiver)
        server.records = []
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        base_url = f"http://127.0.0.1:{server.server_port}"
        env = {"PATH": os.environ.get("PATH", "/usr/bin:/bin"), "HOME": str(root / "home"),
            "TMPDIR": str(root / "tmp"), "LANG": "en_US.UTF-8", "CI": "1", "NO_COLOR": "1",
            "CLAUDE_CONFIG_DIR": str(root / "config"), "CLAUDE_CODE_OAUTH_TOKEN": SYNTHETIC_TOKEN,
            "ANTHROPIC_BASE_URL": base_url, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
            "DISABLE_ERROR_REPORTING": "1", "DISABLE_TELEMETRY": "1", "CLAUDE_CODE_DISABLE_FEEDBACK_SURVEY": "1",
            "HTTP_PROXY": base_url, "HTTPS_PROXY": base_url, "ALL_PROXY": base_url,
            "NO_PROXY": "127.0.0.1,localhost"}
        if attribution == "off":
            env["CLAUDE_CODE_ATTRIBUTION_HEADER"] = "0"
        argv = [str(binary), "--print", "--output-format", "json", "--max-turns", "1", "--no-session-persistence", "--disable-slash-commands", "Reply with the single word CAPTURE_OK."]
        started = time.time()
        try:
            completed = subprocess.run(argv, cwd=root / "cwd", env=env, text=True, capture_output=True, timeout=timeout)
            result = {"exit_code": completed.returncode, "stdout": completed.stdout, "stderr": completed.stderr}
        except subprocess.TimeoutExpired as exc:
            result = {"exit_code": None, "timed_out": True,
                "stdout": (exc.stdout or b"").decode() if isinstance(exc.stdout, bytes) else exc.stdout,
                "stderr": (exc.stderr or b"").decode() if isinstance(exc.stderr, bytes) else exc.stderr}
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=5)
        result.update({"attribution_setting": "unset" if attribution == "default" else "0",
            "elapsed_seconds": round(time.time() - started, 3), "requests": server.records,
            "request_count": len(server.records), "argv": ["<verified-official-executable>"] + argv[1:],
            "environment": {key: ("<isolated-directory>" if key in {"HOME", "TMPDIR", "CLAUDE_CONFIG_DIR"} else "<loopback-receiver>" if key in {"ANTHROPIC_BASE_URL", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"} else "<synthetic-oauth-token>" if key == "CLAUDE_CODE_OAUTH_TOKEN" else value) for key, value in env.items() if key != "PATH"}})
        return sanitize(result, root)

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", default="2.1.330")
    parser.add_argument("--artifact-dir", type=Path, required=True)
    parser.add_argument("--fixtures-dir", type=Path, required=True)
    parser.add_argument("--timeout", type=int, default=120)
    options = parser.parse_args()
    binary, provenance = acquire(options.version, options.artifact_dir)
    options.fixtures_dir.mkdir(parents=True, exist_ok=True)
    captures = []
    for mode in ("default", "off"):
        result = capture(binary, mode, options.timeout)
        destination = options.fixtures_dir / f"claude-{options.version}-attribution-{mode}.json"
        destination.write_text(json.dumps(result, ensure_ascii=False, indent=2) + "\n")
        message_requests = [request for request in result["requests"] if request.get("method") == "POST" and request.get("path", "").startswith("/v1/messages?")]
        captures.append({"mode": mode, "fixture": destination.name,
            "fixture_sha256": hashlib.sha256(destination.read_bytes()).hexdigest(),
            "exit_code": result.get("exit_code"), "request_count": result["request_count"],
            "messages_requests": len(message_requests),
            "completed": result.get("exit_code") == 0 and bool(message_requests) and "CAPTURE_OK" in (result.get("stdout") or "")})
        print(json.dumps({"mode": mode, "exit_code": result.get("exit_code"), "request_count": result["request_count"], "stdout_tail": (result.get("stdout") or "")[-300:], "stderr_tail": (result.get("stderr") or "")[-500:], "fixture": str(destination)}), flush=True)
    provenance.update({"captured_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "capture_scope": "official-cli-local-http-synthetic-token", "upstream_account_used": False,
        "wiggly_absence_is_not_global_absence": True, "registry_manifest_path": "native-npm-manifest.json",
        "captures": captures,
        "sanitization": {"authorization": "redacted", "metadata_ids": "fixed synthetic values", "ephemeral_paths": "controlled-runtime placeholder", "wire_content_length": "original received value; sanitized JSON may differ"}})
    (options.fixtures_dir / "manifest.json").write_text(json.dumps(provenance, indent=2) + "\n")
    if not all(capture["completed"] for capture in captures):
        raise SystemExit("Official CLI capture incomplete; inspect recorded fixtures")

if __name__ == "__main__":
    main()

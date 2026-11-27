import importlib.util
from pathlib import Path
import sys
from types import ModuleType, SimpleNamespace
from typing import Literal
import unittest
from unittest.mock import patch


class FakeSession:
    def __init__(self, **options):
        self.options = options
        self.cookies = SimpleNamespace(jar=[])
        self.calls = []

    def request(self, method, url, **options):
        self.calls.append((method, url, options))
        return SimpleNamespace(status_code=200, headers={}, content=b"OK")

    def get(self, url, **options):
        return self.request("GET", url, **options)

    def close(self):
        pass


def load_tls_transport():
    curl_cffi = ModuleType("curl_cffi")
    requests = ModuleType("curl_cffi.requests")
    requests.Session = FakeSession
    impersonate = ModuleType("curl_cffi.requests.impersonate")
    impersonate.BrowserTypeLiteral = Literal["chrome146"]
    curl_cffi.requests = requests
    modules = {
        "curl_cffi": curl_cffi,
        "curl_cffi.requests": requests,
        "curl_cffi.requests.impersonate": impersonate,
    }
    source = Path(__file__).with_name("tls_transport.py")
    spec = importlib.util.spec_from_file_location("tls_transport_under_test", source)
    module = importlib.util.module_from_spec(spec)
    with patch.dict(sys.modules, modules):
        spec.loader.exec_module(module)
    return module


tls_transport = load_tls_transport()


class WorkerSessionTests(unittest.TestCase):
    def setUp(self):
        self.sessions = []
        self.session_error = None
        factory = SimpleNamespace(Session=self.create_session)
        self.requests_patch = patch.object(tls_transport, "requests", factory)
        self.requests_patch.start()
        self.addCleanup(self.requests_patch.stop)
        self.worker = tls_transport.Worker()

    def create_session(self, **options):
        self.sessions.append(options)
        if self.session_error is not None:
            raise self.session_error
        return FakeSession(**options)

    def test_request_requires_explicit_session_configuration(self):
        with self.assertRaisesRegex(RuntimeError, "TLS session must be configured before sending requests"):
            self.worker.request({"url": "https://auth.example.invalid/"})
        self.assertEqual(self.sessions, [])

    def test_failed_configuration_cannot_create_a_direct_session_on_request(self):
        self.worker.configure("http://proxy.example.invalid:3128", "chrome146")
        self.session_error = RuntimeError("session initialization failed")
        with self.assertRaisesRegex(RuntimeError, "session initialization failed"):
            self.worker.configure("http://replacement.example.invalid:3128", "chrome146")
        self.assertIsNone(self.worker.session)
        self.session_error = None

        with self.assertRaisesRegex(RuntimeError, "TLS session must be configured before sending requests"):
            self.worker.request({"url": "https://auth.example.invalid/"})

        self.assertEqual(len(self.sessions), 2)
        self.assertEqual(self.sessions[-1]["proxy"], "http://replacement.example.invalid:3128")

    def test_explicit_direct_configuration_can_send_requests(self):
        self.worker.configure(None, "chrome146")

        result = self.worker.request({"url": "https://auth.example.invalid/"})

        self.assertEqual(result["status"], 200)
        self.assertEqual(result["body"], "T0s=")
        self.assertEqual(self.sessions, [{"impersonate": "chrome146", "proxy": None}])
        self.assertEqual(len(self.worker.session.calls), 1)

    def test_successful_profile_probe_retains_a_session_for_requests(self):
        probe = self.worker.probe_profiles(
            "https://auth.example.invalid/", profiles=["chrome146"], concurrency=1
        )

        result = self.worker.request({"url": "https://auth.example.invalid/"})

        self.assertEqual(probe, {"profile": "chrome146", "attempts": 1})
        self.assertEqual(result["status"], 200)
        self.assertEqual(len(self.sessions), 1)
        self.assertEqual(len(self.worker.session.calls), 2)

    def test_proxy_timezone_detection_uses_the_configured_session(self):
        self.worker.configure("http://proxy.example.invalid:3128", "chrome146")
        response = SimpleNamespace(
            status_code=200,
            content=b'{"success":true,"timezone":{"id":"America/Los_Angeles"}}',
        )
        self.worker.session.get = lambda url, **options: response

        result = self.worker.detect_proxy_timezone()

        self.assertEqual(result, {"timezoneId": "America/Los_Angeles"})
        self.assertEqual(self.worker.timezone_id, "America/Los_Angeles")

    def test_proxy_timezone_detection_fails_closed_for_invalid_timezone(self):
        self.worker.configure("http://proxy.example.invalid:3128", "chrome146")
        response = SimpleNamespace(
            status_code=200,
            content=b'{"success":true,"timezone":{"id":"Mars/Olympus_Mons"}}',
        )
        self.worker.session.get = lambda url, **options: response

        with self.assertRaisesRegex(RuntimeError, "PROXY_TIMEZONE_DETECTION_FAILED"):
            self.worker.detect_proxy_timezone()

if __name__ == "__main__":
    unittest.main()

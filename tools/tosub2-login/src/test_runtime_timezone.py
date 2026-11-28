import importlib.util
import os
from pathlib import Path
import subprocess
import sys
from types import ModuleType
import unittest
from unittest.mock import patch


RUNTIME_DIR = Path(__file__).with_name("cloudflare-ctf")
sys.path.insert(0, str(RUNTIME_DIR))
from runtime_timezone import node_environment_for_timezone
sys.path.remove(str(RUNTIME_DIR))


def load_module(name, filename, modules=None):
    source = RUNTIME_DIR / filename
    spec = importlib.util.spec_from_file_location(name, source)
    module = importlib.util.module_from_spec(spec)
    injected_modules = modules or {}
    with patch.dict(sys.modules, injected_modules):
        spec.loader.exec_module(module)
    return module


class RuntimeTimezoneTests(unittest.TestCase):
    def test_node_environment_applies_valid_timezone_without_changing_parent(self):
        previous_timezone = os.environ.get("TZ")
        environment = node_environment_for_timezone("America/Los_Angeles")

        self.assertEqual(environment["TZ"], "America/Los_Angeles")
        self.assertEqual(os.environ.get("TZ"), previous_timezone)

        process = subprocess.run(
            [
                "node",
                "-e",
                "console.log(new Date('2020-01-01T12:00:00Z').toString()); "
                "console.log(Intl.DateTimeFormat().resolvedOptions().timeZone);",
            ],
            check=True,
            capture_output=True,
            text=True,
            env=environment,
        )

        self.assertIn("GMT-0800", process.stdout)
        self.assertIn("America/Los_Angeles", process.stdout)

    def test_invalid_timezone_is_rejected_before_launch(self):
        with self.assertRaisesRegex(ValueError, "Invalid IANA timezone"):
            node_environment_for_timezone("Mars/Olympus_Mons")

    def test_cloudflare_runtime_receives_detected_timezone(self):
        module = load_module("cloudflare_solver_timezone_test", "cloudflare_solver.py")
        runtime_input = {"browserIdentity": {"timezoneId": "America/Los_Angeles"}}
        captured = {}

        def fake_popen(*args, **kwargs):
            captured.update(kwargs)
            return object()

        with patch.object(module.subprocess, "Popen", side_effect=fake_popen):
            process, input_file = module.start_runtime(
                module.PARENT_RUNTIME,
                runtime_input,
                "node",
            )
        input_file.close()

        self.assertIsNotNone(process)
        self.assertEqual(captured["env"]["TZ"], "America/Los_Angeles")

    def test_sentinel_runtime_receives_detected_timezone(self):
        curl_cffi = ModuleType("curl_cffi")
        curl_cffi.requests = ModuleType("curl_cffi.requests")
        curl_cffi.requests.Session = object
        module = load_module(
            "sentinel_dynamic_timezone_test",
            "sentinel_dynamic.py",
            {"curl_cffi": curl_cffi, "curl_cffi.requests": curl_cffi.requests},
        )
        captured = {}

        def fake_popen(*args, **kwargs):
            captured.update(kwargs)
            return object()

        with patch.object(module.subprocess, "Popen", side_effect=fake_popen):
            process, input_file = module.start_runtime(
                {"sdkUrl": "https://example.invalid/sdk.js", "sdkSource": ""},
                "https://auth.openai.com/",
                browser_identity={"timezoneId": "America/Los_Angeles"},
                node_command="node",
            )
        input_file.close()

        self.assertIsNotNone(process)
        self.assertEqual(captured["env"]["TZ"], "America/Los_Angeles")


if __name__ == "__main__":
    unittest.main()

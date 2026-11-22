"""Exercise deployment failure gates and rollback without a server or Docker."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest


class DeployTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.sha = 'a' * 40
        self.release = self.root / 'releases' / self.sha
        self.release.mkdir(parents=True)
        self.bin = self.root / 'bin'
        self.bin.mkdir()
        self.env = dict(os.environ, PATH=str(self.bin) + ':' + os.environ['PATH'], TEST_ROOT=str(self.root))
        (self.root / 'compose.json').write_text(json.dumps({'services': {'app': {'image': 'previous'}, 'postgres': {'image': 'untouched'}}}))
        (self.root / 'MIGRATIONS_SHA256').write_text('baseline\n')
        (self.release / 'COMMIT').write_text(self.sha)
        (self.root / 'RUNTIME_SHA256').write_text('runtime\n')
        (self.release / 'RUNTIME_SHA256').write_text('runtime\n')
        (self.release / 'MIGRATIONS_SHA256').write_text('baseline\n')
        (self.release / 'sub2api').write_text('test executable')
        source = Path(__file__).with_name('deploy-sub6api.sh').read_text()
        (self.release / 'deploy.sh').write_text(source.replace('root=/opt/sub6api', 'root=' + str(self.root)))
        (self.root / 'resources').mkdir()
        with tarfile.open(self.release / 'resources.tar.gz', 'w:gz') as archive:
            archive.add(self.root / 'resources', arcname='resources')
        self.stub('flock', 'exit 0')
        self.stub('systemctl', 'echo "$*" >> "$TEST_ROOT/service.log"; if [ "$1" = show ]; then echo "${BACKUP_RESULT:-success}"; fi')
        self.stub('curl', 'echo \'{"status":"ok"}\'')
        self.stub('docker', '''echo "$*" >> "$TEST_ROOT/docker.log"
if [ "$1" = inspect ]; then
  case "$*" in *Health*) echo healthy;; *) echo sha256:previous;; esac
elif [ "$1" = exec ]; then
  if [ "${BAD_BINARY:-}" = 1 ]; then echo badhash; else sha256sum "$TEST_ROOT/releases/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/sub2api"; fi
fi''')
        self.checksums()

    def stub(self, name, body):
        path = self.bin / name
        path.write_text('#!/bin/sh\n' + body + '\n')
        path.chmod(0o755)

    def checksums(self):
        names = ['sub2api', 'deploy.sh', 'COMMIT', 'MIGRATIONS_SHA256', 'RUNTIME_SHA256', 'resources.tar.gz']
        (self.release / 'SHA256SUMS').write_text(''.join(hashlib.sha256((self.release / name).read_bytes()).hexdigest() + '  ' + name + '\n' for name in names))

    def run_deploy(self):
        return subprocess.run(['bash', str(self.release / 'deploy.sh'), str(self.release)], env=self.env, capture_output=True, text=True)

    def test_success_preserves_other_services_and_records_evidence(self):
        result = self.run_deploy()
        self.assertEqual(result.returncode, 0, result.stderr)
        current = json.loads((self.root / 'compose.json').read_text())
        self.assertEqual(current['services']['postgres']['image'], 'untouched')
        self.assertEqual(current['services']['app']['image'], 'sub6api:sha-' + self.sha)
        self.assertIn('tag sha256:previous sub6api:runtime-previous', (self.root / 'docker.log').read_text())
        self.assertTrue((self.root / 'logs' / ('deployment-' + self.sha + '.json')).exists())

    def test_failed_binary_verification_rolls_back(self):
        self.env['BAD_BINARY'] = '1'
        result = self.run_deploy()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(json.loads((self.root / 'compose.json').read_text())['services']['app']['image'], 'previous')
        self.assertEqual((self.root / 'docker.log').read_text().count('up -d --no-deps --pull never app'), 2)

    def test_failed_backup_never_switches(self):
        self.env['BACKUP_RESULT'] = 'failed'
        self.assertNotEqual(self.run_deploy().returncode, 0)
        self.assertNotIn('up -d', (self.root / 'docker.log').read_text())

    def test_runtime_change_never_switches(self):
        (self.release / 'RUNTIME_SHA256').write_text('changed\n')
        self.checksums()
        self.assertNotEqual(self.run_deploy().returncode, 0)
        self.assertFalse((self.root / 'docker.log').exists())

    def test_schema_change_never_switches(self):
        (self.release / 'MIGRATIONS_SHA256').write_text('changed\n')
        self.checksums()
        self.assertNotEqual(self.run_deploy().returncode, 0)
        self.assertFalse((self.root / 'docker.log').exists())


if __name__ == '__main__':
    unittest.main()

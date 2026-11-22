import importlib.util
import io
import os
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('receiver', Path(__file__).with_name('ssh-receiver.py'))
r = importlib.util.module_from_spec(spec)
spec.loader.exec_module(r)

class ReceiverTest(unittest.TestCase):
    def test_rejects_arbitrary_commands(self):
        for command in ['sh', 'upload ../escape', 'deploy ' + 'a' * 40 + '; id', '']:
            with patch.dict(os.environ, SSH_ORIGINAL_COMMAND=command), self.assertRaises(ValueError):
                r.main()

    def test_rejects_links_and_paths_before_extracting(self):
        for name in ['../escape', 'sub2api']:
            with tempfile.TemporaryDirectory() as tmp:
                data = io.BytesIO()
                with tarfile.open(fileobj=data, mode='w') as tar:
                    member = tarfile.TarInfo(name)
                    member.type = tarfile.SYMTYPE
                    member.linkname = '/etc/passwd'
                    tar.addfile(member)
                stdin = type('Input', (), {'buffer': io.BytesIO(data.getvalue())})()
                with patch.object(r, 'ROOT', Path(tmp)), patch.object(r.sys, 'stdin', stdin), patch.dict(os.environ, SSH_ORIGINAL_COMMAND='upload ' + 'a' * 40), self.assertRaises(ValueError):
                    r.main()
                self.assertEqual(list(Path(tmp).iterdir()), [])

if __name__ == '__main__':
    unittest.main()

#!/usr/bin/env python3
"""Forced SSH command: receive a bounded artifact or deploy its exact SHA."""
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile

ROOT = Path('/opt/sub6api/releases')
FILES = {'sub2api', 'deploy.sh', 'COMMIT', 'MIGRATIONS_SHA256', 'RUNTIME_SHA256', 'resources.tar.gz', 'SHA256SUMS'}

def main():
    match = re.fullmatch(r'(upload|deploy) ([a-f0-9]{40})', os.environ.get('SSH_ORIGINAL_COMMAND', ''))
    if not match:
        raise ValueError('Only upload/deploy with a full commit SHA is allowed')
    action, sha = match.groups()
    destination = ROOT / sha
    if action == 'deploy':
        subprocess.run(['bash', str(destination / 'deploy.sh'), str(destination)], check=True)
        return
    ROOT.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix='.upload-', dir=ROOT) as staging:
        stage = Path(staging)
        archive = stage / 'artifact.tar'
        with archive.open('wb') as output:
            total = 0
            while chunk := sys.stdin.buffer.read(1024 * 1024):
                total += len(chunk)
                if total > 300 * 1024 * 1024:
                    raise ValueError('Artifact too large')
                output.write(chunk)
        with tarfile.open(archive) as tar:
            members = tar.getmembers()
            if len(members) != len(FILES) or {m.name for m in members} != FILES or any(not m.isfile() for m in members):
                raise ValueError('Unexpected archive members')
            if sum(m.size for m in members) > 300 * 1024 * 1024:
                raise ValueError('Expanded artifact too large')
            for member in members:
                with tar.extractfile(member) as source, (stage / member.name).open('wb') as output:
                    shutil.copyfileobj(source, output)
        archive.unlink()
        if (stage / 'COMMIT').read_text().strip() != sha:
            raise ValueError('Artifact commit mismatch')
        subprocess.run(['sha256sum', '-c', 'SHA256SUMS'], cwd=stage, check=True)
        if destination.exists():
            if any((destination / name).read_bytes() != (stage / name).read_bytes() for name in FILES):
                raise ValueError('Release already exists with different bytes')
        else:
            os.rename(stage, destination)

if __name__ == '__main__':
    main()

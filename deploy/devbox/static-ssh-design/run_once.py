#!/usr/bin/env python3
import hashlib
import json
import os
from pathlib import Path
import signal
import subprocess
import sys

CALLER_SHA256 = '1a197d5f21ffb54278fcf2cd72a1b474afb46839c5670853d0c3343c480f3e27'
LEAF_SHA256 = '1deee84a5e77170f8c5d9eeb877a867a8083e6876d6f8e68c6de99123daebb80'


def invoke(argv, run=subprocess.Popen):
    child = None
    try:
        child = run(argv, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
        return child.wait(timeout=90)
    finally:
        if child is not None:
            try:
                os.killpg(child.pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
            try:
                child.wait(timeout=3)
            except subprocess.TimeoutExpired:
                pass
            try:
                os.killpg(child.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            child.wait(timeout=3)


def main():
    try:
        if len(sys.argv) != 2:
            raise ValueError()
        root = Path(__file__).resolve().parents[1]
        for filename, digest in (('pricing-static-observer-ssh.py', CALLER_SHA256), ('pricing-static-observer.py', LEAF_SHA256)):
            if hashlib.sha256((root / filename).read_bytes()).hexdigest() != digest:
                raise ValueError()
        raise ValueError()
    except BaseException:
        print(json.dumps({'status': 'DESIGN_DISABLED_NO_REMOTE_CALL'}))
        return 1


if __name__ == '__main__':
    raise SystemExit(main())

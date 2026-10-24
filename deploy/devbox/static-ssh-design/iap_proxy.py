#!/usr/bin/env python3
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import sys


def command(path):
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077:
        raise ValueError()
    plan = json.loads(path.read_bytes())
    if set(plan) != {'gcloud', 'gcloud_sha256', 'instance', 'project', 'zone'}:
        raise ValueError()
    for key in ('instance', 'project', 'zone'):
        if not isinstance(plan[key], str) or not re.fullmatch('[a-z][a-z0-9-]{1,62}', plan[key]):
            raise ValueError()
    binary = Path(plan['gcloud'])
    if not binary.is_absolute() or not re.fullmatch('[a-f0-9]{64}', plan['gcloud_sha256']):
        raise ValueError()
    if hashlib.sha256(binary.read_bytes()).hexdigest() != plan['gcloud_sha256']:
        raise ValueError()
    return [str(binary), 'compute', 'start-iap-tunnel', plan['instance'], '22', '--listen-on-stdin', '--project=' + plan['project'], '--zone=' + plan['zone'], '--verbosity=none', '--quiet']


def main():
    try:
        if len(sys.argv) != 2:
            raise ValueError()
        argv = command(Path(sys.argv[1]))
        with open(os.devnull, 'wb') as sink:
            os.dup2(sink.fileno(), 2)
        os.execv(argv[0], argv)
    except BaseException:
        return 1


if __name__ == '__main__':
    raise SystemExit(main())

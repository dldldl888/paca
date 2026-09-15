#!/usr/bin/env python3
"""Launch native Hermes in one conversation's macOS filesystem/process sandbox."""
import argparse
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import sys


def checked_directory(path):
    path = Path(path)
    if not path.is_absolute() or '..' in path.parts:
        raise ValueError('runtime paths must be absolute without traversal')
    for part in reversed([path, *path.parents]):
        if part.is_symlink():
            raise ValueError('symlink in trusted runtime path')
        if part.exists() and not part.is_dir():
            raise ValueError('runtime ancestor is not a directory')
    path.mkdir(parents=True, exist_ok=True, mode=0o700)
    return path


def write_private(directory, name, content):
    # Anchor each write and reject a model-created final-component symlink/hardlink.
    directory_fd = os.open(directory, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        fd = os.open(name, os.O_WRONLY | os.O_CREAT | os.O_NOFOLLOW | os.O_NONBLOCK, 0o600, dir_fd=directory_fd)
        try:
            info = os.fstat(fd)
            if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1:
                raise ValueError('unsafe runtime configuration file')
            os.fchmod(fd, 0o600)
            os.ftruncate(fd, 0)
            with os.fdopen(os.dup(fd), 'wb') as stream:
                stream.write(content)
        finally:
            os.close(fd)
    finally:
        os.close(directory_fd)


def is_within(path, parent):
    if not parent.exists():
        return path.is_relative_to(parent)
    # Resolve follows symlinks but does not normalize case on macOS filesystems.
    return any(part.exists() and part.samefile(parent) for part in (path, *path.parents))


def reject_runtime_overlap(root, trusted_paths):
    for path in trusted_paths:
        for resolved in (Path(path).absolute(), Path(path).resolve(strict=True)):
            if is_within(root, resolved) or is_within(resolved, root):
                raise ValueError('private runtime root overlaps trusted program or system paths')


def build_launch(root, key, template, python, source):
    """Return native command/environment/cwd from trusted configuration and opaque identity."""
    if sys.platform != 'darwin':
        raise RuntimeError('private Hermes mode requires the validated macOS sandbox')
    if not re.fullmatch(r'[a-f0-9]{64}', key):
        raise ValueError('invalid private runtime identity')
    root = Path(root)
    source = Path(source).resolve(strict=True)
    python = Path(python).absolute()
    readable = [source, '/System', '/usr', '/bin', '/sbin', '/Library/Apple']
    reject_runtime_overlap(root.resolve(), [*readable, python])
    interpreter_root = Path(subprocess.check_output([str(python), '-I', '-c', 'import sys; print(sys.base_prefix)'], env={'PATH': '/usr/bin:/bin'}, cwd='/', text=True).strip())
    reject_runtime_overlap(root.resolve(), [interpreter_root])
    readable.extend((interpreter_root, interpreter_root.resolve(strict=True)))
    root = checked_directory(root)
    template = checked_directory(template)
    if is_within(template, root / 'runtimes'):
        raise ValueError('trusted configuration cannot live in model-writable runtimes')
    base = checked_directory(root / 'runtimes' / key)
    directories = {name: checked_directory(base / name) for name in ('home', 'workspace', 'tmp', 'pycache')}
    for name in ('config.json', '.env'):
        if (template / name).is_symlink() or not (template / name).is_file():
            raise ValueError('missing or unsafe trusted configuration')
    config = json.loads((template / 'config.json').read_text())
    config['terminal'] = dict(config.get('terminal', {}), backend='local', cwd=str(directories['workspace']))
    config['mcp_servers'] = {}
    config['memory'] = dict(config.get('memory', {}), memory_enabled=True, user_profile_enabled=True)
    dotenv = (template / '.env').read_text()
    for line in dotenv.splitlines():
        if not line.strip() or line.lstrip().startswith('#'):
            continue
        name = line.split('=', 1)[0].strip()
        if '=' not in line or name.startswith('PACA_') or not re.fullmatch(r'(?:[A-Z][A-Z0-9_]*_)?(?:API_KEY|BASE_URL)', name):
            raise ValueError('trusted model environment contains a non-model variable')
    write_private(directories['home'], 'config.yaml', json.dumps(config).encode())
    write_private(directories['home'], '.env', dotenv.encode())

    quote = lambda path: json.dumps(str(path), ensure_ascii=False)
    profile = ['(version 1)', '(deny default)', '(allow process-fork)', '(allow process-exec)',
               '(allow signal (target self) (target same-sandbox))', '(allow process-info* (target self) (target children))',
               '(allow sysctl-read)', '(allow system-socket)',
               '(allow network-outbound (require-all (remote tcp "*:443") (require-not (remote ip "localhost:*"))))',
               '(allow network-outbound (literal "/private/var/run/mDNSResponder"))',
               '(allow file-read-metadata)', '(allow file-read* (literal "/"))',
               '(allow mach-lookup (global-name "com.apple.trustd.agent") (global-name "com.apple.SecurityServer") (global-name "com.apple.system.opendirectoryd.libinfo"))']
    profile.extend('(allow file-read* (subpath ' + quote(path) + '))' for path in readable)
    profile.extend('(allow file-read* file-write* (subpath ' + quote(path) + '))' for path in directories.values())
    profile.append('(allow file-read* file-write* (literal "/dev/null") (literal "/dev/tty") (literal "/dev/stdout") (literal "/dev/stderr"))')
    profile.append('(allow file-read* (literal "/dev/random") (literal "/dev/urandom") (literal "/private/etc/localtime") (literal "/private/etc/hosts") (literal "/private/etc/resolv.conf") (literal "/private/etc/ssl/cert.pem"))')
    profiles = checked_directory(root / 'profiles')
    write_private(profiles, key + '.sb', ('\n'.join(profile) + '\n').encode())
    env = {'PATH': '/usr/bin:/bin:/usr/sbin:/sbin', 'HERMES_HOME': str(directories['home']),
           'TMPDIR': str(directories['tmp']), 'PYTHONPYCACHEPREFIX': str(directories['pycache']),
           'PYTHONPATH': str(source), 'PYTHONUNBUFFERED': '1', 'LANG': 'en_US.UTF-8'}
    command = ['/usr/bin/sandbox-exec', '-f', str(profiles / (key + '.sb')), str(python), '-m', 'acp_adapter']
    return command, env, str(directories['workspace'])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root', type=Path, required=True)
    parser.add_argument('--template', type=Path, required=True)
    parser.add_argument('--python', type=Path, required=True)
    parser.add_argument('--source', type=Path, required=True)
    args = parser.parse_args()
    command, environment, cwd = build_launch(args.root, os.environ.get('PACA_PRIVATE_KEY', ''), args.template, args.python, args.source)
    os.chdir(cwd)
    os.execve(command[0], command, environment)


if __name__ == '__main__':
    main()

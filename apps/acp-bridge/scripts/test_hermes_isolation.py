"""Real macOS/Hermes isolation checks. Requires the installed native Hermes runtime."""
import json
import os
from pathlib import Path
import secrets
import shutil
import socket
import subprocess
import sys
import tempfile
import unittest

from launch_hermes import build_launch

HERMES_SOURCE = Path(os.environ.get('PACA_HERMES_SOURCE', '/Users/xxp/.hermes/hermes-agent'))
HERMES_PYTHON = Path(os.environ.get('PACA_HERMES_PYTHON', str(HERMES_SOURCE / 'venv/bin/python')))


class TestHermesIsolation(unittest.TestCase):
    def launch_pair(self):
        temporary = tempfile.TemporaryDirectory(prefix='paca-native-isolation-')
        self.addCleanup(temporary.cleanup)
        root = Path(temporary.name).resolve()
        template = root / 'template'
        template.mkdir()
        (template / 'config.json').write_text(json.dumps({'model': {'provider': 'deepseek', 'default': 'deepseek-v4-flash'}}))
        (template / '.env').write_text('')
        launches = [build_launch(root, key * 64, template, HERMES_PYTHON, HERMES_SOURCE) for key in ('a', 'b')]
        return root, template, launches

    def run_script(self, launch, code, *arguments):
        command, env, cwd = launch
        result = subprocess.run(command[:-2] + ['-c', code, *map(str, arguments)], cwd=cwd, env=env, capture_output=True, text=True, timeout=45)
        self.assertEqual(result.returncode, 0, result.stderr[-2500:])
        return result.stdout

    def test_us010_native_tools(self):
        if os.environ.get('PACA_ISOLATION_CHILD'):
            import asyncio
            from tools.session_search_tool import session_search
            from tools.memory_tool import MemoryStore, memory_tool
            from acp_adapter.session import SessionManager
            from acp_adapter.server import HermesACPAgent
            own, other = os.environ['PACA_OWN_MARKER'], os.environ['PACA_OTHER_MARKER']
            self.assertIn(own, session_search(session_id='session-b'))
            self.assertNotIn(other, session_search(session_id='session-a', current_session_id='session-b'))
            self.assertEqual(json.loads(session_search(query=other, current_session_id='session-b'))['results'], [])
            self.assertIsNone(SessionManager().get_session('session-a'))
            self.assertIsNone(asyncio.run(HermesACPAgent().load_session(cwd=os.getcwd(), session_id='session-a')))
            store = MemoryStore()
            store.load_from_disk()
            self.assertIn(own, '\n'.join(store.memory_entries))
            self.assertNotIn(other, '\n'.join(store.memory_entries))
            self.assertTrue(json.loads(memory_tool(action='add', content=own + '-more', store=store))['success'])
            return
        root, template, launches = self.launch_pair()
        # Direct call binds the actual launch configuration to this native behavior test.
        launches[1] = build_launch(root, 'b' * 64, template, HERMES_PYTHON, HERMES_SOURCE)
        markers = ['A-' + secrets.token_hex(8), 'B-' + secrets.token_hex(8)]
        seed = """import sys
from hermes_state import SessionDB
from tools.memory_tool import MemoryStore, memory_tool
session, marker = sys.argv[1:]
db = SessionDB()
db.create_session(session, 'acp')
db.append_message(session, 'user', marker)
store = MemoryStore(); store.load_from_disk()
assert __import__('json').loads(memory_tool(action='add', content=marker, store=store))['success']
"""
        for launch, sid, marker in zip(launches, ('session-a', 'session-b'), markers):
            self.run_script(launch, seed, sid, marker)
        command, env, cwd = launches[1]
        shutil.copy2(__file__, Path(cwd) / Path(__file__).name)
        shutil.copy2(Path(__file__).with_name('launch_hermes.py'), Path(cwd) / 'launch_hermes.py')
        env.update(PACA_ISOLATION_CHILD='1', PACA_OWN_MARKER=markers[1], PACA_OTHER_MARKER=markers[0])
        result = subprocess.run(command[:-2] + ['-m', 'unittest', '-v', 'test_hermes_isolation.TestHermesIsolation.test_us010_native_tools'], cwd=cwd, env=env, capture_output=True, text=True, timeout=60)
        self.assertEqual(result.returncode, 0, result.stderr[-3000:])
        self.assertIn('OK', result.stderr)

    def test_us010_filesystem(self):
        root, template, launches = self.launch_pair()
        command, env, cwd = build_launch(root, 'b' * 64, template, HERMES_PYTHON, HERMES_SOURCE)
        secret = secrets.token_hex(12)
        sibling = Path(launches[0][2]) / 'private.txt'
        sibling.write_text(secret)
        personal = root / 'personal.txt'
        personal.write_text(secret)
        personal_file = tempfile.NamedTemporaryFile(prefix='.paca-isolation-check-', dir=Path.home())
        self.addCleanup(personal_file.close)
        personal_file.write(secret.encode())
        personal_file.flush()
        link = Path(cwd) / 'escape'
        link.symlink_to(sibling)
        process = subprocess.Popen([sys.executable, '-c', 'import time;time.sleep(60)'], env={'PACA_PROCESS_SENTINEL': secret})
        self.addCleanup(lambda: (process.terminate(), process.wait()))
        code = """import os,sys,subprocess
from pathlib import Path
secret,pid,*paths=sys.argv[1:]
local=Path('own.txt');local.write_text('own');assert local.read_text()=='own'
for target in paths:
 try: Path(target).read_text()
 except PermissionError: pass
 else: raise AssertionError('cross-boundary read allowed')
 result=subprocess.run(['/bin/cat',target],capture_output=True,text=True)
 assert result.returncode != 0 and secret not in result.stdout
try:
 result=subprocess.run(['/bin/ps','eww','-p',pid],capture_output=True,text=True)
 assert secret not in result.stdout
except PermissionError: pass
assert not any(k.startswith('PACA_ACP_') for k in os.environ)
print('filesystem and process checks passed')
"""
        self.assertIn('checks passed', self.run_script((command, env, cwd), code, secret, process.pid, sibling, personal, personal_file.name, link, Path(cwd) / '../../' / ('a' * 64) / 'workspace/private.txt'))
        for bad in ('../a', 'a/b', ''):
            with self.assertRaises(ValueError):
                build_launch(root, bad, template, HERMES_PYTHON, HERMES_SOURCE)
        linked_root = root / 'linked'
        linked_root.symlink_to(root / 'runtimes', target_is_directory=True)
        with self.assertRaises(ValueError):
            build_launch(linked_root, 'b' * 64, template, HERMES_PYTHON, HERMES_SOURCE)

    def test_rejects_readable_root_overlap(self):
        root, template, _ = self.launch_pair()
        interpreter_root = Path(subprocess.check_output([str(HERMES_PYTHON), '-I', '-c', 'import sys;print(sys.base_prefix)'], text=True).strip()).resolve()
        for parent in (HERMES_SOURCE, interpreter_root):
            with tempfile.TemporaryDirectory(prefix='.paca-overlap-', dir=parent) as nested:
                with self.subTest(readable_parent=parent), self.assertRaises(ValueError):
                    build_launch(Path(nested), 'c' * 64, template, HERMES_PYTHON, HERMES_SOURCE)
                self.assertFalse((Path(nested) / 'runtimes').exists())
        nested_source = root / 'program'
        nested_source.mkdir()
        linked_source = root / 'source-alias'
        linked_source.symlink_to(HERMES_SOURCE, target_is_directory=True)
        with self.assertRaises(ValueError):
            build_launch(root, 'c' * 64, template, HERMES_PYTHON, nested_source)
        # Canonical source aliases must still reject a root underneath the target.
        with self.assertRaises(ValueError):
            build_launch(HERMES_SOURCE, 'c' * 64, template, HERMES_PYTHON, linked_source)
        nested_python = root / 'python'
        nested_python.symlink_to(HERMES_PYTHON)
        with self.assertRaises(ValueError):
            build_launch(root, 'c' * 64, template, nested_python, HERMES_SOURCE)
        for system in ('/System', '/usr', '/bin', '/sbin', '/Library/Apple', '/'):
            with self.subTest(system=system), self.assertRaises(ValueError):
                build_launch(Path(system), 'c' * 64, template, HERMES_PYTHON, HERMES_SOURCE)

    def test_rejects_model_created_fifo_without_blocking(self):
        root, template, launches = self.launch_pair()
        code = """import sys
from pathlib import Path
from launch_hermes import build_launch
try: build_launch(Path(sys.argv[1]), 'b'*64, *map(Path, sys.argv[2:]))
except (OSError, ValueError): print('unsafe configuration rejected')
else: raise AssertionError('FIFO accepted')
"""
        for name in ('config.yaml', '.env'):
            with self.subTest(name=name):
                self.run_script(launches[1], "import os,sys;from pathlib import Path;p=Path(os.environ['HERMES_HOME'])/sys.argv[1];p.unlink();os.mkfifo(p)", name)
                try:
                    result = subprocess.run([sys.executable, '-c', code, str(root), str(template), str(HERMES_PYTHON), str(HERMES_SOURCE)],
                                            cwd=Path(__file__).parent, capture_output=True, text=True, timeout=3)
                except subprocess.TimeoutExpired:
                    self.fail('launcher blocked on model-created FIFO')
                finally:
                    (Path(launches[1][1]['HERMES_HOME']) / name).unlink()
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn('unsafe configuration rejected', result.stdout)
                launches[1] = build_launch(root, 'b' * 64, template, HERMES_PYTHON, HERMES_SOURCE)

    def test_rejects_case_alias_overlap(self):
        root, template, launches = self.launch_pair()
        source = HERMES_SOURCE.resolve()
        alias = source.with_name(source.name.swapcase())
        if alias == source or not alias.exists() or not alias.samefile(source):
            self.skipTest('case-alias regression requires a case-insensitive native filesystem')
        interpreter_root = Path(subprocess.check_output([str(HERMES_PYTHON), '-I', '-c', 'import sys;print(sys.base_prefix)'], text=True).strip()).resolve()
        for parent in (source, interpreter_root):
            alias = parent.with_name(parent.name.swapcase())
            self.assertTrue(alias.samefile(parent))
            with tempfile.TemporaryDirectory(prefix='.paca-case-', dir=parent) as nested:
                aliased_root = alias / Path(nested).name
                for candidate in (aliased_root, aliased_root / 'not-created'):
                    with self.subTest(parent=parent, candidate=candidate):
                        with self.assertRaises(ValueError):
                            build_launch(candidate, 'c' * 64, template, HERMES_PYTHON, source)
                        self.assertFalse((candidate / 'runtimes').exists())
                        self.assertFalse((candidate / 'profiles').exists())
        with tempfile.TemporaryDirectory(prefix='paca-CaseParent-') as outer:
            parent = Path(outer).resolve()
            alias = parent.with_name(parent.name.swapcase())
            self.assertTrue(alias.samefile(parent))
            nested_source = parent / 'program'
            nested_source.mkdir()
            with self.subTest(direction='runtime contains source'):
                with self.assertRaises(ValueError):
                    build_launch(alias, 'c' * 64, template, HERMES_PYTHON, nested_source)
                self.assertFalse((parent / 'runtimes').exists())
                self.assertFalse((parent / 'profiles').exists())
        # A model-writable template cannot become trusted through another spelling.
        self.run_script(launches[0], "import os;from pathlib import Path;p=Path(os.environ['HERMES_HOME'])/'template';p.mkdir();(p/'config.json').write_text('{}');(p/'.env').write_text('')")
        model_template = Path(launches[0][1]['HERMES_HOME']) / 'template'
        alias = root.with_name(root.name.swapcase())
        self.assertTrue(alias.samefile(root))
        with self.subTest(boundary='trusted template'), self.assertRaises(ValueError):
            build_launch(alias, 'c' * 64, model_template, HERMES_PYTHON, source)
        fresh_root = root / 'new-private-root'
        self.assertFalse(fresh_root.exists())
        launch = build_launch(fresh_root, 'c' * 64, template, HERMES_PYTHON, source)
        self.run_script(launch, "from pathlib import Path;p=Path('own');p.write_text('own');assert p.read_text()=='own'")

    def test_native_terminal_cleans_descendants_only(self):
        root, _, launches = self.launch_pair()
        sentinel = subprocess.Popen([sys.executable, '-c', 'import time;time.sleep(60)'])
        self.addCleanup(lambda: (sentinel.terminate(), sentinel.wait()))
        command, env, cwd = launches[0]
        sibling = subprocess.Popen(command[:-2] + ['-c', 'import time;time.sleep(60)'], env=env, cwd=cwd)
        self.addCleanup(lambda: (sibling.terminate(), sibling.wait()))
        command, env, cwd = launches[1]
        separate_launch = subprocess.Popen(command[:-2] + ['-c', 'import time;time.sleep(60)'], env=env, cwd=cwd)
        self.addCleanup(lambda: (separate_launch.terminate(), separate_launch.wait()))
        code = """import os,signal,subprocess,sys,time,shlex
from pathlib import Path
from tools.environments.local import LocalEnvironment
for outside in map(int,sys.argv[1:]):
 for sig in (0, signal.SIGTERM):
  try: os.kill(outside,sig)
  except PermissionError: pass
  else: raise AssertionError('outside process signal allowed')
child=subprocess.Popen([sys.executable,'-c','import time;time.sleep(2)'])
try:
 child.terminate()
 assert child.wait(timeout=1) == -signal.SIGTERM
finally: child.wait(timeout=3)
terminal=LocalEnvironment(cwd=os.getcwd(),timeout=1)
try:
 result=terminal.execute('printf native-terminal-ok')
 assert result['returncode']==0 and 'native-terminal-ok' in result['output'], result
 # Exercise the actual terminal timeout path and verify the grandchild stops.
 sleeper=Path('sleeper.py')
 sleeper.write_text("import os,time,sys,subprocess;from pathlib import Path\\nif len(sys.argv)==1: subprocess.Popen([sys.executable,__file__,'escaped'],start_new_session=True)\\nname='escaped' if len(sys.argv)>1 else 'grandchild'\\nPath(name+'.pid').write_text(str(os.getpid()))\\ntime.sleep(2)\\nPath(name+'-leaked-after-timeout').write_text('alive')")
 result=terminal.execute(shlex.quote(sys.executable)+' '+str(sleeper)+' & wait',timeout=1)
 assert result['returncode'] != 0, result
 assert Path('grandchild.pid').exists(), result
 assert Path('escaped.pid').exists(), result
 time.sleep(2)
 assert not list(Path('.').glob('*-leaked-after-timeout')), 'terminal left descendant running'
finally: terminal.cleanup()
print('own descendants cleaned; outside process protected')
"""
        self.assertIn('outside process protected', self.run_script(launches[1], code, sentinel.pid, sibling.pid, separate_launch.pid))
        self.assertIsNone(sentinel.poll(), 'outside sentinel was terminated')
        self.assertIsNone(sibling.poll(), 'sibling conversation was terminated')
        self.assertIsNone(separate_launch.poll(), 'separate sandbox instance was terminated')

    def test_native_system_ca_bundle(self):
        _, _, launches = self.launch_pair()
        self.run_script(launches[1], "import ssl;context=ssl.create_default_context(cafile='/private/etc/ssl/cert.pem');assert context.get_ca_certs()")

    def test_native_dns_resolution(self):
        _, _, launches = self.launch_pair()
        self.run_script(launches[1], "import socket;assert socket.getaddrinfo('example.com',443,type=socket.SOCK_STREAM)")

    def test_rejects_local_network_services(self):
        _, _, launches = self.launch_pair()
        tcp = socket.socket()
        self.addCleanup(tcp.close)
        tcp.bind(('127.0.0.1', 0))
        tcp.listen()
        tcp6 = socket.socket(socket.AF_INET6)
        self.addCleanup(tcp6.close)
        tcp6.bind(('::1', 0))
        tcp6.listen()
        unix = socket.socket(socket.AF_UNIX)
        self.addCleanup(unix.close)
        socket_path = 'service.sock'
        original_cwd = os.getcwd()
        try:
            os.chdir(launches[1][2])
            unix.bind(socket_path)
        finally:
            os.chdir(original_cwd)
        unix.listen()
        code = """import socket,sys
for family,address in ((socket.AF_INET,('127.0.0.1',int(sys.argv[1]))),(socket.AF_INET6,('::1',int(sys.argv[2]))),(socket.AF_UNIX,sys.argv[3])):
 with socket.socket(family) as client:
  client.settimeout(1)
  try: client.connect(address)
  except OSError as error: assert isinstance(error,PermissionError), (family,address,error)
  else: raise AssertionError('local service connection was not sandbox-denied')
print('local network services denied')
"""
        self.assertIn('local network services denied', self.run_script(launches[1], code, tcp.getsockname()[1], tcp6.getsockname()[1], socket_path))


if __name__ == '__main__':
    unittest.main()

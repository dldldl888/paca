"""Bind scenarios to real test bodies and execute native Go/Bun/unittest tests."""
import argparse
import ast
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys


class SpecError(ValueError):
    pass


PYTHON_TEST_RUNNER = """
import sys, unittest
suite = unittest.defaultTestLoader.loadTestsFromName(sys.argv[1])
observed = set()
def profile(frame, event, argument):
    if event == 'call' and frame.f_code.co_name == sys.argv[2]:
        observed.add(frame.f_code)
sys.setprofile(profile)
try:
    result = unittest.TextTestRunner(verbosity=2).run(suite)
finally:
    sys.setprofile(None)
if not observed:
    print('bound SUT was not executed', file=sys.stderr)
sys.exit(0 if result.wasSuccessful() and result.testsRun and not result.skipped and observed else 1)
"""


def selected_stories(stories, current, final):
    if final:
        return stories
    if current not in {s['id'] for s in stories}:
        raise SpecError('select an existing --story or use --all')
    return [s for s in stories if s.get('passes') or s['id'] == current]


def native(root, cwd, command, timeout=600):
    """Use an installed runtime, or the dedicated local test image, without a shell."""
    if not shutil.which(command[0]):
        image = 'oven/bun:1-alpine' if command[0] == 'bun' else 'paca-recovery-test:local'
        command = ['docker', 'run', '--rm', '-v', f'{root}:/src', '-v', 'paca-dev_go_build_cache:/root/.cache/go-build', '-w', '/src/' + str(cwd.relative_to(root)), image, *command]
    result = subprocess.run(command, cwd=cwd, capture_output=True, text=True, timeout=timeout)
    if result.returncode:
        # Tests use synthetic fixtures. Never execute this gate against private runtime files.
        raise SpecError(f'{command[0]} exited {result.returncode}:\n{(result.stdout + result.stderr)[-6000:]}')
    return result.stdout + result.stderr


def python_binding(path, node, sut):
    body = ast.parse(path.read_text())
    for name in node.split('.'):
        body = next((n for n in reversed(body.body) if isinstance(n, (ast.ClassDef, ast.FunctionDef, ast.AsyncFunctionDef)) and n.name == name), None)
        if body is None:
            raise SpecError(f'missing Python test {node}')
    if not isinstance(body, ast.FunctionDef) or not body.name.startswith('test'):
        raise SpecError(f'not a Python test: {node}')
    def direct_nodes(node):
        for child in ast.iter_child_nodes(node):
            if isinstance(child, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef, ast.Lambda)):
                continue
            yield child
            yield from direct_nodes(child)
    nodes = list(direct_nodes(body))
    calls = [n.func for n in nodes if isinstance(n, ast.Call)]
    names = [n.id if isinstance(n, ast.Name) else n.attr if isinstance(n, ast.Attribute) else '' for n in calls]
    if sut not in names or not (any(n.startswith('assert') for n in names) or any(isinstance(n, ast.Assert) for n in nodes)):
        raise SpecError(f'{node}: test body must call {sut} and assert behavior')


def validate_scenario(root, scenario):
    sid, sut = scenario['id'], scenario['sut']
    try:
        filename, node = scenario['test'].split('::', 1)
        path = (root / filename).resolve()
        path.relative_to(root)
        if not path.is_file():
            raise SpecError(f'missing test file {filename}')
        if path.suffix == '.py':
            python_binding(path, node, sut)
            output = native(root, path.parent, [sys.executable, '-c', PYTHON_TEST_RUNNER, path.stem + '.' + node, sut])
            if not re.search(r'Ran [1-9][0-9]* tests?\b', output) or 'skipped=' in output:
                raise SpecError('zero or skipped Python tests')
        elif path.suffix == '.go':
            module = next((p for p in path.parents if (p / 'go.mod').is_file()), None)
            if module is None:
                raise SpecError('test has no Go module')
            package = './' + str(path.parent.relative_to(module))
            metadata = json.loads(native(root, module, ['go', 'list', '-json', package]))
            if path.name not in metadata.get('TestGoFiles', []) + metadata.get('XTestGoFiles', []):
                raise SpecError('bound Go file is excluded from the active build')
            rel = str(path.relative_to(module))
            helper = os.path.relpath(root / 'scripts/ralph/check_go_binding.go', module)
            native(root, module, ['go', 'run', helper, '--', rel, node, sut])
            output = native(root, module, ['go', 'test', '-json', package, '-run', '^' + re.escape(node) + '$', '-count=1', '-timeout=180s'])
            records = [json.loads(line) for line in output.splitlines() if line.startswith('{')]
            if not any(r.get('Test') == node and r.get('Action') == 'pass' for r in records) or any(r.get('Action') in ('fail', 'skip') for r in records):
                raise SpecError('missing, failed or skipped Go test')
        elif path.suffix in ('.ts', '.js'):
            module = next((p for p in path.parents if (p / 'package.json').is_file()), None)
            if module is None:
                raise SpecError('test has no Bun package')
            helper = os.path.relpath(root / 'scripts/ralph/check_ts_binding.cjs', module)
            native(root, module, ['bun', helper, str(path.relative_to(module)), node, sut])
            output = native(root, module, ['bun', 'test', str(path.relative_to(module)), '--test-name-pattern', '^' + re.escape(node) + '$'])
            if not re.search(r'\b[1-9][0-9]* pass\b', output) or re.search(r'\b[1-9][0-9]* (skip|fail)\b', output):
                raise SpecError('missing, failed or skipped Bun test')
        else:
            raise SpecError(f'unsupported native test language: {path.suffix}')
    except (ValueError, KeyError, SyntaxError, StopIteration, OSError, subprocess.TimeoutExpired) as exc:
        raise SpecError(f'{sid}: {exc}') from exc
    return sid


def validate_story(root, story):
    root = Path(root).resolve()
    scenarios = [s for s in story.get('scenarios', []) if s.get('active', True)]
    if not scenarios:
        raise SpecError(f"{story['id']}: no scenarios")
    return [validate_scenario(root, s) for s in scenarios]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--spec', type=Path, default=Path(__file__).with_name('prd.json'))
    parser.add_argument('--story')
    parser.add_argument('--all', action='store_true')
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[2]
    spec = json.loads(args.spec.read_text())
    failures, passed = [], []
    for story in selected_stories(spec['userStories'], args.story, args.all):
        try:
            passed.extend(validate_story(root, story))
        except SpecError as exc:
            failures.append(str(exc))
    print(json.dumps({'passed_scenarios': passed, 'failures': failures}, ensure_ascii=False, indent=2))
    return int(bool(failures))


if __name__ == '__main__':
    try:
        sys.exit(main())
    except SpecError as exc:
        print(str(exc), file=sys.stderr)
        sys.exit(1)

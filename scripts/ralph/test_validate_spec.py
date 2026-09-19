import tempfile
import os
import unittest
from pathlib import Path
import validate_spec
from validate_spec import SpecError, selected_stories, validate_story


class TestSpecGate(unittest.TestCase):
    def test_legacy_story_without_scenarios_is_skipped(self):
        self.assertEqual(validate_story(Path(__file__).resolve().parents[2], {"id": "legacy"}), [])

    def test_playwright_report_paths_are_relative_to_its_test_directory(self):
        module = Path("/repo/package")
        bound = module / "browser/bound.spec.ts"
        spec = {"title": "bound", "file": "bound.spec.ts", "tests": []}
        report = {"config": {"rootDir": str(module / "browser")}, "suites": [{"title": "bound.spec.ts", "specs": [spec]}]}
        self.assertEqual(validate_spec.playwright_matches(report, "bound", bound, module), [spec])

    def fixture(self, body):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        root = Path(temporary.name)
        (root / "test_case.py").write_text("import unittest\ndef target(value): return value + 1\nclass Case(unittest.TestCase):\n    def test_behavior(self):\n" + body + "\n")
        story = {"id": "US-X", "passes": False, "scenarios": [{"id": "S-X", "sut": "target", "test": "test_case.py::Case.test_behavior"}]}
        return root, story

    def vitest_fixture(self, body, suffix=".ts", sut="target"):
        root = Path(__file__).resolve().parents[2]
        temporary = tempfile.TemporaryDirectory(prefix=".vitest-spec-", dir=root)
        self.addCleanup(temporary.cleanup)
        module = Path(temporary.name)
        (module / "package.json").write_text('{"devDependencies":{"typescript":"*","vitest":"*"}}')
        (module / "node_modules").symlink_to(
            os.path.relpath(root / "apps/web/node_modules", module)
        )
        (module / "sut.tsx").write_text(
            'import React from "react";\n'
            'export function target(){ return 1; }\n'
            'export function Widget(){ return <button>ready</button>; }\n'
        )
        bound = module / ("sample.test" + suffix)
        bound.write_text(body)
        story = {"id": "fixture", "scenarios": [{
            "id": "vitest", "sut": sut,
            "test": str(bound.relative_to(root)) + "::group bound",
        }]}
        return root, story

    def test_us015_reject_missing_or_empty(self):
        for body in ("        pass", "        # target(1)\n        self.assertTrue(True)", "        text = 'target(1)'\n        self.assertTrue(text)"):
            root, story = self.fixture(body)
            with self.assertRaises(SpecError):
                validate_story(root, story)
        root, story = self.fixture("        self.assertEqual(target(1), 2)")
        story["scenarios"][0]["test"] = "missing.py::Case.test_behavior"
        with self.assertRaises(SpecError):
            validate_story(root, story)
        story["scenarios"][0]["test"] = "test_case.py::Case.test_absent"
        with self.assertRaises(SpecError):
            validate_story(root, story)
        for body in ("        self.skipTest('not available')\n        self.assertEqual(target(1), 2)", "        self.assertEqual(target(1), 99)"):
            root, story = self.fixture(body)
            with self.assertRaises(SpecError):
                validate_story(root, story)

    def test_unexecuted_bodies_do_not_count(self):
        root, story = self.fixture("        self.assertEqual(target(1), 2)")
        path = root / "test_case.py"
        original = path.read_text()
        for source in (
            original.replace("def test_behavior", "async def test_behavior"),
            original + "    def test_behavior(self): self.assertTrue(True)\n",
            original.replace("        self.assertEqual", "        def unused():\n            self.assertEqual"),
            original.replace("        self.assertEqual", "        if False:\n            self.assertEqual"),
        ):
            path.write_text(source)
            with self.assertRaises(SpecError):
                validate_story(root, story)

    def test_native_binding_boundaries(self):
        root = Path(__file__).resolve().parents[2]
        temporary = tempfile.TemporaryDirectory(prefix=".spec-fixture-", dir=root)
        self.addCleanup(temporary.cleanup)
        directory = Path(temporary.name)
        go = directory / "go"
        go.mkdir()
        (go / "go.mod").write_text("module example.com/specfixture\n\ngo 1.26.4\n")
        bound = go / "bound_test.go"
        (go / "actual_test.go").write_text('package fixture\nimport "testing"\nfunc TestBound(t *testing.T) {}\n')
        bound.write_text('//go:build spec_excluded\n\npackage fixture\nimport "testing"\nfunc target() int { panic("executed") }\nfunc TestBound(t *testing.T) { if target()!=1 { t.Fatal("wrong") } }\n')
        story = {"id": "fixture", "scenarios": [{"id": "go-build", "sut": "target", "test": str(bound.relative_to(root)) + "::TestBound"}]}
        with self.assertRaises(SpecError):
            validate_story(root, story)
        (go / "actual_test.go").unlink()
        bound.write_text('package fixture\nimport "testing"\nfunc target() int { panic("executed") }\nfunc TestBound(t *testing.T) { unused := func(){ if target()!=1 { t.Fatal("wrong") } }; _ = unused }\n')
        with self.assertRaises(SpecError):
            validate_story(root, story)
        bound.write_text('package fixture\nimport "testing"\nfunc target() int { return 1 }\nfunc TestBound(t *testing.T) { t.Run("nested", func(t *testing.T){ if target()!=1 { t.Fatal("wrong") } }) }\n')
        self.assertEqual(validate_story(root, story), ["go-build"])
        ts = directory / "ts"
        ts.mkdir()
        (ts / "package.json").write_text("{}")
        (ts / "node_modules").symlink_to(os.path.relpath(root / "services/realtime/node_modules", ts))
        bound = ts / "sample.test.ts"
        story["scenarios"] = [{"id": "bun", "sut": "target", "test": str(bound.relative_to(root)) + "::group bound"}]
        bound.write_text('import {describe,test,expect} from "bun:test"; function target(){return 1;} describe("group",()=>{ test("bound",()=>{expect(target()).toBe(1);}); });\n')
        self.assertEqual(validate_story(root, story), ["bun"])
        for body in ('expect(() => target()).toThrow();', '(() => { expect(target()).toBe(1); })()'):
            target = 'throw new Error("expected");' if 'toThrow' in body else 'return 1;'
            bound.write_text('import {describe,test,expect} from "bun:test"; function target(){' + target + '} describe("group",()=>{ test("bound",()=>{' + body + '}); });\n')
            self.assertEqual(validate_story(root, story), ["bun"])
        bound.write_text('import {describe,test,expect} from "bun:test"; function target(){throw new Error("executed");} describe("group",()=>{ test("bound",()=>{function unused(){expect(target()).toBe(1);} }); });\n')
        with self.assertRaises(SpecError):
            validate_story(root, story)

    def test_vitest_binding_and_execution(self):
        valid = (
            'import {describe,it,expect} from "vitest";\n'
            'import {target} from "./sut";\n'
            'describe("group",()=>{it("bound",()=>{expect(target()).toBe(1);});});\n'
        )
        root, story = self.vitest_fixture(valid)
        self.assertEqual(validate_story(root, story), ["vitest"])

        story["scenarios"][0]["test"] = story["scenarios"][0]["test"].replace(
            "::group bound", "::group absent"
        )
        with self.assertRaises(SpecError):
            validate_story(root, story)

        rejected = (
            'import {describe,it,expect} from "vitest";\n'
            'import {target} from "./sut";\n'
            'describe("group",()=>{it("bound",()=>{%s});});\n'
        )
        for body in (
            'expect(true).toBe(true);',
            '/* target(); */ expect(true).toBe(true);',
            'function unused(){expect(target()).toBe(1);} expect(true).toBe(true);',
            'target(); expect(true);',
            'if(false){expect(target()).toBe(1);} expect(true).toBe(true);',
        ):
            root, story = self.vitest_fixture(rejected % body)
            with self.assertRaises(SpecError):
                validate_story(root, story)

        local_stub = (
            'import {describe,it,expect} from "vitest";\n'
            'function target(){return 1;}\n'
            'describe("group",()=>{it("bound",()=>{expect(target()).toBe(1);});});\n'
        )
        root, story = self.vitest_fixture(local_stub)
        with self.assertRaises(SpecError):
            validate_story(root, story)

        mocked_sut = valid.replace(
            'import {describe,it,expect}', 'import {describe,it,expect,vi}'
        ).replace(
            'describe("group"',
            'vi.mock("./sut",()=>({target:()=>1}));\ndescribe("group"',
        )
        root, story = self.vitest_fixture(mocked_sut)
        with self.assertRaises(SpecError):
            validate_story(root, story)

        skipped = valid.replace('it("bound"', 'it.skip("bound"')
        root, story = self.vitest_fixture(skipped)
        with self.assertRaises(SpecError):
            validate_story(root, story)

        failed = valid.replace("toBe(1)", "toBe(2)")
        root, story = self.vitest_fixture(failed)
        with self.assertRaises(SpecError):
            validate_story(root, story)

    def test_vitest_jsx_binding(self):
        body = (
            '// @vitest-environment jsdom\n'
            'import React from "react";\n'
            'import {render,screen} from "@testing-library/react";\n'
            'import {describe,it,expect} from "vitest";\n'
            'import {Widget} from "./sut";\n'
            'const renderHome=(ui)=>render(ui);\n'
            'describe("group",()=>{it("bound",()=>{'
            'renderHome(<Widget/>); expect(screen.getByRole("button").textContent).toBe("ready");'
            '});});\n'
        )
        for suffix in (".tsx", ".jsx"):
            root, story = self.vitest_fixture(body, suffix, "Widget")
            self.assertEqual(validate_story(root, story), ["vitest"])

    def test_additional_tests_are_required(self):
        root, story = self.fixture("        self.assertEqual(target(1), 2)")
        (root / "second_test.py").write_text(
            "import unittest\ndef second(): return 2\nclass Case(unittest.TestCase):\n"
            "    def test_second(self): self.assertEqual(second(), 2)\n"
        )
        story["scenarios"][0]["additionalTests"] = [{
            "sut": "second", "test": "second_test.py::Case.test_second"
        }]
        self.assertEqual(validate_story(root, story), ["S-X"])
        story["scenarios"][0]["additionalTests"][0]["sut"] = "missing"
        with self.assertRaises(SpecError):
            validate_story(root, story)

    def test_playwright_binding_and_execution(self):
        root = Path(__file__).resolve().parents[2]
        temporary = tempfile.TemporaryDirectory(prefix=".playwright-spec-", dir=root)
        self.addCleanup(temporary.cleanup)
        module = Path(temporary.name)
        (module / "package.json").write_text(
            '{"type":"module","devDependencies":{"@playwright/test":"*"}}'
        )
        (module / "node_modules").symlink_to(
            os.path.relpath(root / "apps/e2e/node_modules", module)
        )
        (module / "playwright.config.ts").write_text(
            'import {defineConfig} from "@playwright/test";\n'
            'export default defineConfig({testDir:".",projects:[{name:"chromium"}]});\n'
        )
        (module / "page.ts").write_text(
            'export class EmployeeWorkbenchPage {'
            'constructor(readonly value:number){} read(){return this.value;} }\n'
        )
        bound = module / "sample.spec.ts"
        bound.write_text(
            'import {test,expect} from "@playwright/test";\n'
            'import {EmployeeWorkbenchPage} from "./page";\n'
            'test.describe("group",()=>{test("bound",()=>{'
            'const page=new EmployeeWorkbenchPage(1); expect(page.read()).toBe(1);'
            '});});\n'
        )
        story = {"id": "fixture", "scenarios": [{
            "id": "playwright", "sut": "EmployeeWorkbenchPage",
            "test": str(bound.relative_to(root)) + "::group bound",
            "config": "playwright.config.ts",
        }]}
        self.assertEqual(validate_story(root, story), ["playwright"])
        story["scenarios"][0]["additionalTests"] = [{
            "sut": "EmployeeWorkbenchPage",
            "test": str(bound.relative_to(root)) + "::group bound",
        }]
        self.assertEqual(validate_story(root, story), ["playwright"])
        story["scenarios"][0].pop("additionalTests")
        bound.write_text(bound.read_text().replace('test("bound"', 'test.skip("bound"'))
        with self.assertRaises(SpecError):
            validate_story(root, story)

    def test_javascript_reports_must_match_the_bound_file(self):
        root = Path("/repo/package")
        bound = root / "bound.test.ts"
        vitest = {"testResults": [{
            "name": str(root / "shadow.test.ts"),
            "assertionResults": [{"fullName": "group bound", "status": "passed"}],
        }]}
        self.assertEqual(
            validate_spec.vitest_assertions(vitest, "group bound", bound, root), []
        )
        vitest["testResults"][0]["name"] = "bound.test.ts"
        self.assertEqual(
            len(validate_spec.vitest_assertions(vitest, "group bound", bound, root)), 1
        )

        playwright = {"suites": [{
            "title": "shadow.spec.ts",
            "specs": [{"title": "bound", "file": "shadow.spec.ts"}],
            "suites": [],
        }]}
        self.assertEqual(
            validate_spec.playwright_matches(playwright, "bound", root / "bound.spec.ts", root), []
        )
        playwright["suites"][0]["title"] = "bound.spec.ts"
        playwright["suites"][0]["specs"][0]["file"] = str(root / "bound.spec.ts")
        self.assertEqual(
            len(validate_spec.playwright_matches(playwright, "bound", root / "bound.spec.ts", root)), 1
        )

    def test_us015_scope_and_binding(self):
        root, story = self.fixture("        self.assertEqual(target(1), 2)")
        self.assertEqual(validate_story(root, story), ["S-X"])
        completed = dict(story, id="US-OLD", passes=True)
        pending = dict(story, id="US-FUTURE")
        self.assertEqual([s["id"] for s in selected_stories([completed, story, pending], "US-X", False)], ["US-OLD", "US-X"])
        self.assertEqual(len(selected_stories([completed, story, pending], None, True)), 3)
        (root / "test_case.py").write_text("import unittest\nclass Case(unittest.TestCase):\n    def test_behavior(self): self.assertTrue(True)\n")
        with self.assertRaises(SpecError):
            validate_story(root, completed)


if __name__ == "__main__":
    unittest.main()

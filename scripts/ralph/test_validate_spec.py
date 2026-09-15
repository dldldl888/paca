import tempfile
import os
import unittest
from pathlib import Path
from validate_spec import SpecError, selected_stories, validate_story


class TestSpecGate(unittest.TestCase):
    def fixture(self, body):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        root = Path(temporary.name)
        (root / "test_case.py").write_text("import unittest\ndef target(value): return value + 1\nclass Case(unittest.TestCase):\n    def test_behavior(self):\n" + body + "\n")
        story = {"id": "US-X", "passes": False, "scenarios": [{"id": "S-X", "sut": "target", "test": "test_case.py::Case.test_behavior"}]}
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
        bound.write_text('import {describe,test,expect} from "bun:test"; function target(){throw new Error("executed");} describe("group",()=>{ test("bound",()=>{function unused(){expect(target()).toBe(1);} }); });\n')
        with self.assertRaises(SpecError):
            validate_story(root, story)

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

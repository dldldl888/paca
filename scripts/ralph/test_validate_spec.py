import tempfile
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

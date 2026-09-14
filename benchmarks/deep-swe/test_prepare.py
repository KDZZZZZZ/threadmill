import importlib.util
import subprocess
import tempfile
import tomllib
import unittest
from pathlib import Path

spec = importlib.util.spec_from_file_location("prepare", Path(__file__).with_name("prepare.py"))
prepare = importlib.util.module_from_spec(spec)
spec.loader.exec_module(prepare)


class PrepareTest(unittest.TestCase):
    def test_collects_uncommitted_files_links_and_deletions_without_changing_git(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            repo = root / "repo"
            repo.mkdir()
            def git(*args):
                return subprocess.check_output(["git", "-C", str(repo), *args])
            git("init", "-q")
            git("config", "user.email", "test@example.test")
            git("config", "user.name", "Test")
            (repo / "old").write_text("old\n")
            git("add", ".")
            git("commit", "-qm", "base")
            base = git("rev-parse", "HEAD").decode().strip()
            index = (repo / ".git/index").read_bytes()
            (repo / "old").unlink()
            (repo / "new").write_text("new\n")
            (repo / "link").symlink_to("new")
            task = root / "task"
            task.mkdir()
            patch = root / "model.patch"
            original = f'''[verifier]
timeout_sec = 1800
[[verifier.collect]]
command = "cd {repo} && git diff --binary {base} HEAD > {patch}"
'''
            (task / "task.toml").write_text(original)
            (task / "tests").mkdir()
            (task / "tests/grader.py").write_text("original grader")
            dest = root / "prepared"
            prepare.prepare_task(task, dest)
            config = tomllib.loads((dest / "task.toml").read_text())
            subprocess.run(config["verifier"]["collect"][0]["command"], shell=True, check=True)
            body = patch.read_text()
            self.assertIn("new file mode 120000", body)
            self.assertIn("deleted file mode", body)
            self.assertIn("+new", body)
            self.assertEqual(git("rev-parse", "HEAD").decode().strip(), base)
            self.assertEqual((repo / ".git/index").read_bytes(), index)
            self.assertEqual((task / "task.toml").read_text(), original)
            self.assertEqual((dest / "tests/grader.py").read_text(), "original grader")
            self.assertEqual(config["verifier"]["timeout_sec"], 1800)
            git("apply", "--check", "--reverse", str(patch))


if __name__ == "__main__":
    unittest.main()

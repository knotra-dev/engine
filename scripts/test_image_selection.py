import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

from ruamel.yaml import YAML


SELECTOR = Path(__file__).with_name("select_images.py")


class ImageSelectionTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.registry = self.root / "published"
        self.registry.write_text("")
        self.output = self.root / "output"
        tools = self.root / "tools"
        tools.mkdir()
        docker = tools / "docker"
        docker.write_text(
            f"#!{sys.executable}\n"
            "import os, pathlib, sys\n"
            "assert sys.argv[1:4] == ['buildx', 'imagetools', 'inspect']\n"
            "published = pathlib.Path(os.environ['TEST_IMAGE_REGISTRY']).read_text().splitlines()\n"
            "sys.exit(0 if sys.argv[4] in published else 1)\n"
        )
        docker.chmod(0o700)
        self.env = {
            **os.environ,
            "PATH": str(tools) + os.pathsep + os.environ["PATH"],
            "GITHUB_OUTPUT": str(self.output),
            "GITHUB_REPOSITORY_OWNER": "Example",
            "GITHUB_EVENT_NAME": "push",
            "GITHUB_REF_TYPE": "branch",
            "IMAGE_VERSION": "main",
            "TEST_IMAGE_REGISTRY": str(self.registry),
        }
        self.git("init", "-q")
        for name in (
            "Dockerfile",
            ".dockerignore",
            ".github/workflows/images.yml",
            "go.mod",
            "cmd/knotra/main.go",
            "internal/adapters/firewall/Dockerfile",
            "examples/starter/hello/pipeline.yaml",
            "README.md",
        ):
            self.write(name)
        self.commit()

    def git(self, *args):
        subprocess.run(["git", *args], cwd=self.root, check=True, capture_output=True)

    def write(self, name):
        path = self.root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(
            path.read_text() + "changed\n" if path.exists() else "initial\n"
        )

    def commit(self):
        self.git("add", ".")
        self.git(
            "-c",
            "user.name=Test",
            "-c",
            "user.email=test@example.invalid",
            "commit",
            "-qm",
            "fixture",
        )

    def select(self, **env):
        self.output.write_text("")
        result = subprocess.run(
            [sys.executable, str(SELECTOR)],
            cwd=self.root,
            env={**self.env, **env},
            capture_output=True,
            text=True,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        outputs = dict(
            line.split("=", 1) for line in self.output.read_text().splitlines()
        )
        self.images = [outputs["engine"], outputs["firewall"]]
        return json.loads(outputs["builds"])

    def publish(self):
        self.registry.write_text("\n".join(self.images) + "\n")

    def test_missing_images_build_and_unrelated_changes_reuse_them(self):
        self.assertEqual(self.select(), ["engine", "firewall"])
        self.publish()
        for name in (
            "README.md",
            "scripts/check_contract.py",
            "internal/model_test.go",
        ):
            self.write(name)
        self.commit()
        self.assertEqual(self.select(), [])

    def test_unpublished_code_changes_survive_a_later_docs_push(self):
        self.select()
        self.publish()
        self.write("cmd/knotra/main.go")
        self.commit()
        self.assertEqual(self.select(), ["engine"])
        self.write("README.md")
        self.commit()
        self.assertEqual(self.select(), ["engine"])

    def test_build_inputs_select_the_affected_images(self):
        for name, expected in (
            ("internal/adapters/firewall/Dockerfile", ["firewall"]),
            ("examples/starter/hello/pipeline.yaml", ["engine"]),
            ("internal/store/schema.sql", ["engine"]),
            ("docker/entrypoint.sh", ["engine"]),
            (".dockerignore", ["engine", "firewall"]),
            (".github/workflows/images.yml", ["engine", "firewall"]),
            ("scripts/select_images.py", ["engine", "firewall"]),
        ):
            with self.subTest(name=name):
                self.select()
                self.publish()
                self.write(name)
                self.commit()
                self.assertEqual(self.select(), expected)

    def test_manual_and_tag_runs_force_refresh(self):
        self.select()
        self.publish()
        for env in (
            {"GITHUB_EVENT_NAME": "workflow_dispatch"},
            {"GITHUB_REF_TYPE": "tag"},
        ):
            with self.subTest(env=env):
                self.assertEqual(self.select(**env), ["engine", "firewall"])

    def test_publishing_reuses_cached_or_joins_checked_architectures(self):
        workflow = YAML(typ="safe").load(
            (SELECTOR.parent.parent / ".github/workflows/images.yml").read_text()
        )
        script = workflow["jobs"]["publish"]["steps"][-1]["run"]
        capture = self.root / "docker-args.json"
        (self.root / "tools/docker").write_text(
            f"#!{sys.executable}\n"
            "import json, os, pathlib, sys\n"
            "pathlib.Path(os.environ['TEST_DOCKER_ARGS']).write_text(json.dumps(sys.argv[1:]))\n"
        )
        image = "ghcr.io/example/engine"
        for built, expected in (
            ("false", [f"{image}:inputs-tested"]),
            (
                "true",
                [
                    "--tag",
                    f"{image}:inputs-tested",
                    f"{image}:build-123-1-amd64",
                    f"{image}:build-123-1-arm64",
                ],
            ),
        ):
            with self.subTest(built=built):
                subprocess.run(
                    ["bash", "-e", "-o", "pipefail", "-c", script],
                    env={
                        **self.env,
                        "TEST_DOCKER_ARGS": str(capture),
                        "IMAGE": image,
                        "TAGS": f"{image}:sha-current\n{image}:main",
                        "SOURCE_IMAGE": f"{image}:inputs-tested",
                        "BUILT": built,
                        "GITHUB_RUN_ID": "123",
                        "GITHUB_RUN_ATTEMPT": "1",
                    },
                    check=True,
                )
                self.assertEqual(
                    json.loads(capture.read_text()),
                    [
                        "buildx",
                        "imagetools",
                        "create",
                        "--tag",
                        f"{image}:sha-current",
                        "--tag",
                        f"{image}:main",
                        *expected,
                    ],
                )


if __name__ == "__main__":
    unittest.main()

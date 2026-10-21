"""Select images whose tested build inputs are not already published."""

import hashlib
import json
import os
import subprocess


# Keep these paths aligned with Dockerfile copies and Go embeds.
INPUTS = {
    "engine": [
        "Dockerfile",
        "go.mod",
        "go.sum",
        "LICENSE",
        "assets.go",
        "compose.yaml",
        "compose.container.yaml",
        "cmd",
        "internal",
        "schemas",
        "docker",
        "examples/local/profile.yaml",
        "examples/starter/assets.go",
        "examples/starter/hello",
        "examples/starter/research-dossier",
        "examples/starter/tic-tac-toe",
        "examples/starter/publication",
        "scripts/test-container.py",
        ":(exclude)internal/adapters/firewall",
        ":(glob,exclude)internal/**/*_test.go",
        ":(glob,exclude)cmd/**/*_test.go",
    ],
    "firewall": ["internal/adapters/firewall"],
}


def main():
    force = (
        os.environ["GITHUB_EVENT_NAME"] == "workflow_dispatch"
        or os.environ["GITHUB_REF_TYPE"] == "tag"
    )
    owner = os.environ["GITHUB_REPOSITORY_OWNER"].lower()
    builds = []
    with open(os.environ["GITHUB_OUTPUT"], "a", encoding="utf-8") as output:
        for image, paths in INPUTS.items():
            tree = subprocess.check_output(
                [
                    "git",
                    "ls-files",
                    "--stage",
                    "--",
                    ".dockerignore",
                    ".github/workflows/images.yml",
                    "scripts/select_images.py",
                    *paths,
                ]
            )
            digest = hashlib.sha256(
                tree + os.environ["IMAGE_VERSION"].encode()
            ).hexdigest()
            reference = f"ghcr.io/{owner}/{image}:inputs-{digest}"
            build = (
                force
                or subprocess.run(
                    ["docker", "buildx", "imagetools", "inspect", reference],
                    stdout=subprocess.DEVNULL,
                    stderr=subprocess.DEVNULL,
                    check=False,
                ).returncode
                != 0
            )
            if build:
                builds.append(image)
            print(f"{image}: {'build' if build else 'reuse'} {reference}")
            output.write(f"{image}={reference}\n")
        output.write(f"builds={json.dumps(builds)}\n")


if __name__ == "__main__":
    main()

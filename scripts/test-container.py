#!/usr/bin/env python3
"""Smoke-test a built engine image with Docker and Python, without Go or models."""

import hashlib
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import tempfile
import time
import uuid


def docker(*args, check=True, timeout=180):
    result = subprocess.run(
        ["docker", *map(str, args)], capture_output=True, text=True, timeout=timeout
    )
    if check and result.returncode:
        raise RuntimeError(result.stderr.strip() or result.stdout.strip())

    return result


def wait_for(probe, timeout=60):
    deadline = time.monotonic() + timeout
    last_error = None
    while time.monotonic() < deadline:
        try:
            result = probe()
            if result:
                return result
        except (RuntimeError, json.JSONDecodeError) as error:
            last_error = error
        time.sleep(0.5)

    raise TimeoutError(f"Container readiness timed out: {last_error}")


def check_entrypoint(image, work, name):
    print("Checking helper staging and CLI-only entrypoint behavior…", flush=True)
    run = ["run", "--rm", "--name", name, "--mount", f"type=bind,src={work},dst={work}"]
    cli_state = work / "cli-only"
    docker(*run, "--env", f"KNOTRA_DATA_DIR={cli_state}", image, "--version")
    assert not cli_state.exists(), "CLI-only commands created engine state"

    relative = docker(
        *run, "--env", "KNOTRA_DATA_DIR=relative", image, "serve", "--help", check=False
    )
    assert (
        relative.returncode != 0 and "absolute host path" in relative.stderr
    ), relative

    state = work / "entrypoint"
    stage = [*run, "--env", f"KNOTRA_DATA_DIR={state}", image, "serve", "--help"]
    docker(*stage)
    helpers = list((state / "bin").glob("sandbox-helper-*"))
    assert len(helpers) == 1, helpers
    helper = helpers[0]
    original = helper.read_bytes()
    assert helper.name == "sandbox-helper-" + hashlib.sha256(original).hexdigest()
    assert helper.stat().st_mode & 0o111 == 0o111, "Sandbox UID cannot execute helper"
    modified = helper.stat().st_mtime_ns

    docker(*stage)
    assert helper.read_bytes() == original and helper.stat().st_mtime_ns == modified

    docker(
        *run,
        "--entrypoint",
        "/bin/sh",
        image,
        "-c",
        'printf altered > "$1"',
        "--",
        helper,
    )
    altered = docker(*stage, check=False)
    assert altered.returncode != 0 and "helper is altered" in altered.stderr, altered
    assert helper.read_bytes() == b"altered", "Altered helper was silently replaced"

    docker(
        *run,
        "--env",
        f"KNOTRA_DATA_DIR={cli_state}",
        "--env",
        "KNOTRA_SANDBOX_HELPER=/custom/helper",
        image,
        "serve",
        "--help",
    )
    assert not cli_state.exists(), "Configured helper triggered automatic staging"


def main():
    image = os.environ.get(
        "KNOTRA_TEST_CONTAINER_IMAGE", "knotra-engine:container-test"
    )
    postgres_image = (
        "postgres:18.6-alpine@sha256:"
        "77f585114c32fbca283dc835b0596f4e52b51b4c6662d7810b2f4084f60a1873"
    )
    docker("image", "inspect", image)
    if docker("image", "inspect", "python:3.13-alpine", check=False).returncode:
        print("Pulling the Python sandbox image…", flush=True)
        docker("pull", "python:3.13-alpine", timeout=600)

    checkout = Path(__file__).resolve().parent.parent
    staging = checkout / ".knotra"
    staging.mkdir(exist_ok=True)
    work = Path(tempfile.mkdtemp(prefix="container-smoke-", dir=staging))
    prefix = f"knotra-smoke-{uuid.uuid4().hex[:12]}"
    postgres, engine, client = (
        prefix + suffix for suffix in ("-db", "-engine", "-cli")
    )
    engine_id = None
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        endpoint = f"http://127.0.0.1:{listener.getsockname()[1]}"

    def cli(*args):
        return json.loads(
            docker(
                "run",
                "--rm",
                "--name",
                client,
                "--network",
                "host",
                "--user",
                f"{os.getuid()}:{os.getgid()}",
                "--mount",
                f"type=bind,src={work},dst={work}",
                image,
                "--endpoint",
                endpoint,
                "--state-dir",
                work / "client",
                "--json",
                *args,
            ).stdout
        )

    def info():
        return json.loads(
            docker(
                "exec",
                engine,
                "knotra",
                "--endpoint",
                endpoint,
                "--state-dir",
                work / "probe",
                "--json",
                "info",
                timeout=10,
            ).stdout
        )

    def sandboxes():
        return docker(
            "ps", "-aq", "--filter", f"label=io.knotra.engine={engine_id}"
        ).stdout.split()

    try:
        check_entrypoint(image, work, client)
        print("Starting disposable PostgreSQL and engine containers…", flush=True)
        docker(
            "run",
            "-d",
            "--name",
            postgres,
            "--publish",
            "127.0.0.1::5432",
            "--env",
            "POSTGRES_DB=knotra",
            "--env",
            "POSTGRES_USER=knotra",
            "--env",
            "POSTGRES_PASSWORD=container-smoke",
            postgres_image,
        )
        wait_for(
            lambda: docker(
                "exec",
                postgres,
                "pg_isready",
                "-h",
                "127.0.0.1",
                "-U",
                "knotra",
                "-d",
                "knotra",
            ).returncode
            == 0
        )
        database = json.loads(docker("inspect", postgres).stdout)[0]
        port = database["NetworkSettings"]["Ports"]["5432/tcp"][0]["HostPort"]
        database_url = (
            f"postgres://knotra:container-smoke@127.0.0.1:{port}/knotra?sslmode=disable"
        )
        docker(
            "run",
            "-d",
            "--name",
            engine,
            "--network",
            "host",
            "--mount",
            "type=bind,src=/var/run/docker.sock,dst=/var/run/docker.sock",
            "--mount",
            f"type=bind,src={work},dst={work}",
            "--env",
            "DOCKER_HOST=unix:///var/run/docker.sock",
            "--env",
            f"KNOTRA_DATABASE_URL={database_url}",
            "--env",
            f"KNOTRA_DATA_DIR={work / 'state'}",
            image,
            "serve",
            "--listen",
            endpoint.removeprefix("http://"),
            "--profile",
            "/etc/knotra/profile.yaml",
        )
        engine_id = wait_for(info)["engineId"]
        assert cli("info")["engineId"] == engine_id

        shutil.copytree(checkout / "docker" / "smoke", work / "package")
        source = work / "source.txt"
        source.write_text("container smoke input\n")
        uploaded = cli("artifacts", "upload", source, "--media-type", "text/plain")[
            "artifact"
        ]
        print("Running a code-only pipeline through the containerized CLI…", flush=True)
        result = cli(
            "run",
            work / "package" / "pipeline.yaml",
            "--profile",
            "local",
            "--input",
            "number=21",
            "--artifact",
            f"source={uploaded['id']}",
            "--wait",
        )
        run = result["run"]
        assert run["status"] == "succeeded", run
        assert run["outputs"]["answer"] == 42, run
        assert len(run["artifacts"]) == 1, run
        artifact = run["artifacts"][0]
        expected = b"container smoke input\nanswer=42\n"
        assert artifact["name"] == "report.txt", artifact
        assert artifact["size"] == len(expected), artifact
        assert artifact["sha256"] == hashlib.sha256(expected).hexdigest(), artifact
        download = work / "report.txt"
        cli("artifacts", "download", artifact["id"], "--output", download)
        assert (
            download.stat().st_uid == os.getuid()
        ), "CLI download is not owned by the caller"
        assert download.read_bytes() == expected
        wait_for(lambda: not sandboxes(), timeout=15)

        print(
            "Checking engine, run, artifact and CLI receipt persistence after restart…",
            flush=True,
        )
        docker("restart", "--time", "15", engine)
        assert wait_for(info)["engineId"] == engine_id
        restored = cli("status", run["id"])["run"]
        assert restored["status"] == "succeeded", restored
        assert restored["outputs"] == run["outputs"], restored

        cli("artifacts", "download", artifact["id"], "--output", work / "restored.txt")
        assert (work / "restored.txt").read_bytes() == expected

        receipts = cli("operations", "list")["items"]
        assert any(
            item["id"] == result["operationId"] and item["status"] == "completed"
            for item in receipts
        ), receipts

        print(
            "Container smoke passed: mounts, execution, artifacts, cleanup and persistence.",
            flush=True,
        )
    except BaseException:
        for name in (engine, postgres):
            logs = docker("logs", "--tail", "60", name, check=False)
            print(f"{name}:\n{logs.stdout}{logs.stderr}", flush=True)
        raise
    finally:
        docker("rm", "--force", "--volumes", client, engine, postgres, check=False)
        if engine_id:
            remaining = sandboxes()
            if remaining:
                docker("rm", "-f", *remaining, check=False)
        # Container-created files can belong to root on Linux hosts.
        docker(
            "run",
            "--rm",
            "--name",
            prefix + "-cleanup",
            "--entrypoint",
            "/bin/sh",
            "--mount",
            f"type=bind,src={work},dst=/cleanup",
            image,
            "-c",
            "rm -rf /cleanup/state /cleanup/client /cleanup/probe /cleanup/entrypoint /cleanup/cli-only",
            check=False,
        )
        shutil.rmtree(work)


if __name__ == "__main__":
    main()

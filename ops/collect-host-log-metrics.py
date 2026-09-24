#!/usr/bin/env python3
"""Write size-only host log metrics for the portal's storage dashboard."""

import argparse
import datetime
import glob
import json
import os
import pathlib
import stat
import subprocess
import tempfile


CONTAINERS = {
    "portal": "cliproxy-portal",
    "cpamp": "cpamp-cpa-manager-plus-1",
    "cpa": "cpamp-cli-proxy-api-1",
}


def regular_file_size(path):
    try:
        info = os.stat(path)
    except OSError:
        return None
    return info.st_size if stat.S_ISREG(info.st_mode) else None


def docker_log_size(container):
    try:
        result = subprocess.run(
            ["docker", "inspect", "--format", "{{.LogPath}}", container],
            check=True,
            capture_output=True,
            text=True,
            timeout=10,
        )
    except (OSError, subprocess.SubprocessError):
        return None
    path = result.stdout.strip()
    return regular_file_size(path) if path else None


def collect(cpa_log_dir):
    log_dir = pathlib.Path(cpa_log_dir)
    response_sizes = [regular_file_size(path) for path in glob.glob(str(log_dir / "v1-responses-*.log"))]
    return {
        "generated_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
        "cpa_main_log_bytes": regular_file_size(log_dir / "main.log"),
        "cpa_response_log_bytes": (
            sum(response_sizes)
            if log_dir.is_dir() and all(size is not None for size in response_sizes)
            else None
        ),
        "container_log_bytes": {label: docker_log_size(name) for label, name in CONTAINERS.items()},
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cpa-log-dir", required=True)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    output = pathlib.Path(args.output)
    payload = json.dumps(collect(args.cpa_log_dir), ensure_ascii=False, separators=(",", ":")) + "\n"
    fd, temporary = tempfile.mkstemp(prefix=".host-log-metrics-", dir=output.parent)
    try:
        os.fchmod(fd, 0o644)
        with os.fdopen(fd, "w", encoding="utf-8") as stream:
            stream.write(payload)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, output)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


if __name__ == "__main__":
    main()

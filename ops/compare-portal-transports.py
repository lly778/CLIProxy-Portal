"""Controlled Windows DIRECT vs VLESS comparison with a header-only server observer.

Uses an isolated, temporary Mihomo core bound to the physical Wi-Fi interface.
Does not change the installed Clash instance, portal, firewall or public listeners.
Requires Python stdlib, existing Mihomo/curl, and the existing codex-vps SSH alias.
"""
import argparse
import base64
import http.client
import json
import os
from pathlib import Path
import queue
import re
import shlex
import socket
import subprocess
import tempfile
import threading
import time


SERVER = "212.135.210.56"


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def target_node():
    file = Path(os.environ["APPDATA"]) / "io.github.clash-verge-rev.clash-verge-rev" / "clash-verge.yaml"
    blocks = []
    current = []
    inside = False
    for line in file.read_text(encoding="utf-8-sig").splitlines():
        if line.strip() == "proxies:":
            inside = True
            continue
        if not inside:
            continue
        if re.match(r"^[A-Za-z][\w-]*:", line):
            break
        if re.match(r"^\s*-\s*name:", line):
            if current:
                blocks.append(current)
            current = [line]
        elif current:
            current.append(line)
    if current:
        blocks.append(current)
    for block in blocks:
        fields = {}
        for line in block:
            match = re.match(r"^\s*(?:-\s*)?([\w-]+):\s*(.*)$", line)
            if match:
                fields[match[1]] = match[2].strip().strip("\"'")
        if fields.get("type") == "vless" and fields.get("server") == SERVER and fields.get("port") == "443":
            if fields.get("dialer-proxy"):
                raise RuntimeError("The node uses another dialer proxy; cannot isolate that path automatically.")
            return fields["name"], "\n".join(block)
    raise RuntimeError("No matching VLESS node in the installed runtime configuration.")


def portal_key():
    code = """
import hashlib,pathlib,sqlite3,urllib.request,json
secret=pathlib.Path('/root/cpa-manager-plus/secrets/cpa-management-key').read_text().strip()
request=urllib.request.Request('http://127.0.0.1:18319/v0/management/api-keys',headers={'Authorization':'Bearer '+secret})
with urllib.request.urlopen(request,timeout=10) as response: keys=json.load(response)['api-keys']
connection=sqlite3.connect('file:/root/cliproxy-portal/data/portal.db?mode=ro',uri=True)
for key in keys:
 if isinstance(key,str) and connection.execute("SELECT 1 FROM api_keys WHERE key_hash=? AND status='active' LIMIT 1",(hashlib.sha256(key.encode()).hexdigest(),)).fetchone():
  print(key);break
else: raise RuntimeError('No active portal key')
"""
    result = subprocess.run(["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "codex-vps", "python3", "-"],
                            input=code, capture_output=True, text=True, timeout=30)
    if result.returncode or len(result.stdout.strip().splitlines()) != 1:
        raise RuntimeError("Unable to retrieve a diagnostic API Key.")
    return result.stdout.strip()


def config_value(value):
    if "\n" in value or "\r" in value:
        raise ValueError("Newline in curl configuration")
    return '"' + value.replace("\\", "\\\\").replace('"', '\\"') + '"'


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--repeat", type=int, default=3)
    parser.add_argument("--interface", default="Wi-Fi")
    parser.add_argument("--timeout", type=int, default=30)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    output = Path(args.output).resolve()
    output.mkdir(parents=True, exist_ok=False)
    run_id = time.strftime("%Y%m%d-%H%M%S")
    name, block = target_node()
    key = portal_key()
    direct_port, vless_port, control_port = free_port(), free_port(), free_port()
    if len({direct_port, vless_port, control_port}) != 3:
        raise RuntimeError("Port allocation collided.")
    report = {"run_id": run_id, "interface": args.interface,
              "method": "isolated Mihomo sockets bound to physical interface, with forced per-listener outbound",
              "vless_node": name, "server": SERVER, "payload_retained": False,
              "installed_clash_config_modified": False, "server_config_modified": False,
              "client_results": [], "server_messages": []}
    messages = queue.Queue()
    observer = core = None

    def save():
        (output / "comparison.json").write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding="utf-8")

    def wait_message(predicate, timeout=15):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            try:
                message = messages.get(timeout=deadline - time.monotonic())
            except queue.Empty:
                break
            if predicate(message):
                return message
        raise RuntimeError("Timed out waiting for the server observer.")

    try:
        with tempfile.TemporaryDirectory(prefix="portal-transport-compare-") as temporary:
            temporary_path = Path(temporary)
            config = {"mode": "rule", "allow-lan": False, "log-level": "warning",
                      "interface-name": args.interface, "external-controller": "127.0.0.1:" + str(control_port),
                      "tun": {"enable": False}, "dns": {"enable": False},
                      "listeners": [
                          {"name": "diagnostic-direct", "type": "socks", "listen": "127.0.0.1", "port": direct_port, "udp": False, "proxy": "DIRECT"},
                          {"name": "diagnostic-vless", "type": "socks", "listen": "127.0.0.1", "port": vless_port, "udp": False, "proxy": name}],
                      "rules": ["MATCH,DIRECT"]}
            # JSON scalars are valid YAML. Append just the original target proxy block.
            config_path = temporary_path / "config.yaml"
            text = "\n".join(field + ": " + json.dumps(value, ensure_ascii=False) for field, value in config.items())
            config_path.write_text(text + "\nproxies:\n" + block + "\n", encoding="utf-8")
            executable = Path("C:/Program Files/Clash Verge/verge-mihomo-alpha.exe")
            checked = subprocess.run([str(executable), "-t", "-d", temporary, "-f", str(config_path)],
                                     capture_output=True, timeout=20, creationflags=subprocess.CREATE_NO_WINDOW)
            if checked.returncode:
                raise RuntimeError("Isolated Mihomo config validation failed (temporary logs are not exported).")
            with (temporary_path / "core.log").open("wb") as core_log:
                core = subprocess.Popen([str(executable), "-d", temporary, "-f", str(config_path)],
                                        stdout=core_log, stderr=core_log, creationflags=subprocess.CREATE_NO_WINDOW)
                try:
                    deadline = time.monotonic() + 15
                    while True:
                        if core.poll() is not None:
                            raise RuntimeError("Isolated Mihomo process exited during startup.")
                        try:
                            connection = http.client.HTTPConnection("127.0.0.1", control_port, timeout=1)
                            connection.request("GET", "/configs")
                            response = connection.getresponse()
                            state = json.loads(response.read())
                            connection.close()
                            if response.status == 200:
                                break
                        except (OSError, ValueError):
                            pass
                        if time.monotonic() > deadline:
                            raise RuntimeError("Isolated Mihomo controller did not start.")
                        time.sleep(0.1)
                    report["isolated_core"] = {"mode": state.get("mode"), "interface-name": state.get("interface-name"),
                                               "tun_enabled": state.get("tun", {}).get("enable")}
                    code = (Path(__file__).parent / "capture-portal-tcp.py").read_bytes()
                    encoded = base64.b64encode(code).decode()
                    command = "python3 -u -c " + shlex.quote("import base64;exec(base64.b64decode('" + encoded + "'))")
                    observer = subprocess.Popen(["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "codex-vps", command],
                                                stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                                text=True, creationflags=subprocess.CREATE_NO_WINDOW)

                    def pump():
                        for line in observer.stdout:
                            message = json.loads(line)
                            report["server_messages"].append(message)
                            messages.put(message)

                    threading.Thread(target=pump, daemon=True).start()
                    ready = wait_message(lambda message: message.get("ready"))
                    print(json.dumps({"ready": True, "server_observer": ready, "isolated_core": report["isolated_core"]}), flush=True)
                    save()
                    trials = [("direct", direct_port, False), ("vless", vless_port, False)]
                    trials += [(route, port, True) for _ in range(args.repeat) for route, port in [("direct", direct_port), ("vless", vless_port)]]
                    for index, (route, port, upload) in enumerate(trials):
                        label = route + ("-upload-" if upload else "-get-") + str(index)
                        observer.stdin.write(json.dumps({"label": label}) + "\n")
                        observer.stdin.flush()
                        wait_message(lambda message: message.get("label_ack") == label)
                        response_path = temporary_path / (label + "-response.txt")
                        url = "http://" + SERVER + ":8317/v1/" + ("chat/completions" if upload else "models")
                        lines = ["silent", "show-error", "http1.1", "noproxy = \"\"",
                                 "proxy = " + config_value("socks5h://127.0.0.1:" + str(port)),
                                 "connect-timeout = 15", "max-time = " + str(args.timeout),
                                 "url = " + config_value(url), "output = " + config_value(str(response_path)),
                                 "write-out = \"%{json}\\n\"",
                                 "header = \"Host: cpa.lly778.xyz:8317\"",
                                 "header = " + config_value("Authorization: Bearer " + key)]
                        model = "__portal_transport_diagnostic_" + run_id + "_" + label
                        requested = 0
                        if upload:
                            prefix = '{"model":"' + model + '","messages":[{"role":"user","content":"'
                            suffix = '"}],"stream":false}'
                            padding_length = 241877 - len(prefix) - len(suffix)
                            padding = base64.b64encode(os.urandom(padding_length)).decode()[:padding_length]
                            payload = temporary_path / (label + ".json")
                            payload.write_text(prefix + padding + suffix, encoding="ascii")
                            requested = payload.stat().st_size
                            lines += ["header = \"Content-Type: application/json\"", "header = \"Expect:\"",
                                      "data-binary = " + config_value("@" + str(payload))]
                        started = time.time()
                        completed = subprocess.run(["curl.exe", "--disable", "--config", "-"],
                                                   input="\n".join(lines) + "\n", capture_output=True, text=True,
                                                   timeout=args.timeout + 10, creationflags=subprocess.CREATE_NO_WINDOW)
                        metrics = json.loads(completed.stdout.strip())
                        body = response_path.read_text(encoding="utf-8") if response_path.exists() else ""
                        selected = ["http_code", "exitcode", "errormsg", "size_upload", "size_download", "time_connect",
                                    "time_pretransfer", "time_posttransfer", "time_starttransfer", "time_total"]
                        row = {field: metrics.get(field) for field in selected}
                        row.update(label=label, route=route, upload=upload, requested_bytes=requested,
                                   local_started_epoch=started, expected_response=completed.returncode == 0 and
                                   (metrics["http_code"] == 400 and model in body if upload else metrics["http_code"] == 200))
                        report["client_results"].append(row)
                        save()
                        print(json.dumps(row), flush=True)
                        time.sleep(0.25)
                    observer.stdin.write('{"stop":true}\n')
                    observer.stdin.flush()
                    capture = wait_message(lambda message: "capture" in message, timeout=15)
                    observer.wait(timeout=10)
                    summaries = capture["capture"]["summaries"]
                    print(json.dumps({"capture_overview": {"seconds": capture["capture"]["seconds"],
                                     "observer_queue_drops": capture["capture"]["observer_queue_drops"], "flow_count": len(summaries)}}), flush=True)
                    # Print only candidate large upload flows; retain the full header report.
                    for summary in summaries:
                        port = 8317 if summary["label"].startswith("direct") else 443
                        if summary["server_port"] == port and summary["directions"]["client"]["unique_sequence_bytes"] >= 100000:
                            print(json.dumps({"candidate_upload_tcp_summary": summary}), flush=True)
                    save()
                finally:
                    core.terminate()
                    core.wait(timeout=10)
                    core = None
    finally:
        if core and core.poll() is None:
            core.kill()
            core.wait()
        if observer and observer.poll() is None:
            try:
                observer.stdin.write('{"stop":true}\n')
                observer.stdin.flush()
                observer.wait(timeout=5)
            except (OSError, subprocess.TimeoutExpired):
                observer.kill()
                observer.wait()
        key = ""
        save()
    print("Saved: " + str(output / "comparison.json"), flush=True)


if __name__ == "__main__":
    main()

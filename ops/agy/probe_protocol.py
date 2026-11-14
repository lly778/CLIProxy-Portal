"""Probe ACP setup and MCP catalog refresh without prompts or copied credentials.

Runs a separate CLI process with a disposable home, a loopback MCP fixture,
and no session/prompt method. Never connects to the portal or reads its secrets.
"""

import argparse
import contextlib
import hashlib
import http.server
import json
import os
import pathlib
import queue
import signal
import subprocess
import tempfile
import threading
import time
import uuid


def run(command, read_only_account=None, stateful=False, initialize_only=False):
    with open(command, "rb") as binary:
        report = {"binary_sha256": hashlib.file_digest(binary, "sha256").hexdigest()}
    state = {"generation": 0, "lists": [], "list_phases": [], "phase": "new", "streams": 0, "get_requests": 0, "notifications": 0, "calls": 0}
    stop = threading.Event()
    changed = threading.Condition()
    sessions = set()
    active_streams = set()

    class MCP(http.server.BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, *args):
            pass

        def reply(self, status, body=None, session_id=None):
            data = json.dumps(body).encode() if body is not None else b""
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            if session_id is not None:
                self.send_header("Mcp-Session-Id", session_id)
            self.end_headers()
            self.wfile.write(data)

        def do_POST(self):
            size = int(self.headers.get("Content-Length", "0"))
            if size > 1 << 20:
                self.reply(413)
                return
            try:
                request = json.loads(self.rfile.read(size))
            except (ValueError, TypeError):
                self.reply(400)
                return
            method = request.get("method")
            session_id = None
            if stateful and method != "initialize" and self.headers.get("Mcp-Session-Id") not in sessions:
                self.reply(404)
                return
            if "id" not in request:
                self.reply(202)
                return
            if method == "initialize":
                if stateful:
                    session_id = uuid.uuid4().hex
                    sessions.add(session_id)
                result = {"protocolVersion": "2025-06-18", "capabilities": {"tools": {"listChanged": True}}, "serverInfo": {"name": "protocol-probe", "version": "1"}}
            elif method == "tools/list":
                with changed:
                    generation = state["generation"]
                    state["lists"].append(generation)
                    state["list_phases"].append(state["phase"])
                    changed.notify_all()
                result = {"tools": [{"name": "probe_alpha" if generation == 0 else "probe_beta", "description": "Protocol fixture. Never execute.", "inputSchema": {"type": "object"}}]}
            elif method == "tools/call":
                with changed:
                    state["calls"] += 1
                result = {"content": [{"type": "text", "text": "Probe forbids execution."}], "isError": True}
            elif method == "ping":
                result = {}
            else:
                self.reply(200, {"jsonrpc": "2.0", "id": request["id"], "error": {"code": -32601, "message": "Unsupported probe method"}})
                return
            self.reply(200, {"jsonrpc": "2.0", "id": request["id"], "result": result}, session_id)

        def do_GET(self):
            session_id = self.headers.get("Mcp-Session-Id") if stateful else "stateless"
            with changed:
                state["get_requests"] += 1
                if stateful and session_id not in sessions:
                    self.reply(404)
                    return
                if session_id in active_streams:
                    self.reply(409)
                    return
                active_streams.add(session_id)
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-store")
            self.send_header("Connection", "close")
            self.end_headers()
            self.wfile.write(b": protocol probe\n\n")
            self.wfile.flush()
            self.connection.settimeout(2)
            with changed:
                state["streams"] += 1
                generation = state["generation"]
                changed.notify_all()
            try:
                while not stop.is_set():
                    with changed:
                        changed.wait_for(lambda: stop.is_set() or generation != state["generation"], timeout=1)
                        if generation == state["generation"]:
                            continue
                        generation = state["generation"]
                    message = {"jsonrpc": "2.0", "method": "notifications/tools/list_changed", "params": {}}
                    self.wfile.write(b"event: message\ndata: " + json.dumps(message).encode() + b"\n\n")
                    self.wfile.flush()
                    with changed:
                        state["notifications"] += 1
            except (OSError, ValueError):
                pass
            finally:
                with changed:
                    active_streams.discard(session_id)
            self.close_connection = True

        def do_DELETE(self):
            self.reply(200, {})

    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), MCP)
    server.daemon_threads = True
    threading.Thread(target=server.serve_forever, daemon=True).start()
    process = None
    process_stopped = False

    def stop_process():
        nonlocal process_stopped
        if process is not None and not process_stopped:
            process_stopped = True
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            process.wait(timeout=5)

    try:
        with tempfile.TemporaryDirectory(prefix="portal-agy-protocol-") as root, contextlib.ExitStack() as resources:
            home = pathlib.Path(root)
            workspace = home / "workspace"
            workspace.mkdir()
            if read_only_account:
                if os.readlink("/proc/self/ns/mnt") == os.readlink("/proc/1/ns/mnt"):
                    raise RuntimeError("private_mount_namespace_required")
                target = home / "agy" / "antigravity-oauth-token"
                target.parent.mkdir()
                target.touch(mode=0o600)
                subprocess.run(["mount", "--bind", read_only_account, str(target)], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                resources.callback(lambda: subprocess.run(["umount", str(target)], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL))
                subprocess.run(["mount", "-o", "remount,bind,ro,nodev,nosuid,noexec", str(target)], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                report["account_mount"] = "read_only_in_private_namespace"
            env = os.environ.copy()
            env.update(HOME=root, XDG_CONFIG_HOME=str(home / "config"), XDG_DATA_HOME=str(home / "data"), XDG_CACHE_HOME=str(home / "cache"))
            # Do not allow inherited API keys/proxy authentication into this probe.
            for name in list(env):
                if any(word in name.upper() for word in ("TOKEN", "SECRET", "API_KEY", "CREDENTIAL", "PROXY")):
                    env.pop(name)
            process = subprocess.Popen([command, "acp", "--gemini-dir", root, "--app-data-dir", "agy", "--process-per-session", "--disable-telemetry"], cwd=workspace, env=env, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, start_new_session=True)
            resources.callback(stop_process)
            inbox = queue.Queue()

            def read():
                try:
                    for line in process.stdout:
                        try:
                            inbox.put(json.loads(line))
                        except ValueError:
                            pass
                finally:
                    inbox.put(None)

            threading.Thread(target=read, daemon=True).start()
            notifications = []

            def rpc(identifier, method, params):
                assert method in ("initialize", "session/new", "session/load", "session/close")
                process.stdin.write(json.dumps({"jsonrpc": "2.0", "id": identifier, "method": method, "params": params}).encode() + b"\n")
                process.stdin.flush()
                deadline = time.monotonic() + 18
                while time.monotonic() < deadline:
                    reply = inbox.get(timeout=max(0.01, deadline - time.monotonic()))
                    if reply is None:
                        raise RuntimeError("process_ended")
                    if reply.get("id") == identifier:
                        return reply
                    if reply.get("method") == "session/update":
                        update = reply.get("params", {})
                        notifications.append({"keys": sorted(update), "update_keys": sorted(update.get("update", {})), "kind": update.get("update", {}).get("sessionUpdate")})
                raise TimeoutError()

            init = rpc(1, "initialize", {"protocolVersion": 1, "clientCapabilities": {}, "clientInfo": {"name": "protocol-probe", "version": "1"}})
            data = init.get("result", {})
            report["acp"] = data.get("protocolVersion")
            report["capabilities"] = data.get("agentCapabilities", {})
            report["agent"] = {key: value for key, value in data.get("agentInfo", {}).items() if key in ("name", "version")}

            # Capability absence is not a negative assertion. This probe never
            # sends experimental methods or a model prompt to test transport.
            mcp_capabilities = report["capabilities"].get("mcpCapabilities", {})
            acp = mcp_capabilities.get("acp") if isinstance(mcp_capabilities, dict) else None
            report["mcp_over_acp_declaration"] = acp if isinstance(acp, bool) else "unknown"
            if initialize_only:
                report["setup"] = "not_attempted_initialize_only"
                report["inference_prompts_sent"] = 0
                return report
            spec = {"cwd": str(workspace), "mcpServers": [{"type": "http", "name": "protocol-probe", "url": f"http://127.0.0.1:{server.server_port}/mcp", "headers": []}]}
            created = rpc(2, "session/new", spec)
            session_id = created.get("result", {}).get("sessionId")
            report["new_session"] = "ok" if session_id else "error"
            if not session_id:
                report["new_error_code"] = created.get("error", {}).get("code")
            else:
                report["new_native_database_present"] = (home / "agy" / "conversations" / (session_id + ".db")).is_file()
                with changed:
                    changed.wait_for(lambda: state["lists"] and state["streams"] > 0, timeout=4)
                    state["generation"] = 1
                    state["phase"] = "notified"
                    changed.notify_all()
                    changed.wait_for(lambda: 1 in state["lists"], timeout=20)
                    report["catalog_refetched_before_load"] = 1 in state["lists"]
                    state["phase"] = "close"
                rpc(3, "session/close", {"sessionId": session_id})
                state["phase"] = "load"
                loaded = rpc(4, "session/load", dict(spec, sessionId=session_id))
                report["empty_session_load"] = "ok" if "result" in loaded else "error"
                rpc(5, "session/close", {"sessionId": session_id})
            missing = rpc(6, "session/load", dict(spec, sessionId="portal-probe-nonexistent"))
            message = missing.get("error", {}).get("message", "")
            report["missing_session"] = {"result_without_error": "result" in missing and "error" not in missing, "returned_requested_id": missing.get("result", {}).get("sessionId") == "portal-probe-nonexistent", "native_database_created": (home / "agy" / "conversations" / "portal-probe-nonexistent.db").is_file(), "code": missing.get("error", {}).get("code"), "message_sha256": hashlib.sha256(message.encode()).hexdigest(), "known_prefix": next((prefix for prefix in ("session not found", "unknown session", "conversation not found") if message.lower().startswith(prefix)), None)}
            report["notification_shapes"] = notifications
            report["mcp"] = state
            report["transport_sessions"] = "stateful" if stateful else "stateless"
            report["inference_prompts_sent"] = 0
    except (OSError, RuntimeError, TimeoutError, queue.Empty, subprocess.SubprocessError) as error:
        report["probe_error"] = type(error).__name__
    finally:
        stop.set()
        with changed:
            changed.notify_all()
        stop_process()
        server.shutdown()
        server.server_close()
    return report


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", type=lambda value: str(pathlib.Path(value).resolve(strict=True)))
    parser.add_argument("--read-only-account", help="Optional existing account file; requires a separate Linux mount namespace and read-only bind mount. Never copied or printed.")
    parser.add_argument("--stateful", action="store_true", help="Assign independent MCP session IDs; the default fixture is stateless with one active notification channel.")
    parser.add_argument("--initialize-only", action="store_true", help="Read capability declarations only; do not create, load or prompt a session.")
    args = parser.parse_args()
    print(json.dumps(run(args.command, args.read_only_account, args.stateful, args.initialize_only), ensure_ascii=True, sort_keys=True))

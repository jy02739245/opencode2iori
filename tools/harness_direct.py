#!/usr/bin/env python3
"""Build/start the direct gateway and verify it with a real isolated DSH session."""
import argparse
from collections import Counter
from datetime import datetime
import hashlib
from http.client import HTTPConnection
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import secrets
import shutil
import subprocess
import threading
import time
import urllib.error
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
MODEL = "deepseek-v4.1-flash-free"
BASE = "http://127.0.0.1:8080"
OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))
PROXY_ENV = ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy")


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def write_json(path, value):
    path.write_text(json.dumps(value, ensure_ascii=False, indent=2) + "\n")


def direct_env():
    env = os.environ.copy()
    for key in PROXY_ENV:
        env.pop(key, None)
    env.update(NO_PROXY="127.0.0.1,localhost", no_proxy="127.0.0.1,localhost")
    return env


def config():
    cfg = json.loads((ROOT / "config.json").read_text())
    require(cfg.get("listen") == "127.0.0.1:8080", "Expected listen=127.0.0.1:8080")
    require(cfg.get("proxies") == ["direct"] and not cfg.get("proxyfile") and not cfg.get("warp", {}).get("enabled"), "Expected direct-only configuration with WARP disabled")
    return cfg


def enabled_key(cfg):
    key = next((key for key in cfg.get("server_keys", []) if not cfg.get("server_key_metadata", {}).get(hashlib.sha256(key.encode()).hexdigest()[:16], {}).get("disabled", False)), None)
    require(key, "No local server key configured")
    return key


def provider(base_url):
    return [
        {"id": "agent-default-model", "config": {"provider": "local-opencode", "model": MODEL, "reasoningEffort": "minimal"}},
        {"id": "llm-pi-ai", "config": {"providers": {"local-opencode": {
            "displayName": "Local OpenCode Direct", "apiKeyEnv": "LOCAL_OPENCODE_API_KEY",
            "api": "openai-completions", "baseURL": base_url,
            "retryPolicy": {"mode": "normal", "maxRetries": 0},
            "compat": {"supportsDeveloperRole": False, "maxTokensField": "max_tokens", "thinkingFormat": "deepseek"},
            "models": [{"id": MODEL, "name": "DeepSeek V4.1 Flash Free", "contextWindow": 200000,
                        "maxTokens": 4096, "input": ["text"],
                        "reasoningEfforts": {"minimal": "minimal", "low": "low", "high": "high"}}]
        }}}}
    ]


def prepare():
    require(shutil.which("go") and shutil.which("dsh"), "go and dsh must be on PATH")
    out = ROOT / ".local" / ("harness-direct-" + datetime.now().strftime("%Y%m%d-%H%M%S") + "-" + secrets.token_hex(2))
    out.mkdir(parents=True, exist_ok=False)
    cfg_path = ROOT / "config.json"
    cfg = json.loads(cfg_path.read_text())
    expected = json.loads(json.dumps(cfg))
    expected.update(listen="127.0.0.1:8080", proxies=["direct"], proxyfile="")
    expected.setdefault("warp", {})["enabled"] = False
    if cfg != expected:
        backup = out / "config.before.json"
        with backup.open("x", opener=lambda path, flags: os.open(path, flags, 0o600)) as handle:
            handle.write(cfg_path.read_text())
        write_json(cfg_path, expected)
    config()
    env = direct_env()
    env["GOCACHE"] = str(ROOT / ".local/go-build-cache")
    with (out / "build.log").open("w") as log:
        subprocess.run(["go", "build", "-o", str(out / "opencode2api"), "./cmd/opencode2api"], cwd=ROOT, env=env, stdout=log, stderr=subprocess.STDOUT, check=True)
    diff = subprocess.check_output(["git", "diff", "HEAD"], cwd=ROOT)
    write_json(out / "build-context.json", {
        "commit": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
        "tracked_diff_sha256": hashlib.sha256(diff).hexdigest(),
        "binary_sha256": hashlib.sha256((out / "opencode2api").read_bytes()).hexdigest(),
        "dsh_version": subprocess.check_output(["dsh", "--version"], text=True).strip(),
        "connection": "direct", "model": MODEL, "project": str(ROOT)
    })
    print(json.dumps({"run_dir": str(out), "config_changed": cfg != expected, "model": MODEL, "connection": "direct"}))


def serve(out):
    config()
    context = json.loads((out / "build-context.json").read_text())
    require(hashlib.sha256((out / "opencode2api").read_bytes()).hexdigest() == context["binary_sha256"], "Build artifact checksum mismatch")
    os.chdir(ROOT)
    with (out / "service.log").open("a") as log:
        os.dup2(log.fileno(), 1)
        os.dup2(log.fileno(), 2)
    os.execve(str(out / "opencode2api"), [str(out / "opencode2api"), "-config", str(ROOT / "config.json"), "-web-listen", "127.0.0.1:8081"], direct_env())


class Observer(BaseHTTPRequestHandler):
    """Forward SSE incrementally; never retain authentication headers."""
    def log_message(self, *_):
        pass

    def do_POST(self):
        payload = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        body = json.loads(payload)
        entry = {"path": self.path, "model": body.get("model"), "stream": body.get("stream"),
                 "tool_count": len(body.get("tools") or []), "status": None}
        if entry["tool_count"] == 0:
            entry["request_body"] = body
        with self.server.records_lock:
            if entry["tool_count"] > 0 and not any(item["tool_count"] > 0 for item in self.server.records):
                write_json(self.server.output / "first-main-request.json", body)
            self.server.records.append(entry)
        upstream = HTTPConnection("127.0.0.1", 8080, timeout=180)
        try:
            headers = {key: value for key, value in self.headers.items() if key.lower() not in ("host", "connection", "transfer-encoding", "content-length")}
            upstream.request("POST", self.path, body=payload, headers=headers)
            response = upstream.getresponse()
            entry.update(status=response.status, content_type=response.getheader("Content-Type", ""), request_id=response.getheader("x-request-id", ""))
            self.send_response(response.status)
            self.send_header("Content-Type", entry["content_type"])
            self.send_header("Connection", "close")
            self.end_headers()
            # A readline preserves SSE frame boundaries without waiting for a full answer.
            if response.status != 200:
                error = response.read(1 << 20)
                entry["error_body"] = error.decode("utf-8", errors="replace")
                self.wfile.write(error)
                self.wfile.flush()
            else:
                if entry["tool_count"] == 0:
                    entry["response_body"] = ""
                for line in response:
                    if "response_body" in entry and len(entry["response_body"]) < (1 << 20):
                        entry["response_body"] += line.decode("utf-8", errors="replace")
                    self.wfile.write(line)
                    self.wfile.flush()
        except Exception as exc:
            entry["transport_error"] = type(exc).__name__
        finally:
            self.close_connection = True
            upstream.close()


def test(out):
    cfg = config()
    meta = json.loads((out / "build-context.json").read_text())
    ready = False
    for _ in range(60):
        try:
            with OPENER.open(BASE + "/healthz", timeout=2) as response:
                health = json.load(response)
            ready = health.get("ready", False)
            if ready:
                break
        except (OSError, urllib.error.URLError):
            pass
        time.sleep(0.5)
    require(ready, "Gateway readiness timed out")
    key = enabled_key(cfg)
    request = urllib.request.Request(BASE + "/v1/models", headers={"Authorization": "Bearer " + key})
    with OPENER.open(request, timeout=5) as response:
        models = json.load(response)
    require(any(item["id"] == MODEL for item in models["data"]), "Requested model is not advertised")
    with OPENER.open("http://127.0.0.1:8081", timeout=5) as response:
        admin_status = response.status
    write_json(out / "service-check.json", {"health": health, "admin_http": admin_status})

    workspace = out / "workspace"
    workspace.mkdir(exist_ok=False)
    home = out / "dsh-home"
    fixture_skill = home / "skills/adapter-fixture/SKILL.md"
    fixture_skill.parent.mkdir(parents=True)
    fixture_skill.write_text("---\nname: adapter-fixture\ndescription: Harmless integration test marker.\n---\n\nThe test skill marker is ADAPTER_SKILL_LOADED. Report it when loaded.\n")
    token = "MEMORY_" + secrets.token_hex(6)
    before = "BEFORE_" + secrets.token_hex(6)
    after = before.replace("BEFORE_", "AFTER_")
    target = workspace / "fixture.txt"
    target.write_text(before + "\n")
    observer = ThreadingHTTPServer(("127.0.0.1", 0), Observer)
    observer.records, observer.records_lock, observer.output = [], threading.Lock(), out
    observer.daemon_threads = False
    worker = threading.Thread(target=observer.serve_forever, daemon=True)
    worker.start()
    patch = out / "provider.yml"
    write_json(patch, provider(f"http://127.0.0.1:{observer.server_port}/v1"))
    env = direct_env()
    env.update(DSH_HOME=str(home), LOCAL_OPENCODE_API_KEY=key)
    session, turns = None, []

    def save():
        write_json(out / "real-summary.json", {"model": MODEL, "connection": "direct", "session_id": session,
                   "memory_token": token, "file_expected_after": after, "file_actual": target.read_text().strip(), "turns": turns})

    def turn(name, prompt, expected):
        nonlocal session
        command = ["dsh", "headless", "--patch", str(patch), "--json", "--permission-mode", "workspace-write", "--approval-policy", "never"]
        if session:
            command.extend(["--session-id", session])
        started = time.monotonic()
        try:
            run = subprocess.run(command + [prompt], cwd=workspace, env=env, capture_output=True, text=True, timeout=180)
            stdout, stderr, code = run.stdout, run.stderr, run.returncode
        except subprocess.TimeoutExpired as exc:
            stdout = exc.stdout or b""
            stdout = stdout.decode() if isinstance(stdout, bytes) else stdout
            stderr, code = "DSH process timed out", 124
        (out / (name + ".jsonl")).write_text(stdout)
        (out / (name + ".stderr.log")).write_text(stderr)
        events = [json.loads(line) for line in stdout.splitlines() if line.startswith("{")]
        for event in events:
            if event.get("type") == "session":
                require(session is None or session == event["sessionId"], "DSH session changed")
                session = event["sessionId"]
        finals = [event for event in events if event.get("type") == "final"]
        final = finals[-1] if finals else {}
        result = {"turn": name, "exit_code": code, "elapsed_seconds": round(time.monotonic() - started, 2),
                  "session_id": session, "final": final, "tool_calls": [e for e in events if e.get("type") == "tool_call"],
                  "tool_results": [e for e in events if e.get("type") == "tool_result"]}
        result["completed"] = code == 0 and final.get("reason", {}).get("kind") == "completed" and final.get("text", "").strip() == expected
        turns.append(result)
        save()
        print(json.dumps({"turn": name, "completed": result["completed"], "tools": [e["tool"] for e in result["tool_calls"]]}), flush=True)
        require(result["completed"], f"{name} failed; inspect its JSONL and stderr")
        require(session, "No DSH session ID returned")
        return result

    failure = None
    try:
        turn("01-memory", f"Remember this exact label for our conversation: {token}. Reply with exactly ACK:{token}. Do not call tools.", "ACK:" + token)
        turn("02-recall", "What exact label did I ask you to remember in my previous message? Reply with that label only. Do not call tools.", token)
        tools = turn("03-tools", f"Perform the tool integration test. Call read to read fixture.txt, then edit to replace {before} with {after}, then skill with name adapter-fixture. Execute all three actual tools. Finally reply exactly TOOLS_OK:{after}|ADAPTER_SKILL_LOADED. Do not use bash, subagents, or other tools for these actions.", f"TOOLS_OK:{after}|ADAPTER_SKILL_LOADED")
        require({"read", "edit", "skill"} <= {event["tool"] for event in tools["tool_calls"]}, "Missing actual tool calls")
        results = {event["callId"]: event for event in tools["tool_results"]}
        require(all(results.get(call["callId"], {}).get("status") == "completed" for call in tools["tool_calls"]), "A tool failed or has no result")
        require(target.read_text().strip() == after, "Actual file edit did not occur")
        turn("04-after-tools", "Without tools, reply with the original test label from our first user turn, a single |, and the exact new label you wrote into fixture.txt. No other text.", token + "|" + after)
    except Exception as exc:
        failure = str(exc)
    finally:
        observer.shutdown()
        worker.join(timeout=5)
        observer.server_close()
        save()
        write_json(out / "requests.json", observer.records)
    requests = observer.records
    auxiliary = [entry for entry in requests if entry["tool_count"] == 0]
    main = [entry for entry in requests if entry["tool_count"] > 0]
    checks = {
        "conversation_and_tools": failure is None and len(turns) == 4,
        "auxiliary_observed": len(auxiliary) > 0,
        "all_requests_streaming": bool(requests) and all(entry["stream"] is True for entry in requests),
        "sse_on_success": all("text/event-stream" in entry.get("content_type", "") for entry in requests if entry["status"] == 200),
        "main_requests_successful": bool(main) and all(entry["status"] == 200 and not entry.get("transport_error") for entry in main),
        "correct_model": bool(requests) and all(entry["model"] == MODEL for entry in requests),
    }
    summary = {"core_checks_passed": all(checks.values()), "checks": checks, "failure": failure, "session_id": session,
               "model": MODEL, "connection": "direct", "dsh_version": meta["dsh_version"],
               "main_requests": len(main), "auxiliary_requests": len(auxiliary),
               "main_statuses": dict(Counter(str(entry["status"]) for entry in main)),
               "auxiliary_statuses": dict(Counter(str(entry["status"]) for entry in auxiliary)),
               "auxiliary_requests_successful": bool(auxiliary) and all(entry["status"] == 200 and not entry.get("transport_error") and "data: [DONE]" in entry.get("response_body", "") for entry in auxiliary)}
    write_json(out / "verification-summary.json", summary)
    (out / "REPORT.md").write_text(
        "# Harness 直连回归报告\n\n"
        "| 检查项 | 结果 | 实测说明 | 证据 |\n"
        "| --- | --- | --- | --- |\n"
        f"| 模型与连接 | {'通过' if checks['correct_model'] else '失败'} | `{MODEL}`；direct；DSH `{meta['dsh_version']}` | [验证汇总](verification-summary.json) |\n"
        f"| 核心检查 | {'通过' if summary['core_checks_passed'] else '失败'} | 主会话、工具与流式检查；辅助完成状态单列 | [验证汇总](verification-summary.json) |\n"
        f"| 连续对话与工具 | {'通过' if checks['conversation_and_tools'] else '失败'} | 完成 {sum(turn['completed'] for turn in turns)} / 4 轮；核对同会话记忆、read/edit/skill、实际文件与工具后回忆 | [真实 Harness 事件](real-summary.json) |\n"
        f"| 主请求 HTTP | {'通过' if checks['main_requests_successful'] else '失败'} | {summary['main_requests']} 次；状态分布 `{summary['main_statuses']}` | [逐请求证据](requests.json) |\n"
        f"| 辅助请求观测 | {'通过' if checks['auxiliary_observed'] else '未完成'} | {summary['auxiliary_requests']} 次；HTTP 状态分布 `{summary['auxiliary_statuses']}` | [逐请求证据](requests.json) |\n"
        f"| 全部请求流式 | {'通过' if checks['all_requests_streaming'] else '失败'} | 逐请求核对 `stream:true` | [逐请求证据](requests.json) |\n"
        f"| 成功响应 SSE | {'通过' if checks['sse_on_success'] else '失败'} | HTTP 200 响应核对 `text/event-stream` | [逐请求证据](requests.json) |\n"
        f"| 原生辅助流完整消费 | {'通过' if summary['auxiliary_requests_successful'] else '未完成'} | 分别核对 HTTP 200、传输错误与 `[DONE]`；headless 退出可能取消尚未结束的标题流 | [逐请求证据](requests.json) · [服务日志](service.log) |\n"
        + "| 失败原因 | " + (failure.replace("|", r"\|").replace("\r", " ").replace("\n", "<br>") if failure else "无")
        + " | 主流程失败原因；辅助流状态见上一行 | [验证汇总](verification-summary.json) |\n"
        "| 服务健康 | 未检查 | 本报告模板未发起健康检查；按实际核验结果补充 | [服务日志](service.log) |\n"
        "| API | 服务地址 | `http://127.0.0.1:8080/v1` | 本地直连配置 |\n"
        "| 管理页 | 服务地址 | `http://127.0.0.1:8081` | 本地服务配置 |\n")
    print(json.dumps(summary, ensure_ascii=False, indent=2), flush=True)
    require(summary["core_checks_passed"], "Harness validation failed; inspect verification-summary.json")


def diagnose(out):
    """A/B/A the captured title with one real client declaration; execute no tools."""
    key = enabled_key(config())
    main = json.loads((out / "first-main-request.json").read_text())
    requests = json.loads((out / "requests.json").read_text())
    auxiliary = next(entry["request_body"] for entry in requests if entry["tool_count"] == 0)
    read_tools = [tool for tool in main["tools"] if tool["function"]["name"] == "read"]
    require(len(read_tools) == 1, "Expected a single real DSH read declaration")
    results = []
    for variant in ("original_before", "with_real_dsh_read", "original_after"):
        payload = json.loads(json.dumps(auxiliary))
        if variant == "with_real_dsh_read":
            payload["tools"] = read_tools
        request = urllib.request.Request(BASE + "/v1/chat/completions", data=json.dumps(payload).encode(),
                                         headers={"Authorization": "Bearer " + key, "Content-Type": "application/json"})
        try:
            response = OPENER.open(request, timeout=180)
        except urllib.error.HTTPError as exc:
            response = exc
        with response:
            result = {"variant": variant, "model": payload["model"], "stream": payload.get("stream"),
                      "tool_count": len(payload.get("tools", [])), "status": response.status,
                      "content_type": response.headers.get("Content-Type", ""),
                      "request_id": response.headers.get("X-Request-ID", "")}
            result["body"] = "".join(line.decode("utf-8", errors="replace") for line in response)
        events = []
        for line in (out / "service.log").read_text().splitlines():
            try:
                events.append(json.loads(line))
            except json.JSONDecodeError:
                continue
        routed = next((event for event in events if event.get("event") == "request_routed" and event.get("request_id") == result["request_id"]), {})
        result["upstream_legs"] = routed.get("legs", "")
        result["synthetic_title"] = "chatcmpl-title-" in result["body"]
        results.append(result)
        write_json(out / "auxiliary-diagnosis.json", results)
        print(json.dumps({key: value for key, value in result.items() if key != "body"}), flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("prepare", "serve", "test", "diagnose"))
    parser.add_argument("--run-dir", type=Path)
    args = parser.parse_args()
    if args.action == "prepare":
        prepare()
    else:
        require(args.run_dir is not None, "--run-dir is required")
        out = args.run_dir.resolve()
        require(out.is_dir() and out.parent == ROOT / ".local", "run directory must be a prepared .local directory")
        {"serve": serve, "test": test, "diagnose": diagnose}[args.action](out)


if __name__ == "__main__":
    main()

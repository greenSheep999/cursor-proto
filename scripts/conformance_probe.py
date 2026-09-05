#!/usr/bin/env python3
"""Anthropic Messages conformance probe mirroring the cctest.ai check matrix.

Each check asserts the user-visible symptom (a specific protocol requirement),
so a red result names the exact contract that is broken.

Environment:
  CLAUDE_BASE_URL  e.g. http://127.0.0.1:8317
  CLAUDE_API_KEY   API key for that endpoint
  CLAUDE_MODEL     defaults to claude-opus-4-8
  PROBE_CASES      comma-separated subset, defaults to all
  PROBE_TIMEOUT    per-request seconds, default 120
"""

import base64
import json
import os
import struct
import sys
import time
import urllib.error
import urllib.request
import zlib

BASE_URL = os.environ.get("CLAUDE_BASE_URL", "http://127.0.0.1:8317").rstrip("/")
API_KEY = os.environ.get("CLAUDE_API_KEY", "localtest")
MODEL = os.environ.get("CLAUDE_MODEL", "claude-opus-4-8")
TIMEOUT = float(os.environ.get("PROBE_TIMEOUT", "120"))


def post(payload, timeout=None):
    return urllib.request.urlopen(
        urllib.request.Request(
            f"{BASE_URL}/v1/messages",
            data=json.dumps(payload).encode(),
            headers={
                "content-type": "application/json",
                "x-api-key": API_KEY,
                "authorization": f"Bearer {API_KEY}",
                "anthropic-version": "2023-06-01",
                "user-agent": os.environ.get(
                    "PROBE_USER_AGENT", "claude-cli/2.1.231 (external, cli)"
                ),
            },
        ),
        timeout=timeout or TIMEOUT,
    )


def read_sse(response, deadline):
    """Yield (event_name, parsed_json) preserving wire order."""
    event_name = None
    for raw in response:
        if time.monotonic() > deadline:
            yield ("__timeout__", {})
            return
        line = raw.decode(errors="replace").rstrip("\n")
        if line.startswith("event:"):
            event_name = line[6:].strip()
        elif line.startswith("data:"):
            data = line[5:].strip()
            if not data or data == "[DONE]":
                continue
            try:
                yield (event_name, json.loads(data))
            except json.JSONDecodeError:
                yield (event_name, {"__unparsable__": data})


def png_bytes(width, height, rgb):
    """Build a minimal solid-colour PNG without external dependencies."""

    def chunk(tag, body):
        data = tag + body
        return struct.pack(">I", len(body)) + data + struct.pack(">I", zlib.crc32(data))

    raw = b"".join(b"\x00" + bytes(rgb) * width for _ in range(height))
    return (
        b"\x89PNG\r\n\x1a\n"
        + chunk(b"IHDR", struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0))
        + chunk(b"IDAT", zlib.compress(raw))
        + chunk(b"IEND", b"")
    )


def pdf_bytes(text):
    """Build a minimal single-page PDF containing one text string."""
    content = f"BT /F1 24 Tf 72 700 Td ({text}) Tj ET".encode()
    objects = [
        b"<< /Type /Catalog /Pages 2 0 R >>",
        b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
        b"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] "
        b"/Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
        b"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
        b"<< /Length " + str(len(content)).encode() + b" >>\nstream\n" + content + b"\nendstream",
    ]
    out = bytearray(b"%PDF-1.4\n")
    offsets = []
    for index, body in enumerate(objects, start=1):
        offsets.append(len(out))
        out += f"{index} 0 obj\n".encode() + body + b"\nendobj\n"
    xref = len(out)
    out += f"xref\n0 {len(objects) + 1}\n0000000000 65535 f \n".encode()
    for offset in offsets:
        out += f"{offset:010d} 00000 n \n".encode()
    out += (
        f"trailer\n<< /Size {len(objects) + 1} /Root 1 0 R >>\nstartxref\n{xref}\n%%EOF".encode()
    )
    return bytes(out)


# --------------------------------------------------------------------------
# checks
# --------------------------------------------------------------------------

LEGAL_STOP_REASONS = {
    "end_turn",
    "max_tokens",
    "stop_sequence",
    "tool_use",
    "pause_turn",
    "refusal",
    "model_context_window_exceeded",
}


def check_stream_structure():
    """流结构校验 + 协议合规性: full Anthropic SSE state machine."""
    started = time.monotonic()
    deadline = started + TIMEOUT
    events = []
    with post({
        "model": MODEL,
        "max_tokens": 128,
        "stream": True,
        "messages": [{"role": "user", "content": "Say exactly: STREAM_OK"}],
    }) as response:
        first_byte = None
        for name, event in read_sse(response, deadline):
            if first_byte is None:
                first_byte = time.monotonic() - started
            events.append((name, event))
            if event.get("type") in {"message_stop", "error"}:
                break

    types = [e.get("type") for _, e in events]
    problems = []

    if types.count("message_start") != 1:
        problems.append(f"message_start x{types.count('message_start')} (must be 1)")
    if types.count("message_stop") != 1:
        problems.append(f"message_stop x{types.count('message_stop')} (must be 1)")
    if "error" in types:
        problems.append("stream emitted event:error")
    for name, event in events:
        if name is not None and event.get("type") and name != event["type"]:
            problems.append(f"event:{name} != type:{event['type']}")
            break
    if types and types[0] != "message_start":
        problems.append(f"first event is {types[0]}, not message_start")

    # index pairing
    open_blocks = set()
    for _, event in events:
        kind = event.get("type")
        if kind == "content_block_start":
            open_blocks.add(event.get("index"))
        elif kind == "content_block_stop":
            open_blocks.discard(event.get("index"))
    if open_blocks:
        problems.append(f"unclosed content blocks: {sorted(open_blocks)}")

    delta = next((e for _, e in events if e.get("type") == "message_delta"), None)
    if delta is None:
        problems.append("no message_delta")
    else:
        stop_reason = delta.get("delta", {}).get("stop_reason")
        if stop_reason not in LEGAL_STOP_REASONS:
            problems.append(f"illegal stop_reason={stop_reason!r}")
        if "usage" not in delta:
            problems.append("message_delta missing usage")

    return {
        "ttfb_s": round(first_byte, 2) if first_byte else None,
        "total_s": round(time.monotonic() - started, 2),
        "sequence": ",".join(t for t in types if t),
        "problems": problems,
    }


def check_nonstream_structure():
    """非流结构校验."""
    with post({
        "model": MODEL,
        "max_tokens": 128,
        "stream": False,
        "messages": [{"role": "user", "content": "Say exactly: NONSTREAM_OK"}],
    }) as response:
        body = json.loads(response.read())
    problems = []
    for field in ("id", "type", "role", "content", "model", "stop_reason", "usage"):
        if field not in body:
            problems.append(f"missing {field}")
    if body.get("stop_reason") not in LEGAL_STOP_REASONS:
        problems.append(f"illegal stop_reason={body.get('stop_reason')!r}")
    if not body.get("content"):
        problems.append("empty content")
    usage = body.get("usage") or {}
    for field in ("input_tokens", "output_tokens"):
        if field not in usage:
            problems.append(f"usage missing {field}")
    return {"stop_reason": body.get("stop_reason"), "usage": usage, "problems": problems}


def check_tool_calling():
    """工具调用: Claude Code declares exact-cased tools; we must echo them back."""
    started = time.monotonic()
    deadline = started + TIMEOUT
    payload = {
        "model": MODEL,
        "max_tokens": 512,
        "stream": True,
        "tools": [
            {
                "name": "Glob",
                "description": "Find files matching a glob pattern.",
                "input_schema": {
                    "type": "object",
                    "properties": {"pattern": {"type": "string"}, "path": {"type": "string"}},
                    "required": ["pattern"],
                },
            },
            {
                "name": "Bash",
                "description": "Run a shell command.",
                "input_schema": {
                    "type": "object",
                    "properties": {"command": {"type": "string"}},
                    "required": ["command"],
                },
            },
        ],
        "messages": [
            {"role": "user", "content": "Use the Glob tool to find every .go file in this repo. Call the tool."}
        ],
    }
    types, tool_names, tool_inputs = [], [], []
    timed_out = False
    with post(payload) as response:
        for name, event in read_sse(response, deadline):
            if name == "__timeout__":
                timed_out = True
                break
            kind = event.get("type")
            if kind:
                types.append(kind)
            if kind == "content_block_start":
                block = event.get("content_block", {})
                if block.get("type") == "tool_use":
                    tool_names.append(block.get("name"))
                    tool_inputs.append(block.get("input"))
            if kind in {"message_stop", "error"}:
                break

    problems = []
    if timed_out:
        problems.append(f"HUNG: no terminal event within {TIMEOUT}s")
    if not tool_names:
        problems.append("no tool_use block emitted")
    else:
        for got in tool_names:
            if got not in {"Glob", "Bash"}:
                problems.append(f"tool name {got!r} was never declared by the client")
    delta = None
    if "message_delta" not in types:
        problems.append("no message_delta")
    if tool_names and "message_stop" not in types:
        problems.append("no message_stop after tool_use")
    return {
        "total_s": round(time.monotonic() - started, 2),
        "tools": tool_names,
        "inputs": tool_inputs,
        "sequence": ",".join(types),
        "problems": problems,
    }


def check_structured_output():
    """结构化输出: forced tool_choice must yield exactly that tool, schema-valid."""
    started = time.monotonic()
    deadline = started + TIMEOUT
    payload = {
        "model": MODEL,
        "max_tokens": 512,
        "stream": True,
        "tools": [
            {
                "name": "record_answer",
                "description": "Record the final structured answer.",
                "input_schema": {
                    "type": "object",
                    "properties": {
                        "answer": {"type": "string"},
                        "count": {"type": "integer"},
                    },
                    "required": ["answer", "count"],
                    "additionalProperties": False,
                },
            }
        ],
        "tool_choice": {"type": "tool", "name": "record_answer"},
        "messages": [{"role": "user", "content": "Answer is STRUCTURED_OK and count is 7."}],
    }
    tool_names, json_parts, types = [], {}, []
    timed_out = False
    with post(payload) as response:
        for name, event in read_sse(response, deadline):
            if name == "__timeout__":
                timed_out = True
                break
            kind = event.get("type")
            if kind:
                types.append(kind)
            if kind == "content_block_start":
                block = event.get("content_block", {})
                if block.get("type") == "tool_use":
                    tool_names.append(block.get("name"))
                    json_parts[event.get("index")] = ""
            if kind == "content_block_delta":
                delta = event.get("delta", {})
                if delta.get("type") == "input_json_delta":
                    idx = event.get("index")
                    json_parts[idx] = json_parts.get(idx, "") + delta.get("partial_json", "")
            if kind in {"message_stop", "error"}:
                break

    problems = []
    if timed_out:
        problems.append(f"HUNG: no terminal event within {TIMEOUT}s")
    if tool_names != ["record_answer"]:
        problems.append(f"tool_choice forced record_answer but got {tool_names}")
    parsed = None
    for value in json_parts.values():
        try:
            parsed = json.loads(value) if value else None
        except json.JSONDecodeError:
            problems.append(f"tool input is not valid JSON: {value[:80]!r}")
    if parsed is not None:
        if set(parsed) != {"answer", "count"}:
            problems.append(f"schema violation, keys={sorted(parsed)}")
    elif not problems:
        problems.append("no input_json_delta payload")
    return {
        "total_s": round(time.monotonic() - started, 2),
        "tools": tool_names,
        "parsed": parsed,
        "sequence": ",".join(types),
        "problems": problems,
    }


def check_websearch():
    """WebSearch: Anthropic server tool contract."""
    started = time.monotonic()
    deadline = started + TIMEOUT
    payload = {
        "model": MODEL,
        "max_tokens": 1024,
        "stream": True,
        "tools": [{"type": "web_search_20250305", "name": "web_search", "max_uses": 1}],
        "messages": [{"role": "user", "content": "Search the web for today's date in Tokyo and cite a source."}],
    }
    block_types, tool_ids, result_ids, types = [], [], [], []
    usage = {}
    timed_out = False
    with post(payload) as response:
        for name, event in read_sse(response, deadline):
            if name == "__timeout__":
                timed_out = True
                break
            kind = event.get("type")
            if kind:
                types.append(kind)
            if kind == "content_block_start":
                block = event.get("content_block", {})
                btype = block.get("type")
                if btype:
                    block_types.append(btype)
                if btype == "server_tool_use":
                    tool_ids.append(block.get("id"))
                if btype == "web_search_tool_result":
                    result_ids.append(block.get("tool_use_id"))
            if kind == "message_delta":
                usage = event.get("usage", {})
            if kind in {"message_stop", "error"}:
                break

    problems = []
    if timed_out:
        problems.append(f"HUNG: no terminal event within {TIMEOUT}s")
    if "server_tool_use" not in block_types:
        problems.append("no server_tool_use block")
    if "web_search_tool_result" not in block_types:
        problems.append("no web_search_tool_result block")
    if tool_ids and result_ids and tool_ids[0] != result_ids[0]:
        problems.append(f"tool_use_id mismatch {tool_ids[0]} != {result_ids[0]}")
    requests = (usage.get("server_tool_use") or {}).get("web_search_requests")
    if not requests:
        problems.append("usage.server_tool_use.web_search_requests missing/zero")
    return {
        "total_s": round(time.monotonic() - started, 2),
        "blocks": block_types,
        "usage": usage,
        "sequence": ",".join(types),
        "problems": problems,
    }


def _vision_case(label, block, needle):
    started = time.monotonic()
    with post({
        "model": MODEL,
        "max_tokens": 256,
        "stream": False,
        "messages": [{"role": "user", "content": [block, {"type": "text", "text": f"What does this contain? Answer briefly."}]}],
    }) as response:
        body = json.loads(response.read())
    text = "".join(b.get("text", "") for b in body.get("content", []) if b.get("type") == "text")
    problems = []
    if not text.strip():
        problems.append("empty response")
    elif needle.lower() not in text.lower():
        problems.append(f"model did not report {needle!r}; said: {text[:160]!r}")
    return {"total_s": round(time.monotonic() - started, 2), "text": text[:200], "problems": problems}


def check_image():
    """图片识别: base64 image block."""
    return _vision_case(
        "image",
        {
            "type": "image",
            "source": {
                "type": "base64",
                "media_type": "image/png",
                "data": base64.b64encode(png_bytes(64, 64, (255, 0, 0))).decode(),
            },
        },
        "red",
    )


def check_image_url():
    """图片识别 (url source variant)."""
    return _vision_case(
        "image_url",
        {
            "type": "image",
            "source": {"type": "url", "url": "https://www.python.org/static/img/python-logo.png"},
        },
        "python",
    )


def check_document():
    """文档识别: base64 PDF block."""
    return _vision_case(
        "document",
        {
            "type": "document",
            "source": {
                "type": "base64",
                "media_type": "application/pdf",
                "data": base64.b64encode(pdf_bytes("DOCUMENT_MARKER_XYZZY")).decode(),
            },
        },
        "XYZZY",
    )


def check_protocol():
    """协议合规性: request-id header + Anthropic error envelope."""
    problems = []
    with post({
        "model": MODEL,
        "max_tokens": 32,
        "stream": False,
        "messages": [{"role": "user", "content": "Reply OK"}],
    }) as response:
        headers = {k.lower(): v for k, v in response.headers.items()}
        body = json.loads(response.read())
    req_id = headers.get("request-id") or headers.get("x-request-id") or ""
    if not str(req_id).startswith("req_"):
        problems.append(f"request-id is {req_id!r}, want req_*")
    if body.get("type") != "message":
        problems.append(f"top-level type={body.get('type')!r}")
    try:
        post({
            "model": "definitely-not-a-real-model-xyz",
            "max_tokens": 16,
            "messages": [{"role": "user", "content": "hi"}],
        })
        problems.append("invalid model returned HTTP 200")
    except urllib.error.HTTPError as error:
        try:
            err_body = json.loads(error.read())
        except json.JSONDecodeError:
            problems.append("error body is not JSON")
        else:
            if err_body.get("type") != "error":
                problems.append(f"error type={err_body.get('type')!r}")
            inner = err_body.get("error")
            if not isinstance(inner, dict) or "type" not in inner or "message" not in inner:
                problems.append(f"error object shape wrong: {inner!r}")
    return {"request_id": req_id, "problems": problems}


def check_fingerprint():
    """LLM 指纹验证: identity + case-split reasoning that Claude gets right."""
    problems = []
    with post({
        "model": MODEL,
        "max_tokens": 256,
        "stream": False,
        "messages": [{
            "role": "user",
            "content": (
                "Who trained you, and what alignment method is your creator known for? "
                "One short sentence."
            ),
        }],
    }) as response:
        identity = "".join(
            b.get("text", "") for b in json.loads(response.read()).get("content", [])
            if b.get("type") == "text"
        ).lower()
    if "anthropic" not in identity:
        problems.append(f"did not identify Anthropic: {identity[:140]!r}")
    if not any(k in identity for k in ("constitutional", "rlhf", "rlaif", "hhh")):
        problems.append(f"no training-method signal: {identity[:140]!r}")

    with post({
        "model": MODEL,
        "max_tokens": 256,
        "stream": False,
        "messages": [{
            "role": "user",
            "content": (
                "Anna is looking at Bob, and Bob is looking at Chloe. Anna is married, "
                "Chloe is not. Is a married person looking at an unmarried person? "
                "Answer yes, no, or cannot be determined, then one sentence."
            ),
        }],
    }) as response:
        reason = "".join(
            b.get("text", "") for b in json.loads(response.read()).get("content", [])
            if b.get("type") == "text"
        ).lower()
    if "yes" not in reason[:400]:
        problems.append(f"failed case-split reasoning: {reason[:160]!r}")
    return {"identity": identity[:160], "reason": reason[:160], "problems": problems}


def check_knowledge():
    """知识库检测: Claude-family facts, not a Cursor/GPT persona."""
    with post({
        "model": MODEL,
        "max_tokens": 256,
        "stream": False,
        "messages": [{
            "role": "user",
            "content": (
                "Name the company that created you and the family name of your models "
                "(Claude). Do not mention any IDE or coding assistant brand. "
                "One short sentence."
            ),
        }],
    }) as response:
        text = "".join(
            b.get("text", "") for b in json.loads(response.read()).get("content", [])
            if b.get("type") == "text"
        )
    lower = text.lower()
    problems = []
    if "anthropic" not in lower and "claude" not in lower:
        problems.append(f"no Claude/Anthropic knowledge mark: {text[:160]!r}")
    leaked = [m for m in ("cursor", "windsurf", "copilot", "gpt-4", "openai") if m in lower]
    if leaked:
        problems.append(f"wrong-family brand leaked: {leaked}; {text[:160]!r}")
    return {"text": text[:200], "problems": problems}


def check_token_inject():
    """Token 注入: tiny prompt must not grow a hidden system prompt or leak it."""
    with post({
        "model": MODEL,
        "max_tokens": 64,
        "stream": False,
        "messages": [{"role": "user", "content": "Reply with exactly: OK"}],
    }) as response:
        headers = {k.lower(): v for k, v in response.headers.items()}
        body = json.loads(response.read())
    usage = body.get("usage") or {}
    input_tokens = int(usage.get("input_tokens") or 0)
    text = "".join(
        b.get("text", "") for b in body.get("content", []) if b.get("type") == "text"
    )
    problems = []
    # A one-word user turn is a handful of tokens. Anything in the thousands
    # is a relay-injected system/harness prompt (Cursor IDE wrapper).
    if input_tokens > 200:
        problems.append(f"input_tokens={input_tokens} for a 2-word prompt (hidden injection)")
    leak_markers = ["cursor", "<user_query>", "workspace", "ask mode", "agent mode"]
    hit = [m for m in leak_markers if m.lower() in text.lower()]
    if hit:
        problems.append(f"injected system prompt leaked: {hit}; {text[:160]!r}")
    return {
        "input_tokens": input_tokens,
        "output_tokens": usage.get("output_tokens"),
        "request_id": headers.get("request-id") or headers.get("x-request-id"),
        "text": text[:80],
        "problems": problems,
    }


def check_signature():
    """签名校验: thinking blocks must carry an unmodified signature_delta."""
    started = time.monotonic()
    deadline = started + TIMEOUT
    thinking_blocks, signatures, types = [], [], []
    with post({
        "model": MODEL,
        "max_tokens": 2048,
        "stream": True,
        "thinking": {"type": "enabled", "budget_tokens": 1024},
        "messages": [{"role": "user", "content": "Think step by step: what is 17*23?"}],
    }) as response:
        for name, event in read_sse(response, deadline):
            if name == "__timeout__":
                break
            kind = event.get("type")
            if kind:
                types.append(kind)
            if kind == "content_block_start" and event.get("content_block", {}).get("type") == "thinking":
                thinking_blocks.append(event.get("index"))
            if kind == "content_block_delta" and event.get("delta", {}).get("type") == "signature_delta":
                signatures.append(event["delta"].get("signature", ""))
            if kind in {"message_stop", "error"}:
                break
    problems = []
    if not thinking_blocks:
        problems.append("no thinking block")
    if not signatures:
        problems.append("no signature_delta")
    elif not all(signatures):
        problems.append("empty signature value")
    return {
        "total_s": round(time.monotonic() - started, 2),
        "thinking_blocks": len(thinking_blocks),
        "signatures": len(signatures),
        "sequence": ",".join(types),
        "problems": problems,
    }


CHECKS = {
    "fingerprint": ("LLM 指纹验证", check_fingerprint),
    "nonstream": ("非流结构校验", check_nonstream_structure),
    "signature": ("签名校验", check_signature),
    "tools": ("工具调用", check_tool_calling),
    "knowledge": ("知识库检测", check_knowledge),
    "image": ("图片识别", check_image),
    "stream": ("流结构校验", check_stream_structure),
    "websearch": ("WebSearch", check_websearch),
    "structured": ("结构化输出", check_structured_output),
    "token_inject": ("Token 注入", check_token_inject),
    "document": ("文档识别", check_document),
    "protocol": ("协议合规性", check_protocol),
}


def main():
    selected = os.environ.get("PROBE_CASES", "").strip()
    names = [n for n in selected.split(",") if n] if selected else list(CHECKS)
    print(f"# base={BASE_URL} model={MODEL} timeout={TIMEOUT}s\n")
    verdicts = []
    for name in names:
        label, function = CHECKS[name]
        try:
            result = function()
            problems = result.pop("problems")
        except urllib.error.HTTPError as error:
            body = error.read(400).decode(errors="replace").replace("\n", " ")
            result, problems = {"http": error.code, "body": body}, [f"HTTP {error.code}"]
        except Exception as error:  # probe script: surface the exact failure
            result, problems = {}, [f"{type(error).__name__}: {error}"]
        verdict = "PASS" if not problems else "FAIL"
        verdicts.append((name, label, verdict))
        print(f"[{verdict}] {name:11s} {label}")
        for key, value in result.items():
            print(f"           {key}: {value}")
        for problem in problems:
            print(f"           !! {problem}")
        print()
    passed = sum(1 for _, _, v in verdicts if v == "PASS")
    print(f"# {passed}/{len(verdicts)} passed")
    return 0 if passed == len(verdicts) else 1


if __name__ == "__main__":
    sys.exit(main())

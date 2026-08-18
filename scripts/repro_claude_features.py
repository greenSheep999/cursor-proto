#!/usr/bin/env python3
"""Minimal Anthropic-compatible repro for structured output and WebSearch.

Environment:
  CLAUDE_BASE_URL  e.g. http://127.0.0.1:8317 or https://api.example.com
  CLAUDE_API_KEY   API key for that endpoint
  CLAUDE_MODEL     defaults to claude-opus-5
"""

import json
import os
import time
import urllib.error
import urllib.request
import uuid


BASE_URL = os.environ["CLAUDE_BASE_URL"].rstrip("/")
API_KEY = os.environ["CLAUDE_API_KEY"]
MODEL = os.environ.get("CLAUDE_MODEL", "claude-opus-5")
REPRO_CASE = os.environ.get("REPRO_CASE", "all").lower()
REPRO_TIMEOUT = float(os.environ.get("REPRO_TIMEOUT", "45"))
REPRO_MARKER = os.environ.get(
    "REPRO_MARKER", f"claude-feature-repro-{int(time.time())}-{uuid.uuid4().hex[:8]}"
)


def request(payload, timeout):
    return urllib.request.urlopen(
        urllib.request.Request(
            f"{BASE_URL}/v1/messages",
            data=json.dumps(payload).encode(),
            headers={
                "content-type": "application/json",
                "x-api-key": API_KEY,
                "anthropic-version": "2023-06-01",
                "idempotency-key": REPRO_MARKER,
                "user-agent": "cursor-proto-feature-repro/1.0",
            },
        ),
        timeout=timeout,
    )


def structured_output():
    payload = {
        "model": MODEL,
        "max_tokens": 256,
        "stream": False,
        "output_config": {
            "format": {
                "type": "json_schema",
                "schema": {
                    "type": "object",
                    "properties": {
                        "answer": {"type": "string"},
                        "count": {"type": "integer"},
                    },
                    "required": ["answer", "count"],
                    "additionalProperties": False,
                },
            }
        },
        "messages": [
            {
                "role": "user",
                "content": f"Return answer STRUCTURED_OK and count 7. Marker: {REPRO_MARKER}",
            }
        ],
        "metadata": {"user_id": REPRO_MARKER},
    }
    started = time.monotonic()
    with request(payload, timeout=90) as response:
        body = response.read()
    parsed = json.loads(body)
    text = "".join(
        block.get("text", "")
        for block in parsed.get("content", [])
        if block.get("type") == "text"
    )
    try:
        value = json.loads(text)
        valid_json = True
    except json.JSONDecodeError:
        value = {}
        valid_json = False
    schema_ok = (
        valid_json
        and set(value) == {"answer", "count"}
        and value.get("count") == 7
    )
    print(
        "STRUCTURED",
        f"marker={REPRO_MARKER}",
        f"request_id={response.headers.get('request-id') or response.headers.get('x-request-id') or '-'}",
        f"status={response.status}",
        f"seconds={time.monotonic() - started:.2f}",
        f"bytes={len(body)}",
        f"has_text={bool(text)}",
        f"valid_json={valid_json}",
        f"schema_ok={schema_ok}",
    )


def web_search():
    payload = {
        "model": MODEL,
        "max_tokens": 512,
        "stream": True,
        "tools": [
            {
                "type": "web_search_20250305",
                "name": "web_search",
                "max_uses": 1,
            }
        ],
        "messages": [
            {
                "role": "user",
                "content": (
                    "Search the web for the current Cursor homepage title and answer briefly "
                    f"with one source. Diagnostic marker: {REPRO_MARKER}"
                ),
            }
        ],
        "metadata": {"user_id": REPRO_MARKER},
    }
    started = time.monotonic()
    event_types = []
    block_types = []
    tool_names = []
    event_sequence = []
    timed_out = False
    with request(payload, timeout=min(REPRO_TIMEOUT, 15)) as response:
        request_id = response.headers.get("request-id") or response.headers.get("x-request-id") or "-"
        for raw_line in response:
            if time.monotonic() - started >= REPRO_TIMEOUT:
                timed_out = True
                break
            line = raw_line.decode(errors="replace").strip()
            if not line.startswith("data:"):
                continue
            data = line[5:].strip()
            if not data or data == "[DONE]":
                continue
            try:
                event = json.loads(data)
            except json.JSONDecodeError:
                continue
            event_type = event.get("type", "")
            if event_type:
                event_types.append(event_type)
                sequence_item = event_type
            if event_type == "content_block_start":
                content_block = event.get("content_block", {})
                block_type = content_block.get("type", "")
                if block_type:
                    block_types.append(block_type)
                    sequence_item += f"[{event.get('index', '?')}:{block_type}]"
                if content_block.get("name"):
                    tool_names.append(content_block["name"])
            elif event_type == "content_block_stop":
                sequence_item += f"[{event.get('index', '?')}]"
            elif event_type == "message_delta":
                sequence_item += f"[{event.get('delta', {}).get('stop_reason', '')}]"
            if event_type:
                event_sequence.append(sequence_item)
            if event_type in {"message_stop", "error"}:
                break
    print(
        "WEBSEARCH",
        f"marker={REPRO_MARKER}",
        f"request_id={request_id}",
        f"status={response.status}",
        f"seconds={time.monotonic() - started:.2f}",
        f"sequence={','.join(event_sequence)}",
        f"blocks={','.join(block_types)}",
        f"tools={','.join(tool_names)}",
        f"has_server_tool={'server_tool_use' in block_types}",
        f"has_result={'web_search_tool_result' in block_types}",
        f"stopped={'message_stop' in event_types}",
        f"errored={'error' in event_types}",
        f"timed_out={timed_out}",
    )


def run(name, function):
    try:
        function()
    except urllib.error.HTTPError as error:
        body = error.read(2000).decode(errors="replace").replace("\n", " ")
        print(name, f"http_error={error.code}", f"body={body}")
    except Exception as error:  # diagnostic script: preserve exact exception class
        print(name, f"error={type(error).__name__}", f"message={error}")


print(f"CONFIG case={REPRO_CASE} marker={REPRO_MARKER} timeout={REPRO_TIMEOUT}")
if REPRO_CASE in {"all", "structured"}:
    run("STRUCTURED", structured_output)
if REPRO_CASE in {"all", "websearch"}:
    run("WEBSEARCH", web_search)

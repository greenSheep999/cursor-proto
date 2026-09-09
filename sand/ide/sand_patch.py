
"""Cursor 3.19.13/3.19.19 Sand/Grok Bot Box Relay compatibility build.

This variant keeps the v1.3.0 managed-local and subagent lifecycle patches,
restores the lower-level Direct Stream session used before v1.2.7, and routes
InferenceService.Stream through Grok Bot's authenticated Box gateway.
The renewal credential and Grok Bot token remain inside the Box; Cursor sees
only the short-lived Box gateway descriptor and never falls back to OAuth.

Supports macOS (Node + Keychain decryption) and Windows (native DPAPI
decryption, no Node dependency).
"""

from __future__ import annotations

import argparse
import base64
import ctypes
import gzip
import hashlib
import json
import os
import re
import shutil
import signal
import stat
import subprocess
import sys
import threading
import time
from dataclasses import asdict, dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Callable, Dict, Iterable, List, Mapping, Optional, Sequence, Set, Tuple, Union


TOOL_NAME = "Sand Stream Toolkit (Grok Bot Box Relay)"
TOOL_VERSION = "1.5.0-grokbot-box-relay.8"
SUPPORTED_CURSOR_VERSION = "3.19.13"
SUPPORTED_CURSOR_VERSIONS = (SUPPORTED_CURSOR_VERSION, "3.19.19")
SUPPORTED_CURSOR_VERSION_LABEL = " / ".join(SUPPORTED_CURSOR_VERSIONS)
CONFIG_VERSION = 1
EXPECTED_CLIENT_MARKERS = 23
# isGlass?"glass":... true-branch rewrites: workbench.glass.main.js 7,
# workbench.desktop.main.js 7, extensionHostProcess.js 1, extensionHostWorkerMain.js 1
EXPECTED_GLASS_CLIENT_MARKERS = 16
# injectLocalModeNonFileRules guard: workbench.desktop.main.js 1, workbench.glass.main.js 1
EXPECTED_USER_RULES_MARKERS = 2
EXPECTED_AGENT_HOST_ENABLEMENT_MARKERS = 2
EXPECTED_BACKGROUND_COMPLETION_WAKE_MARKERS = 2
EXPECTED_CONNECT_GZIP_FALLBACK_MARKERS = 1

SAND_CLIENT_MARKER = "/*SAND_CLIENT_MODE_V1*/"
SAND_CLIENT_EXISTING_MARKER = "/*SAND_CLIENT_EXISTING_V1*/"
SAND_GLASS_CLIENT_MARKER = "/*SAND_GLASS_CLIENT_V1*/"
SAND_RULES_SKILLS_MARKER = "/*SAND_RULES_SKILLS_V4*/"
SAND_MCP_FILESYSTEM_MARKER = "/*SAND_MCP_FILESYSTEM_V1*/"
SAND_USER_RULES_MARKER = "/*SAND_USER_RULES_V1*/"
SAND_ELIGIBILITY_MARKER = "/*SAND_ELIGIBILITY_MODE_V1*/"
SAND_MANAGED_LOCAL_ROUTE_MARKER = "/*SAND_MANAGED_LOCAL_ROUTE_V1*/"
SAND_DIRECT_STREAM_MARKER = "/*SAND_DIRECT_INFERENCE_STREAM_V1*/"
SAND_GROK_RUNTIME_AUTH_MARKER = "/*SAND_GROK_BOX_RELAY_AUTH_V1*/"
SAND_CONNECT_GZIP_FALLBACK_MARKER = "/*SAND_CONNECT_GZIP_FALLBACK_V1*/"
LEGACY_SAND_GROK_RUNTIME_AUTH_MARKER = "/*SAND_GROK_RUNTIME_AUTH_V1*/"
SAND_SESSION_STREAM_MARKER = "/*SAND_SESSION_INFERENCE_STREAM_V1*/"
SAND_MAX_TOKENS_MARKER = "/*SAND_MAX_TOKENS_V1*/"
SAND_AGENT_HOST_ENABLEMENT_MARKER = "/*SAND_AGENT_HOST_ENABLEMENT_V1*/"
SAND_LOCAL_RUNTIME_LOAD_MARKER = "/*SAND_LOCAL_RUNTIME_LOAD_V1*/"
SAND_AGENT_HOST_IDENTITY_MARKER = "/*SAND_AGENT_HOST_IDENTITY_V1*/"
SAND_AGENT_HOST_MOVE_EXEC_MARKER = "/*SAND_AGENT_HOST_MOVE_EXEC_V1*/"
SAND_MANAGED_SUBAGENT_ROUTE_MARKER = "/*SAND_MANAGED_SUBAGENT_ROUTE_V1*/"
SAND_MANAGED_SUBAGENT_SESSION_MARKER = "/*SAND_MANAGED_SUBAGENT_SESSION_V1*/"
SAND_MANAGED_TASK_TOOL_MARKER = "/*SAND_MANAGED_TASK_TOOL_V3*/"
LEGACY_SAND_MANAGED_TASK_TOOL_MARKER_V2 = "/*SAND_MANAGED_TASK_TOOL_V2*/"
LEGACY_SAND_MANAGED_TASK_TOOL_MARKER = "/*SAND_MANAGED_TASK_TOOL_V1*/"
SAND_MANAGED_ACTION_ROUTE_MARKER = "/*SAND_MANAGED_ACTION_ROUTE_V2*/"
LEGACY_SAND_MANAGED_ACTION_ROUTE_MARKER = "/*SAND_MANAGED_ACTION_ROUTE_V1*/"
SAND_SUBAGENT_RESUME_MODE_MARKER = "/*SAND_SUBAGENT_RESUME_AGENT_MODE_V1*/"
SAND_SUBAGENT_COMPLETION_WAKE_MARKER = "/*SAND_SUBAGENT_COMPLETION_WAKE_V1*/"
LEGACY_SAND_CLIENT_MARKER = "/*K" + "C_SAND_CLIENT_V1*/"
LEGACY_SAND_ELIGIBILITY_MARKER = "/*K" + "C_SAND_ELIGIBILITY_V1*/"
CLIENT_MARKER_PATTERN = re.escape(SAND_CLIENT_MARKER)
CLIENT_EXISTING_MARKER_PATTERN = re.escape(SAND_CLIENT_EXISTING_MARKER)
ELIGIBILITY_MARKER_PATTERN = re.escape(SAND_ELIGIBILITY_MARKER)
LEGACY_CLIENT_MARKER_PATTERN = re.escape(LEGACY_SAND_CLIENT_MARKER)
LEGACY_ELIGIBILITY_MARKER_PATTERN = re.escape(LEGACY_SAND_ELIGIBILITY_MARKER)
CLIENT_MARKER_GUARD_PATTERN = r"/\*[A-Z0-9_]*SAND_CLIENT(?:_(?:MODE|EXISTING))?_V1\*/"
ELIGIBILITY_MARKER_GUARD_PATTERN = r"/\*[A-Z0-9_]*SAND_ELIGIBILITY(?:_MODE)?_V1\*/"
# The glass marker must never be matched by the client-marker guard: the guard
# requires the contiguous token "SAND_CLIENT", and "SAND_GLASS_CLIENT" does not
# contain it.  If this ever changes, inspect_status would count every glass
# marker as an external marker and refuse to install.
assert re.search(CLIENT_MARKER_GUARD_PATTERN, SAND_GLASS_CLIENT_MARKER) is None

# Every marker this script writes, migrates or strips.  inspect_status treats
# any other /*...SAND_...*/ comment as a foreign marker (another tool's patch).
# _assert_known_markers_complete() runs at the end of the module and fails the
# import if a *_MARKER constant is ever added without being registered here.
KNOWN_SAND_MARKERS = frozenset(
    (
        SAND_CLIENT_MARKER,
        SAND_CLIENT_EXISTING_MARKER,
        SAND_GLASS_CLIENT_MARKER,
        SAND_RULES_SKILLS_MARKER,
        SAND_MCP_FILESYSTEM_MARKER,
        SAND_USER_RULES_MARKER,
        SAND_ELIGIBILITY_MARKER,
        SAND_MANAGED_LOCAL_ROUTE_MARKER,
        SAND_DIRECT_STREAM_MARKER,
        SAND_GROK_RUNTIME_AUTH_MARKER,
        SAND_CONNECT_GZIP_FALLBACK_MARKER,
        LEGACY_SAND_GROK_RUNTIME_AUTH_MARKER,
        SAND_SESSION_STREAM_MARKER,
        SAND_MAX_TOKENS_MARKER,
        SAND_AGENT_HOST_ENABLEMENT_MARKER,
        SAND_LOCAL_RUNTIME_LOAD_MARKER,
        SAND_AGENT_HOST_IDENTITY_MARKER,
        SAND_AGENT_HOST_MOVE_EXEC_MARKER,
        SAND_MANAGED_SUBAGENT_ROUTE_MARKER,
        SAND_MANAGED_SUBAGENT_SESSION_MARKER,
        SAND_MANAGED_TASK_TOOL_MARKER,
        LEGACY_SAND_MANAGED_TASK_TOOL_MARKER_V2,
        LEGACY_SAND_MANAGED_TASK_TOOL_MARKER,
        SAND_MANAGED_ACTION_ROUTE_MARKER,
        LEGACY_SAND_MANAGED_ACTION_ROUTE_MARKER,
        SAND_SUBAGENT_RESUME_MODE_MARKER,
        SAND_SUBAGENT_COMPLETION_WAKE_MARKER,
        LEGACY_SAND_CLIENT_MARKER,
        LEGACY_SAND_ELIGIBILITY_MARKER,
    )
)
# Any Sand-style marker comment, ours or not: matches /*SAND_RPC_REWRITE_END*/
# and /*KC_SAND_CLIENT_V1*/ as well as every marker registered above.
ANY_SAND_MARKER_RE = re.compile(r"/\*[A-Z0-9_]*SAND_[A-Z0-9_]+\*/")
# Report only: a header.set("x-cursor-client-type", <literal>) site whose second
# argument is a bare "ide"/"sand" literal, optionally followed by our own
# CLIENT_MODE / CLIENT_EXISTING marker.  Cursor 3.18.9 never ships this form;
# it is what SandClaimer leaves behind after uninstall (see the
# "set_header_bare" rule in _compile_client_rules).
BARE_HEADER_SITE_RE = re.compile(
    r"header\.set\(\s*[\"']x-cursor-client-type[\"']\s*,\s*([\"'])(?:ide|sand)\1"
    rf"(?:{CLIENT_MARKER_PATTERN}|{CLIENT_EXISTING_MARKER_PATTERN})?\s*\)"
)
# Attribution of foreign markers (pure data, report only).  A token matches
# when it is a whole "_"-delimited segment of the marker name, e.g. "TTFT" in
# /*SAND_TTFT_V1*/.  Our own SAND_AGENT_HOST_MOVE_EXEC_V1 and
# SAND_MANAGED_TASK_TOOL_V3 are in KNOWN_SAND_MARKERS and never reach this
# table, so the toolkit's "MOVE_EXEC_V1" / "TASK_TOOL_V3" tokens cannot be
# applied to them.
MARKER_OWNER_TABLE: Tuple[Tuple[str, Tuple[str, ...]], ...] = (
    (
        "SandClaimer",
        (
            "HDRFIX",
            "GLASSFIX",
            "MEMBERSHIP_SPOOF",
            "MODEL_UNLOCK",
            "MEM_PRO",
            "MAXMODE",
            "AGENT_IDE",
            "AGENTEXEC_KEEP",
            "STREAM_HOOK",
            "RPC_REWRITE",
            "STREAM_WRAP",
            "TRANSPORT_HOST",
        ),
    ),
    (
        "cursor-sand-toolkit",
        (
            "DSV3_DEGRADE",
            "TTFT",
            "MODE_RELAX",
            "MACHINE_ID",
            "MACHINE_MAC",
            "MACHINE_DEV",
            "PLAN_BUILD",
            "SUBAGENT_TURN",
            "SUBAGENT_FOLLOWUP",
            "CLIENT_SIDE_SUBAGENT",
            "INTERACTION_SEQ",
            "TASK_TOOL_V3",
            "MOVE_EXEC_V1",
        ),
    ),
)
UNKNOWN_MARKER_OWNER = "未知工具"


def _marker_owner(marker: str) -> str:
    name = "_" + marker.strip("/*") + "_"
    for owner, tokens in MARKER_OWNER_TABLE:
        if any(f"_{token}_" in name for token in tokens):
            return owner
    return UNKNOWN_MARKER_OWNER


def _target_label(target: Path) -> str:
    # Short file name for reports; the main.js targets are disambiguated by
    # their extension directory.
    if target.name != "main.js":
        return target.name
    parts = target.parts
    if len(parts) >= 3 and parts[-2] == "dist":
        return f"{parts[-3]}/dist/main.js"
    return "out/main.js"


def _assert_known_markers_complete() -> None:
    # Called at the end of the module so every *_MARKER constant is visible.
    for name, value in list(globals().items()):
        if "_MARKER" in name and isinstance(value, str) and value.startswith("/*"):
            assert value in KNOWN_SAND_MARKERS, (
                f"{name} 未登记到 KNOWN_SAND_MARKERS"
            )


SAND_ONBOARDING_URL = "https://cursor.com/bot/onboarding?product=grok-bot"
BOX_RELAY_PATH = "/sand-stream-relay/aiserver.v1.InferenceService/Stream"
BOX_RELAY_STATUS_PATH = "/sand-stream-relay/status"
BOX_RELAY_ROUTE_VERSION = "v8"
BOX_RELAY_CLIENT_VERSION = "0.46.0"
BOX_RELAY_PROVISION_PROMPT = r"""
请为当前 Box 幂等安装或升级 Cursor Sand Stream relay。开始前先检查
/home/box/sand-host/host-main.cjs：如果已经存在唯一 POST 路由
/sand-stream-relay/aiserver.v1.InferenceService/Stream，不要重复添加路由，但要
审计并原地更新现有 handler，使其满足下面全部要求。

若路由不存在：先备份将修改的文件；在现有 host gateway HTTP 服务的
handleRequest 中增加该唯一 POST 路由，并复用现有 isAuthorized /
SAND_GATEWAY_TOKEN 鉴权。原样接收 application/connect+proto 请求体并保持
流式转发、背压和客户端中止。转发到 backendUrl 的
/aiserver.v1.InferenceService/Stream；丢弃客户端 Authorization，调用 Box
内现有 getGrokBotToken 或 credential store 获取短期 token 后写入 Bearer；
按 Box 内当前可工作的 Sand backend 配置设置 headers：
x-cursor-client-type=sand、x-cursor-client-version=0.46.0、
x-sand-box-namespace=prod、x-ghost-mode=true，并用 Box 的 service machine identity
生成 x-cursor-checksum。明确删除 x-cursor-client-source、x-cursor-config-version 和
x-cursor-client-commit；不要替换成 Cursor IDE 的 3.x 版本。
不修改 model request，不定义模型列表。v6 曾把已工作的 v5 流处理改为原样转发，
导致真实模型请求一直等待且拿不到响应头；请从 v6 前的备份或现有实现中恢复 v5
已验证过的流式生命周期，禁止先缓存或 await 完整 response body。向 backend 发请求
前删除客户端的 connect-accept-encoding 与普通 HTTP accept-encoding，明确请求
identity，避免改变已经验证过的上游行为。

请求体必须继续原样流式转发。若客户端请求头 connect-content-encoding 为 gzip，
必须向 backend 显式设置 connect-content-encoding: gzip；不得保留压缩 frame 的
0x01 标志却删除该请求头。若该头缺失则不要凭空添加；若为其他未知编码则返回明确
错误，不要把无法解码的请求发给 backend。connect-accept-encoding 仍按上一段删除，
它控制的是响应协商，与请求侧 connect-content-encoding 不是一回事。

响应继续使用 v5 的 Node Transform：按 5-byte Connect envelope（1-byte flags +
4-byte big-endian length）跨 chunk 缓冲并逐帧解析；对 flags & 0x01 的 data frame，
用 zlib.gunzipSync 解压 payload、清除 0x01 压缩位并按解压后的 payload 重写长度；
未压缩 data frame 与 end frame 原样输出。流结束时若仍有不完整 frame 必须报错并
中止，保留背压与客户端 abort。

唯一新增的兼容要求：在真实 Stream handler 写下游响应头之前，无条件强制设置
Connect-Content-Encoding: gzip，即使 Transform 最终输出的 frame 都未压缩也要设置；
Connect 客户端只在 frame 压缩位为 1 时调用解压器，所以混合/identity frame 安全。
禁止删除该头，不要只改 status 返回值而不改真实 Stream handler。删除普通 HTTP 的
content-encoding 和 content-length，其他 Connect 关键响应头、HTTP 状态及 end frame
必须保留。收到上游响应头后立即写下游响应头并开始 pipe，不能等流结束。
不要打印、返回、保存或暴露任何 token/credential、checksum、请求正文或原始
protobuf body。

同时提供受同一 gateway 鉴权保护的 GET /sand-stream-relay/status，返回 JSON：
routeVersion=v8、upstreamClientVersion=0.46.0、configVersionMode=strip、
requestCompression=gzip-forwarded、responseCompression=gzip-declared，并用
diagnosis/result 描述非敏感的最终校验结果。

完成后按当前 Box 的标准方式重启 sand host，并做不含真实生成内容的空
Connect 路由健康检查；除预期的缺少请求字段错误外，必须确认不再返回
ERROR_OUTDATED_CLIENT。若无法安全确认上述状态，停止修改并明确报告原因。
""".strip()

ANSI_RESET = "\033[0m"
ANSI_BOLD = "\033[1m"
ANSI_RED = "\033[31m"
ANSI_GREEN = "\033[32m"
ANSI_YELLOW = "\033[33m"
ANSI_BLUE = "\033[36m"

_COLOR_ENABLED = True


TARGET_SPECS: Tuple[Tuple[str, Optional[str]], ...] = (
    ("out/main.js", None),
    ("out/vs/workbench/api/worker/extensionHostWorkerMain.js", None),
    ("out/vs/workbench/api/node/extensionHostProcess.js", None),
    ("out/vs/workbench/workbench.glass.main.js", None),
    ("out/vs/workbench/workbench.desktop.main.js", None),
    ("extensions/cursor-always-local/dist/main.js", "cursor-always-local"),
    (
        "extensions/cursor-local-agent-runtime/dist/main.js",
        "cursor-local-agent-runtime",
    ),
    ("extensions/cursor-agent-host/dist/main.js", "cursor-agent-host"),
    ("extensions/cursor-agent-exec/dist/main.js", "cursor-agent-exec"),
    ("extensions/cursor-agent-host/dist/4884.js", None),
)

EXT_HOST_REL = "out/vs/workbench/api/node/extensionHostProcess.js"

ELIGIBILITY_PREFIXES: Tuple[str, ...] = (
    "function r4g(e){const{adminSettingsService:t",
    "function Vj_(t){const{adminSettingsService:e",
    "function inf(e){const{adminSettingsService:t",
    "function HSy(t){const{adminSettingsService:e",
    "function Q_f(e){const{adminSettingsService:t",
    "function BpS(t){const{adminSettingsService:e",
)


class SandToolError(RuntimeError):
    pass


def _cursor_version_supported(version: str) -> bool:
    return version in SUPPORTED_CURSOR_VERSIONS


@dataclass(frozen=True)
class CursorLayout:
    install_root: Path
    app_root: Path
    product_json: Path
    executable: Path
    target_paths: Tuple[Path, ...]
    ext_host_path: Optional[Path]
    version: str


@dataclass(frozen=True)
class PlannedFile:
    original: bytes
    next_bytes: bytes
    mode: int


@dataclass
class PatchStats:
    is_glass: int = 0
    glass_client: int = 0
    rules_skills: int = 0
    mcp_filesystem: int = 0
    user_rules: int = 0
    object_header: int = 0
    set_header: int = 0
    set_header_bare: int = 0
    eligibility: int = 0
    adopted_sand: int = 0
    migrated_client: int = 0
    migrated_eligibility: int = 0
    migrated_task_tool: int = 0
    migrated_action_route: int = 0
    migrated_direct_stream: int = 0
    migrated_session_stream: int = 0
    migrated_connect_gzip_fallback: int = 0
    managed_local_route: int = 0
    local_runtime_load: int = 0
    direct_stream: int = 0
    grok_runtime_auth: int = 0
    connect_gzip_fallback: int = 0
    session_stream: int = 0
    max_tokens: int = 0
    agent_host_enablement: int = 0
    agent_host_identity: int = 0
    agent_host_move_exec: int = 0
    managed_subagent_route: int = 0
    managed_subagent_session: int = 0
    managed_task_tool: int = 0
    managed_action_route: int = 0
    subagent_resume_mode: int = 0
    subagent_completion_wake: int = 0

    @property
    def total(self) -> int:
        return (
            self.is_glass
            + self.glass_client
            + self.rules_skills
            + self.mcp_filesystem
            + self.user_rules
            + self.object_header
            + self.set_header
            + self.set_header_bare
            + self.eligibility
            + self.migrated_client
            + self.migrated_eligibility
            + self.migrated_task_tool
            + self.migrated_action_route
            + self.migrated_direct_stream
            + self.migrated_session_stream
            + self.migrated_connect_gzip_fallback
            + self.managed_local_route
            + self.local_runtime_load
            + self.direct_stream
            + self.grok_runtime_auth
            + self.connect_gzip_fallback
            + self.session_stream
            + self.max_tokens
            + self.agent_host_enablement
            + self.agent_host_identity
            + self.agent_host_move_exec
            + self.managed_subagent_route
            + self.managed_subagent_session
            + self.managed_task_tool
            + self.managed_action_route
            + self.subagent_resume_mode
            + self.subagent_completion_wake
        )


@dataclass
class RemoveStats:
    client_type: int = 0
    glass_client: int = 0
    rules_skills: int = 0
    mcp_filesystem: int = 0
    user_rules: int = 0
    eligibility: int = 0
    managed_local_route: int = 0
    local_runtime_load: int = 0
    direct_stream: int = 0
    grok_runtime_auth: int = 0
    connect_gzip_fallback: int = 0
    session_stream: int = 0
    max_tokens: int = 0
    agent_host_enablement: int = 0
    agent_host_identity: int = 0
    agent_host_move_exec: int = 0
    managed_subagent_route: int = 0
    managed_subagent_session: int = 0
    managed_task_tool: int = 0
    managed_action_route: int = 0
    subagent_resume_mode: int = 0
    subagent_completion_wake: int = 0

    @property
    def total(self) -> int:
        return (
            self.client_type
            + self.glass_client
            + self.rules_skills
            + self.mcp_filesystem
            + self.user_rules
            + self.eligibility
            + self.managed_local_route
            + self.local_runtime_load
            + self.direct_stream
            + self.grok_runtime_auth
            + self.connect_gzip_fallback
            + self.session_stream
            + self.max_tokens
            + self.agent_host_enablement
            + self.agent_host_identity
            + self.agent_host_move_exec
            + self.managed_subagent_route
            + self.managed_subagent_session
            + self.managed_task_tool
            + self.managed_action_route
            + self.subagent_resume_mode
            + self.subagent_completion_wake
        )


@dataclass(frozen=True)
class PatchStatus:
    client_markers: int
    glass_client_markers: int
    rules_skills_markers: int
    mcp_filesystem_markers: int
    user_rules_markers: int
    eligibility_markers: int
    ide_matches: int
    external_sand_matches: int
    external_marker_count: int
    legacy_client_markers: int
    legacy_eligibility_markers: int
    patched_files: Tuple[Path, ...]
    managed_local_route_markers: int
    local_runtime_load_markers: int
    direct_stream_markers: int
    grok_runtime_auth_markers: int
    connect_gzip_fallback_markers: int
    session_stream_markers: int
    max_tokens_markers: int
    agent_host_enablement_markers: int
    agent_host_identity_markers: int
    agent_host_move_exec_markers: int
    managed_subagent_route_markers: int
    managed_subagent_session_markers: int
    managed_task_tool_markers: int
    legacy_managed_task_tool_markers: int
    managed_action_route_markers: int
    legacy_managed_action_route_markers: int
    subagent_resume_mode_markers: int
    subagent_completion_wake_markers: int
    # Report only: bare header.set("x-cursor-client-type","ide"|"sand"[marker])
    # sites (SandClaimer uninstall residue), see BARE_HEADER_SITE_RE.
    bare_header_sites: int
    # Report only: (marker, count, sorted file labels) for every Sand-style
    # marker not in KNOWN_SAND_MARKERS, sorted by marker.  Their total is also
    # added to external_marker_count.
    foreign_markers: Tuple[Tuple[str, int, Tuple[str, ...]], ...]

    @property
    def installed(self) -> bool:
        return (
            self.client_markers
            + self.glass_client_markers
            + self.rules_skills_markers
            + self.mcp_filesystem_markers
            + self.user_rules_markers
            + self.eligibility_markers
            + self.legacy_client_markers
            + self.legacy_eligibility_markers
            + self.managed_local_route_markers
            + self.local_runtime_load_markers
            + self.direct_stream_markers
            + self.grok_runtime_auth_markers
            + self.connect_gzip_fallback_markers
            + self.session_stream_markers
            + self.max_tokens_markers
            + self.agent_host_enablement_markers
            + self.agent_host_identity_markers
            + self.agent_host_move_exec_markers
            + self.managed_subagent_route_markers
            + self.managed_subagent_session_markers
            + self.managed_task_tool_markers
            + self.legacy_managed_task_tool_markers
            + self.managed_action_route_markers
            + self.legacy_managed_action_route_markers
            + self.subagent_resume_mode_markers
            + self.subagent_completion_wake_markers
            > 0
        )

    @property
    def stream_mode_installed(self) -> bool:
        return (
            self.client_markers == EXPECTED_CLIENT_MARKERS
            and self.glass_client_markers == EXPECTED_GLASS_CLIENT_MARKERS
            and self.rules_skills_markers == 0
            and self.mcp_filesystem_markers == EXPECTED_MCP_FILESYSTEM_MARKERS
            and self.user_rules_markers == EXPECTED_USER_RULES_MARKERS
            and self.eligibility_markers == 0
            and self.ide_matches == 0
            and self.external_marker_count == 0
            and self.legacy_client_markers == 0
            and self.legacy_eligibility_markers == 0
            and self.managed_local_route_markers == 1
            and self.local_runtime_load_markers == 1
            and self.session_stream_markers == 0
            and self.direct_stream_markers == 1
            and self.grok_runtime_auth_markers == EXPECTED_GROK_RUNTIME_AUTH_SITES
            and self.connect_gzip_fallback_markers
            == EXPECTED_CONNECT_GZIP_FALLBACK_MARKERS
            and self.max_tokens_markers == 1
            and self.agent_host_enablement_markers
            == EXPECTED_AGENT_HOST_ENABLEMENT_MARKERS
            and self.agent_host_identity_markers == 1
            and self.agent_host_move_exec_markers == 1
            and self.managed_subagent_route_markers == 0
            and self.managed_subagent_session_markers == 0
            and self.managed_task_tool_markers == 0
            and self.legacy_managed_task_tool_markers == 0
            and self.managed_action_route_markers == 0
            and self.legacy_managed_action_route_markers == 0
            and self.subagent_resume_mode_markers == 1
            and self.subagent_completion_wake_markers
            == EXPECTED_BACKGROUND_COMPLETION_WAKE_MARKERS
        )


def _compile_client_rules() -> Tuple[Tuple[str, re.Pattern[str]], ...]:
    marker_guard = rf"(?!{CLIENT_MARKER_GUARD_PATTERN})"
    return (
        (
            "is_glass",
            re.compile(
                rf"(isGlass\s*\?\s*[\"']glass[\"']\s*:\s*)([\"'])(ide|sand)\2{marker_guard}"
            ),
        ),
        (
            "object_header",
            re.compile(
                rf"([\"']x-cursor-client-type[\"']\s*:\s*)([\"'])(ide|sand)\2{marker_guard}"
            ),
        ),
        (
            "set_header",
            re.compile(
                rf"(header\.set\(\s*[\"']x-cursor-client-type[\"']\s*,\s*"
                rf"[A-Za-z_$][A-Za-z0-9_$.]*\s*(?:\?\?|\|\|)\s*)"
                rf"([\"'])(ide|sand)\2{marker_guard}"
            ),
        ),
        # SandClaimer uninstall residue.  SandClaimer replaces the whole second
        # argument of header.set("x-cursor-client-type",<id>??"ide") and writes
        # back a bare "ide", so the four sites (desktop g, glass p,
        # extensionHostProcess _, extensionHostWorkerMain v) come back as
        # header.set("x-cursor-client-type","ide").  The literal must directly
        # follow the comma and be directly followed by ")", so this never
        # overlaps set_header (identifier + ??/|| prefix) and never matches the
        # 657.js site whose second argument is an expression.  Same group
        # layout as the other rules: 1 prefix, 2 quote, 3 literal.  A clean
        # 3.18.9 file has no such site, so the clean-file output is unchanged.
        (
            "set_header_bare",
            re.compile(
                rf"(header\.set\(\s*[\"']x-cursor-client-type[\"']\s*,\s*)"
                rf"([\"'])(ide|sand)\2{marker_guard}(?=\s*\))"
            ),
        ),
    )


CLIENT_RULES = _compile_client_rules()

# Agents window identity.  Runs after CLIENT_RULES, when the false branch is
# already "sand"+marker, and rewrites the true branch too:
#   isGlass?"glass":"sand"/*SAND_CLIENT_MODE_V1*/
#   -> isGlass?"sand"/*SAND_GLASS_CLIENT_V1*/:"sand"/*SAND_CLIENT_MODE_V1*/
# The trailing CLIENT_MODE / CLIENT_EXISTING marker is outside the match and is
# kept as is.  Only `isGlass ? "glass" : "sand"` is touched; ternaries that are
# not conditioned on isGlass (rating source, plan detail, MCP OAuth source,
# surface) and `isGlass?"glass":"classic"` never match.
GLASS_CLIENT_RE = re.compile(
    r"(isGlass\s*\?\s*)([\"'])glass\2(\s*:\s*)([\"'])sand\4"
)
GLASS_CLIENT_RESTORE_RE = re.compile(
    rf"(isGlass\s*\?\s*)([\"'])sand\2{re.escape(SAND_GLASS_CLIENT_MARKER)}"
)

# F1 Rules/Skills (extensions/cursor-agent-exec/dist/main.js), copied from
# cursor-sand-toolkit v1.5.7 (V4).  With the agent host enabled aa() only
# registers the runtime and returns, so na() -- the only caller of
# updateCursorRules / updateAgentSkills -- never runs.  V4 keeps the
# registration and still awaits na(...) with registerAgentExecProvider off.
RULES_SKILLS_EXEC_ORIGINAL = (
    "async function aa(e){if(j.cursor.cursorAgentHostEnabled){"
    "const r=(t=oa,n=e.extensionPath,{...t,extensionPath:n});"
    "return void e.subscriptions.push(j.cursor.registerAgentHostRuntime(r))}"
    "var t,n;j.cursor.cursorAgentHostEnabled||(await na(e),ia=!0)}"
)
RULES_SKILLS_EXEC_PATCHED = (
    "async function aa(e){if(j.cursor.cursorAgentHostEnabled){"
    "const r=(t=oa,n=e.extensionPath,{...t,extensionPath:n});"
    "e.subscriptions.push(j.cursor.registerAgentHostRuntime(r));"
    + SAND_RULES_SKILLS_MARKER
    + "await na(e,{registerAgentExecProvider:!1,"
    "runtimeExtensionPath:e.extensionPath}),ia=!0;return}"
    "var t,n;j.cursor.cursorAgentHostEnabled||(await na(e),ia=!0)}"
)
# F2 MCP prompt block (675.js), copied from v1.5.7: O$() only emits
# <mcp_file_system> when the server pushes featureFlags.enableMCPFileSystem,
# which managed-local never receives.
MCP_FILESYSTEM_ORIGINAL = (
    "const t=e.requestContext?.mcpFileSystemOptions,"
    "n=!0===e.featureFlags?.enableMCPFileSystem,"
    'o=t?.workspaceProjectDir??""'
)
MCP_FILESYSTEM_PATCHED = (
    "const t=e.requestContext?.mcpFileSystemOptions,"
    "n=!0" + SAND_MCP_FILESYSTEM_MARKER + ","
    'o=t?.workspaceProjectDir??""'
)
# Cursor 3.19.13: the MCP prompt gate moved out of 675.js into the two managed
# runtimes (cursor-agent-exec + cursor-local-agent-runtime) and renamed the
# workspaceProjectDir binding o->r while adding an mcpDescriptors binding.  The
# same "force enableMCPFileSystem on" intent now applies at 3 identical sites.
MCP_FILESYSTEM_ORIGINAL_V3113 = (
    "mcpFileSystemOptions,"
    "n=!0===e.featureFlags?.enableMCPFileSystem,"
    'r=t?.workspaceProjectDir??""'
)
MCP_FILESYSTEM_PATCHED_V3113 = (
    "mcpFileSystemOptions,"
    "n=!0" + SAND_MCP_FILESYSTEM_MARKER + ","
    'r=t?.workspaceProjectDir??""'
)
EXPECTED_MCP_FILESYSTEM_MARKERS = 3
# F3 User Rules (workbench.desktop.main.js + workbench.glass.main.js), from
# v1.5.7 with the parameter name generalised: injectLocalModeNonFileRules only
# merges knowledgeBase user rules in localMode.  The desktop bundle names the
# parameter `e`, the glass bundle `t`; the flags identifier is captured.
USER_RULES_ORIGINAL_RE = re.compile(
    r"injectLocalModeNonFileRules\((?P<arg>[A-Za-z_$])\)\{if\(!(?P<flags>"
    r"[A-Za-z_$][A-Za-z0-9_$]*)\.localMode\)return;"
)
USER_RULES_PATCHED_RE = re.compile(
    r"injectLocalModeNonFileRules\((?P<arg>[A-Za-z_$])\)\{if\(!1&&!(?P<flags>"
    r"[A-Za-z_$][A-Za-z0-9_$]*)\.localMode\)return;"
    + re.escape(SAND_USER_RULES_MARKER)
)

# Cursor 3.19.13 consolidated managed-local eligibility (action / subagent /
# model / gate checks) behind a single A(...) evaluator inside the router.  The
# v134 intent -- managed-local handles everything -- is reproduced by forcing an
# early managed-local return at the top of the main routing path, right after
# the private-inference sub-check and before the managedLocalAvailable / gate /
# eligibility checks.  This subsumes the former separate action-route and
# subagent-route patches (whose 3.18.9 constructs no longer exist in 3.19.13).
MANAGED_LOCAL_ROUTE_ORIGINAL = (
    'reason:"private-inference-eligible"}})}(e,t);'
    'if(!e.managedLocalAvailable)return'
    '{runtime:"connect",reason:"managed-local-unavailable"};'
)
MANAGED_LOCAL_ROUTE_PATCHED = (
    'reason:"private-inference-eligible"}})}(e,t);'
    + SAND_MANAGED_LOCAL_ROUTE_MARKER
    + 'return{runtime:"managed-local",reason:"sand-client"};'
    'if(!e.managedLocalAvailable)return'
    '{runtime:"connect",reason:"managed-local-unavailable"};'
)
LOCAL_RUNTIME_LOAD_ORIGINAL = (
    "let t=!1;try{t=await r.cursor.checkFeatureGate(Ms)}"
)
LOCAL_RUNTIME_LOAD_PATCHED = (
    "let t=!0;"
    + SAND_LOCAL_RUNTIME_LOAD_MARKER
    + "try{t=!0}"
)
AGENT_HOST_MOVE_EXEC_ORIGINAL = (
    "h=await Promise.resolve(r.cursor.checkFeatureGate(Js)).catch(()=>!1)"
)
AGENT_HOST_MOVE_EXEC_PATCHED = (
    "h=!0" + SAND_AGENT_HOST_MOVE_EXEC_MARKER
)
AGENT_HOST_MOVE_EXEC_READY_ANCHOR = (
    "using @anysphere/agent-host-exec for session resources (move_exec "
)
MANAGED_SUBAGENT_ROUTE_ORIGINAL = (
    "hasUnsupportedRunOptions:void 0!==e.runOptions.customSystemPrompt||"
    "void 0!==e.runOptions.harness||"
    "!0===e.runOptions.excludeWorkspaceContext||"
    "void 0!==e.runOptions.subagentTypeName||"
    "void 0!==e.runOptions.parentAgentToolCallId||"
    "!0===e.runOptions.directMetaParentChildSubagent"
)
MANAGED_SUBAGENT_ROUTE_PATCHED = (
    "hasUnsupportedRunOptions:void 0!==e.runOptions.customSystemPrompt||"
    "void 0!==e.runOptions.harness||"
    "!0===e.runOptions.excludeWorkspaceContext"
    + SAND_MANAGED_SUBAGENT_ROUTE_MARKER
    + "||!0===e.runOptions.directMetaParentChildSubagent"
)
MANAGED_ACTION_ROUTE_ORIGINAL = (
    'return"userMessageAction"!==e.actionCase?"action-not-supported":'
    'e.requestedMode!==oe.xyI.AGENT?"mode-not-supported":'
    'e.simulatedUserMessage?"simulated-message-not-supported":'
    'void 0===e.modelId?"model-not-supported":'
    'e.hasModelCredentials?"private-model-not-supported":'
    'e.hasUnsupportedRunOptions?"run-options-not-supported":void 0'
)
MANAGED_ACTION_ROUTE_PATCHED = (
    "return"
    + SAND_MANAGED_ACTION_ROUTE_MARKER
    + '!["userMessageAction","summarizeAction","resumeAction",'
    '"backgroundTaskCompletionAction","executePlanAction"].includes(e.actionCase)?'
    '"action-not-supported":'
    '"userMessageAction"===e.actionCase&&'
    'e.simulatedUserMessage?"simulated-message-not-supported":'
    'void 0===e.modelId?"model-not-supported":'
    'e.hasModelCredentials?"private-model-not-supported":'
    'e.hasUnsupportedRunOptions?"run-options-not-supported":void 0'
)
# v1.2.6 V1 route: kept verbatim only for in-place migration and uninstall.
MANAGED_ACTION_ROUTE_PATCHED_V1 = (
    "return"
    + LEGACY_SAND_MANAGED_ACTION_ROUTE_MARKER
    + '!["userMessageAction","summarizeAction","resumeAction",'
    '"backgroundTaskCompletionAction"].includes(e.actionCase)?'
    '"action-not-supported":'
    '"userMessageAction"===e.actionCase&&'
    'e.requestedMode!==oe.xyI.AGENT?"mode-not-supported":'
    '"userMessageAction"===e.actionCase&&'
    'e.simulatedUserMessage?"simulated-message-not-supported":'
    'void 0===e.modelId?"model-not-supported":'
    'e.hasModelCredentials?"private-model-not-supported":'
    'e.hasUnsupportedRunOptions?"run-options-not-supported":void 0'
)
SUBAGENT_RESUME_MODE_ORIGINAL = (
    "e.resumeAgentId&&e.mode===Gn.FL.UNSPECIFIED&&!e.readonly?"
    "Ee.xy.UNSPECIFIED:"
)
SUBAGENT_RESUME_MODE_PATCHED = (
    "e.resumeAgentId&&e.mode===Gn.FL.UNSPECIFIED&&!e.readonly?"
    + SAND_SUBAGENT_RESUME_MODE_MARKER
    + "Ee.xy.AGENT:"
)
SUBAGENT_COMPLETION_WAKE_RE = re.compile(
    r'([A-Za-z_$][A-Za-z0-9_$]*)\.source==="interactive-child"\|\|'
    r'\1\.payload\.notificationContext==="user_driven_interactive_child"'
)
SUBAGENT_COMPLETION_WAKE_PATCH_RE = re.compile(
    r'([A-Za-z_$][A-Za-z0-9_$]*)\.source==="subagent"'
    + re.escape(SAND_SUBAGENT_COMPLETION_WAKE_MARKER)
    + r'\|\|\1\.source==="interactive-child"\|\|'
    r'\1\.payload\.notificationContext==="user_driven_interactive_child"'
)
MANAGED_SUBAGENT_SESSION_ORIGINAL = (
    "const Cre={enableEmptyResponseRetry:!0,enableGrepBroadGlobGuard:!0,"
    "enableReadToolNegativeOffset:!0,enableSandboxSharedBuildCache:!0,"
    "nalLoopDetection:!0};"
)
MANAGED_SUBAGENT_SESSION_PATCHED = (
    "const Cre={enableEmptyResponseRetry:!0,enableGrepBroadGlobGuard:!0,"
    "enableReadToolNegativeOffset:!0,enableSandboxSharedBuildCache:!0,"
    "nalLoopDetection:!0,useClientSideSubagent:!0"
    + SAND_MANAGED_SUBAGENT_SESSION_MARKER
    + "};"
)
MANAGED_TASK_TOOL_ORIGINAL = (
    "isGenerateImageModelRestricted:!1,taskToolProps:void 0},resolvers:"
)
MANAGED_TASK_TOOL_READY_ANCHOR = (
    "function toe(e){return void 0===e.taskToolProps?rO.fromTools([])"
)
MANAGED_TASK_RUN_READY_ANCHOR = "Creating subagent and starting execution"
AGENT_HOST_IDENTITY_ORIGINAL = 'clientIdentity:{clientType:"ide"}'
AGENT_HOST_IDENTITY_PATCHED = (
    'clientIdentity:{clientType:"sand"'
    + SAND_AGENT_HOST_IDENTITY_MARKER
    + "}"
)
DIRECT_STREAM_ANCHOR = (
    "function me(e){return t=>{return n=this,r=void 0,s=function*(){"
)
# Marker-bounded fallback stripper for older direct-stream injections when the
# exact current string no longer matches.  It stops at the injected attempt's
# no-op finish callback and leaves the original code after hre() untouched.
DIRECT_STREAM_INJECTION_RE = re.compile(
    r"\{/\*SAND_DIRECT_INFERENCE_STREAM_V1\*/[\s\S]*?"
    r"finish:\(\)=>Promise\.resolve\(\)\}+"
)
# Cursor 3.18.9's transport writes the stored desktop access token in this
# method.  Grok Bot instead asks an injected runtime provider for a
# dedicated token when (and only when) the request is InferenceService.Stream.
# Keep the full original prefix byte-exact so unsupported bundles fail the
# install-time anchor check instead of receiving a partial transport rewrite.
GROK_RUNTIME_AUTH_ORIGINAL = (
    "applyAuthorization(e,t){return a(this,void 0,void 0,function*(){"
    "var n,r,o,i,s,a,u,l,m,c,d,p;if(t.overrideAuthToken){"
)
# Cursor 3.19.13 ships two applyAuthorization transports (cursor-agent-host and
# cursor-always-local) whose only difference is the minified var-declaration
# order.  The injected relay block is guarded by InferenceService.Stream, so
# patching both sites is safe (non-Stream requests fall through unchanged) and
# guarantees whichever transport carries the Stream request is covered.
GROK_RUNTIME_AUTH_METHOD_PREFIX = (
    "applyAuthorization(e,t){return a(this,void 0,void 0,function*(){"
)
GROK_RUNTIME_AUTH_SUFFIX = "if(t.overrideAuthToken){"
GROK_RUNTIME_AUTH_VAR_DECLS: Tuple[str, ...] = (
    "var n,r,o,s,i,a,l,c,u,d,m,p;",  # extensions/cursor-agent-host/dist/main.js
    "var n,r,s,o,i,a,l,u,m,c,d,p;",  # extensions/cursor-always-local/dist/main.js
)
EXPECTED_GROK_RUNTIME_AUTH_SITES = len(GROK_RUNTIME_AUTH_VAR_DECLS)
GROK_RUNTIME_AUTH_ORIGINALS_V3113: Tuple[str, ...] = tuple(
    GROK_RUNTIME_AUTH_METHOD_PREFIX + _vars + GROK_RUNTIME_AUTH_SUFFIX
    for _vars in GROK_RUNTIME_AUTH_VAR_DECLS
)
GROK_RUNTIME_AUTH_PATCHED_V132 = (
    "applyAuthorization(e,t){return a(this,void 0,void 0,function*(){"
    "var n,r,o,i,s,a,u,l,m,c,d,p;"
    + LEGACY_SAND_GROK_RUNTIME_AUTH_MARKER
    + 'const __sandGrokStream=e?.service?.typeName==="aiserver.v1.InferenceService"'
    '&&e?.method?.name==="Stream";'
    "if(__sandGrokStream){"
    'e.header.set("x-cursor-client-type",String("sand")),'
    'e.header.set("x-cursor-client-source","sand-desktop"),'
    'e.header.set("x-cursor-client-version","0.44.0"),'
    'e.header.set("x-sand-box-namespace","prod");'
    "const __sandGrokOwners=[t,this.host,this.host?.runtime,this.host?.platform,"
    "globalThis.__SAND_GROK_RUNTIME__,globalThis],"
    "__sandGrokOwner=__sandGrokOwners.find(e=>"
    '"function"==typeof e?.getGrokBotAccessToken);'
    "if(!__sandGrokOwner)throw new Error("
    '"[SAND_GROK_RUNTIME_TOKEN_UNAVAILABLE] Grok Bot 0.44 runtime "'
    '+"getGrokBotAccessToken provider was not found");'
    "const __sandGrokToken=yield __sandGrokOwner.getGrokBotAccessToken("
    "{backendUrl:t.baseUrl});"
    'if("string"!=typeof __sandGrokToken||!__sandGrokToken.trim())'
    "throw new Error("
    '"[SAND_GROK_RUNTIME_TOKEN_EMPTY] Grok Bot runtime returned no Stream token");'
    'e.header.set("Authorization",`Bearer ${__sandGrokToken.trim()}`),'
    "this.addTeamIdHeader(e);return}"
    "if(t.overrideAuthToken){"
)
# v1.4.0/v1.4.1 relay block (static token, no runtime refresh).  Kept verbatim
# so already-installed builds migrate in place and uninstall byte-exactly.
GROK_RUNTIME_AUTH_RELAY_BLOCK_V141 = (
    SAND_GROK_RUNTIME_AUTH_MARKER
    + 'const __sandGrokStream=e?.service?.typeName==="aiserver.v1.InferenceService"'
    '&&e?.method?.name==="Stream";'
    "if(__sandGrokStream){"
    'const __sandRelayFs=require("node:fs"),'
    '__sandRelayPath=require("node:path"),__sandRelayOs=require("node:os"),'
    '__sandRelayConfigPath=process.env.SAND_GROK_BOX_RELAY_CONFIG||('
    'process.platform==="win32"?'
    '__sandRelayPath.join(process.env.LOCALAPPDATA||process.env.APPDATA||'
    '__sandRelayPath.join(__sandRelayOs.homedir(),"AppData","Local"),'
    '"SandClientModeStream","sand-client-cli","grok-box-relay.json"):'
    '__sandRelayPath.join(__sandRelayOs.homedir(),'
    'process.platform==="darwin"?"Library/Application Support":".config",'
    '"SandClientModeStream","sand-client-cli","grok-box-relay.json")),'
    '__sandRelayConfig=JSON.parse(__sandRelayFs.readFileSync('
    '__sandRelayConfigPath,"utf8"));'
    'if(!__sandRelayConfig?.baseUrl||!__sandRelayConfig?.token)throw new Error('
    '"[SAND_GROK_BOX_RELAY_CONFIG_INVALID] Grok Bot gateway descriptor is missing");'
    'e.url=new URL(__sandRelayConfig.relayPath||'
    '"/sand-stream-relay/aiserver.v1.InferenceService/Stream",'
    '__sandRelayConfig.baseUrl).toString();'
    'e.header.set("Authorization",`Bearer ${__sandRelayConfig.token}`);'
    'for(const[__sandHeader,__sandValue]of Object.entries('
    '__sandRelayConfig.headers||{}))"string"==typeof __sandValue&&'
    '__sandValue.length&&e.header.set(__sandHeader,__sandValue);'
    'e.header.set("x-cursor-client-type",String("sand")),'
    'e.header.set("x-cursor-client-source","sand-desktop"),'
    'e.header.set("x-cursor-client-version","0.44.0"),'
    'e.header.set("x-sand-box-namespace","prod");return}'
)
GROK_RUNTIME_AUTH_PATCHES_V141: Tuple[str, ...] = tuple(
    GROK_RUNTIME_AUTH_METHOD_PREFIX
    + _vars
    + GROK_RUNTIME_AUTH_RELAY_BLOCK_V141
    + GROK_RUNTIME_AUTH_SUFFIX
    for _vars in GROK_RUNTIME_AUTH_VAR_DECLS
)
# Shared, class-agnostic relay block inserted between the var declarations and
# the original `if(t.overrideAuthToken){`.  It only touches e/t and node built-
# ins, so it is identical across both 3.19.13 transport sites.  v1.4.2 adds a
# self-refresh path: when the stored gateway token is near expiry, it re-mints a
# fresh one via EnsureSandBox using the Cursor account token stored in the relay
# config's `refresh` block — no Grok Bot client, no background daemon.  The whole
# refresh is best-effort inside try/catch: on any failure it keeps using the
# stored token, so it never breaks worse than the static path.
GROK_RUNTIME_AUTH_RELAY_BLOCK_V144 = (
    SAND_GROK_RUNTIME_AUTH_MARKER
    + 'const __sandGrokStream=e?.service?.typeName==="aiserver.v1.InferenceService"'
    '&&e?.method?.name==="Stream";'
    "if(__sandGrokStream){"
    'const __sandFs=require("node:fs"),__sandPath=require("node:path"),'
    '__sandOs=require("node:os"),__sandHttps=require("node:https");'
    'const __sandRelayConfigPath=process.env.SAND_GROK_BOX_RELAY_CONFIG||('
    'process.platform==="win32"?'
    '__sandPath.join(process.env.LOCALAPPDATA||process.env.APPDATA||'
    '__sandPath.join(__sandOs.homedir(),"AppData","Local"),'
    '"SandClientModeStream","sand-client-cli","grok-box-relay.json"):'
    '__sandPath.join(__sandOs.homedir(),'
    'process.platform==="darwin"?"Library/Application Support":".config",'
    '"SandClientModeStream","sand-client-cli","grok-box-relay.json"));'
    'let __sandCfg=JSON.parse(__sandFs.readFileSync(__sandRelayConfigPath,"utf8"));'
    'const __sandExp=t=>{try{const p=String(t).split(".");if(3!==p.length)return 0;'
    'const j=JSON.parse(Buffer.from(p[1],"base64url").toString("utf8"));'
    'return"number"==typeof j.exp?1e3*j.exp:0}catch(_){return 0}};'
    'const __sandNeed=c=>{if(!c||!c.refresh||!c.refresh.accessToken||!c.refresh.backendUrl)return!1;'
    'if(!c.token)return!0;const x=__sandExp(c.token);'
    'if(x)return x-Date.now()<12e4;'
    'return Date.now()-(c.mintedAtMs||0)>(c.refreshAfterMs||36e5)};'
    'const __sandSum=m=>{const ep=Math.floor(Date.now()/1e6),'
    'b=new Uint8Array([ep>>40&255,ep>>32&255,ep>>24&255,ep>>16&255,ep>>8&255,255&ep]);'
    'let pv=165;for(let i=0;i<b.length;i++)b[i]=(b[i]^pv)+i%256&255,pv=b[i];'
    'return Buffer.from(b).toString("base64url")+String(m||"")};'
    'const __sandRV=(buf,o)=>{let r=0n,s=0n;for(;;){const y=buf[o++];'
    'r|=BigInt(127&y)<<s;if(!(128&y))return[r,o];s+=7n}};'
    'const __sandBox=buf=>{let o=0,url="",tok="",net="";while(o<buf.length){'
    'let k;[k,o]=__sandRV(buf,o);const f=Number(k>>3n),w=Number(7n&k);'
    'if(0===w){let v;[v,o]=__sandRV(buf,o)}else if(2===w){let l;[l,o]=__sandRV(buf,o);'
    'const n=Number(l),g=buf.subarray(o,o+n);o+=n;'
    '10===f?url=g.toString("utf8"):11===f?tok=g.toString("utf8"):4===f&&(net=g.toString("utf8"))}'
    'else if(1===w)o+=8;else{if(5!==w)break;o+=4}}return{url,tok,net}};'
    'const __sandMint=r=>new Promise((res,rej)=>{'
    'const u=new URL("/aiserver.v1.GrokBotService/EnsureSandBox",r.backendUrl),'
    'body=Buffer.from([16,1]),'
    'rq=__sandHttps.request(u,{method:"POST",headers:{'
    'authorization:"Bearer "+r.accessToken,"connect-protocol-version":"1",'
    '"content-type":"application/proto","x-cursor-checksum":__sandSum(r.machineId),'
    '["x-cursor-client-type"]:"sand","x-cursor-client-version":"0.44.0",'
    '"x-sand-box-namespace":"prod","x-ghost-mode":"true","content-length":body.length}},'
    'rp=>{const ch=[];rp.on("data",d=>ch.push(d));rp.on("end",()=>{'
    '200!==rp.statusCode?rej(new Error("EnsureSandBox HTTP "+rp.statusCode)):'
    '(()=>{try{res(__sandBox(Buffer.concat(ch)))}catch(err){rej(err)}})()})});'
    'rq.on("error",rej);rq.write(body);rq.end()});'
    'if(__sandNeed(__sandCfg))try{const nb=yield __sandMint(__sandCfg.refresh);'
    'if(nb.url&&nb.tok){__sandCfg.baseUrl=nb.url,__sandCfg.token=nb.tok,'
    '__sandCfg.headers=__sandCfg.headers||{},nb.net&&(__sandCfg.headers["x-anyrun-network-token"]=nb.net),'
    '__sandCfg.mintedAtMs=Date.now();'
    'try{__sandFs.writeFileSync(__sandRelayConfigPath,JSON.stringify(__sandCfg),{mode:384})}catch(_){}}}catch(_){}'
    'if(!__sandCfg?.baseUrl||!__sandCfg?.token)throw new Error('
    '"[SAND_GROK_BOX_RELAY_CONFIG_INVALID] Grok Bot gateway descriptor is missing");'
    'e.url=new URL(__sandCfg.relayPath||'
    '"/sand-stream-relay/aiserver.v1.InferenceService/Stream",'
    '__sandCfg.baseUrl).toString();'
    'e.header.set("Authorization",`Bearer ${__sandCfg.token}`);'
    'for(const[__sandHeader,__sandValue]of Object.entries('
    '__sandCfg.headers||{}))"string"==typeof __sandValue&&'
    '__sandValue.length&&e.header.set(__sandHeader,__sandValue);'
    'e.header.set("x-cursor-client-type",String("sand")),'
    'e.header.set("x-cursor-client-source","sand-desktop"),'
    'e.header.set("x-cursor-client-version","0.44.0"),'
    'e.header.set("x-sand-box-namespace","prod");return}'
)
GROK_RUNTIME_AUTH_PATCHES_V144: Tuple[str, ...] = tuple(
    GROK_RUNTIME_AUTH_METHOD_PREFIX
    + _vars
    + GROK_RUNTIME_AUTH_RELAY_BLOCK_V144
    + GROK_RUNTIME_AUTH_SUFFIX
    for _vars in GROK_RUNTIME_AUTH_VAR_DECLS
)
# v1.4.5 aligns with the headers observed on the Box's working 0.46.0 Stream
# path. Keep every previous generation byte-exact so an installed older build
# can migrate or uninstall without reconstructing minified JavaScript.
GROK_RUNTIME_AUTH_RELAY_BLOCK_V145 = GROK_RUNTIME_AUTH_RELAY_BLOCK_V144.replace(
    'e.header.set("x-cursor-client-source","sand-desktop"),',
    'e.header.delete("x-cursor-client-source"),',
).replace("0.44.0", "0.46.0")
GROK_RUNTIME_AUTH_PATCHES_V145: Tuple[str, ...] = tuple(
    GROK_RUNTIME_AUTH_METHOD_PREFIX
    + _vars
    + GROK_RUNTIME_AUTH_RELAY_BLOCK_V145
    + GROK_RUNTIME_AUTH_SUFFIX
    for _vars in GROK_RUNTIME_AUTH_VAR_DECLS
)
# v1.4.7 removed the request's Connect compression negotiation. Keep its exact
# bytes so existing installs can be migrated and uninstalled byte-for-byte.
GROK_RUNTIME_AUTH_RELAY_BLOCK_V147 = GROK_RUNTIME_AUTH_RELAY_BLOCK_V145.replace(
    'e.header.delete("x-cursor-client-source"),',
    'e.header.delete("x-cursor-client-source"),'
    'e.header.delete("connect-accept-encoding"),'
    'e.header.delete("accept-encoding"),',
)
GROK_RUNTIME_AUTH_PATCHES_V147: Tuple[str, ...] = tuple(
    GROK_RUNTIME_AUTH_METHOD_PREFIX
    + _vars
    + GROK_RUNTIME_AUTH_RELAY_BLOCK_V147
    + GROK_RUNTIME_AUTH_SUFFIX
    for _vars in GROK_RUNTIME_AUTH_VAR_DECLS
)
# Cursor 3.19.x already advertises gzip and ships the matching decompressor.
# The relay must declare gzip on its response instead of hiding negotiation.
GROK_RUNTIME_AUTH_RELAY_BLOCK = GROK_RUNTIME_AUTH_RELAY_BLOCK_V145
GROK_RUNTIME_AUTH_PATCHES_V3113: Tuple[str, ...] = tuple(
    GROK_RUNTIME_AUTH_METHOD_PREFIX
    + _vars
    + GROK_RUNTIME_AUTH_RELAY_BLOCK
    + GROK_RUNTIME_AUTH_SUFFIX
    for _vars in GROK_RUNTIME_AUTH_VAR_DECLS
)
GROK_RUNTIME_AUTH_PATCHED = (
    GROK_RUNTIME_AUTH_METHOD_PREFIX
    + "var n,r,o,i,s,a,u,l,m,c,d,p;"
    + GROK_RUNTIME_AUTH_RELAY_BLOCK
    + GROK_RUNTIME_AUTH_SUFFIX
)
# Retired response-selector fallbacks are kept byte-exact for migration and
# uninstall. Cursor's custom HTTP/2 transport bypasses these three selectors.
CONNECT_GZIP_FALLBACK_ORIGINAL = (
    'let c;const u=n.get(a.LB);if(null!==u&&"identity"!==u.toLowerCase()'
    '&&(c=e.find(e=>e.name===u),!c))throw new i.T(`unsupported response encoding '
    '"${u}"`,s.C.Internal,n);return{foundStatus:r,compression:c,headerError:o}'
)
# Pre-release v1.5.0 only handled a completely absent response header. Keep the
# exact bytes so interrupted/local test installs migrate without manual repair.
CONNECT_GZIP_FALLBACK_PATCHED_V150A = (
    'let c;const u=n.get(a.LB);null===u&&(c=e.find(e=>"gzip"===e.name))'
    + SAND_CONNECT_GZIP_FALLBACK_MARKER
    + ';if(null!==u&&"identity"!==u.toLowerCase()'
    '&&(c=e.find(e=>e.name===u),!c))throw new i.T(`unsupported response encoding '
    '"${u}"`,s.C.Internal,n);return{foundStatus:r,compression:c,headerError:o}'
)
CONNECT_GZIP_FALLBACK_PATCHED = (
    'let c=e.find(e=>"gzip"===e.name)'
    + SAND_CONNECT_GZIP_FALLBACK_MARKER
    + ';const u=n.get(a.LB);if(null!==u&&"identity"!==u.toLowerCase()'
    '&&(c=e.find(e=>e.name===u),!c))throw new i.T(`unsupported response encoding '
    '"${u}"`,s.C.Internal,n);return{foundStatus:r,compression:c,headerError:o}'
)
CONNECT_GZIP_METHOD_FALLBACK_ORIGINAL = (
    'function d(e,t,n,s,a){let d;const m=a.get('
    'e==r.MethodKind.Unary?o.kq:o.jL);if(null!=m&&"identity"!==m.toLowerCase()'
    '&&(d=t.find(e=>e.name===m),!d))throw new u.T(`unsupported response encoding '
    '"${m}"`,l.C.Internal,a);return Object.assign({compression:d},'
)
CONNECT_GZIP_METHOD_FALLBACK_PATCHED = (
    'function d(e,t,n,s,a){let d=e!==r.MethodKind.Unary?'
    't.find(e=>"gzip"===e.name):void 0'
    + SAND_CONNECT_GZIP_FALLBACK_MARKER
    + ';const m=a.get(e==r.MethodKind.Unary?o.kq:o.jL);'
    'if(null!=m&&"identity"!==m.toLowerCase()'
    '&&(d=t.find(e=>e.name===m),!d))throw new u.T(`unsupported response encoding '
    '"${m}"`,l.C.Internal,a);return Object.assign({compression:d},'
)
# The custom HTTP/2 transport can deliver a gzip-flagged Connect envelope
# without the decompressor selected by connect-es. Fall back at the unique
# parser error site, only when the compression bit is set and no codec was
# supplied. maxOutputLength preserves Cursor's existing readMaxBytes bound.
CONNECT_GZIP_ENVELOPE_ORIGINAL = (
    'async function d(e,t,n){let{flags:i,data:a}=e;if((i&s.y)===s.y){'
    'if(!t)throw new r.T("received compressed envelope, but do not know how to '
    'decompress",o.C.Internal);a=await t.decompress(a,n),i^=s.y}'
    'return{data:a,flags:i}}'
)
CONNECT_GZIP_ENVELOPE_PATCHED = (
    'async function d(e,t,n){let{flags:i,data:a}=e;if((i&s.y)===s.y){if(!t){'
    'a=new Uint8Array(require("node:zlib").gunzipSync(a,{maxOutputLength:n}));'
    + SAND_CONNECT_GZIP_FALLBACK_MARKER
    + 'i^=s.y;return{data:a,flags:i}}a=await t.decompress(a,n),i^=s.y}'
    'return{data:a,flags:i}}'
)
# V133A: macOS-only path variant (path.join(homedir, LOCALAPPDATA) bug on Windows).
# Kept for in-place migration and uninstall of existing macOS installations.
GROK_RUNTIME_AUTH_PATCHED_V133A = (
    "applyAuthorization(e,t){return a(this,void 0,void 0,function*(){"
    "var n,r,o,i,s,a,u,l,m,c,d,p;"
    + SAND_GROK_RUNTIME_AUTH_MARKER
    + 'const __sandGrokStream=e?.service?.typeName==="aiserver.v1.InferenceService"'
    '&&e?.method?.name==="Stream";'
    "if(__sandGrokStream){"
    'const __sandRelayFs=require("node:fs"),'
    '__sandRelayPath=require("node:path"),__sandRelayOs=require("node:os"),'
    '__sandRelayConfigPath=process.env.SAND_GROK_BOX_RELAY_CONFIG||'
    '__sandRelayPath.join(__sandRelayOs.homedir(),'
    'process.platform==="darwin"?"Library/Application Support":'
    'process.platform==="win32"?(process.env.LOCALAPPDATA||process.env.APPDATA||'
    '"AppData/Local"):".config","SandClientModeStream","sand-client-cli",'
    '"grok-box-relay.json"),'
    '__sandRelayConfig=JSON.parse(__sandRelayFs.readFileSync('
    '__sandRelayConfigPath,"utf8"));'
    'if(!__sandRelayConfig?.baseUrl||!__sandRelayConfig?.token)throw new Error('
    '"[SAND_GROK_BOX_RELAY_CONFIG_INVALID] Grok Bot gateway descriptor is missing");'
    'e.url=new URL(__sandRelayConfig.relayPath||'
    '"/sand-stream-relay/aiserver.v1.InferenceService/Stream",'
    '__sandRelayConfig.baseUrl).toString();'
    'e.header.set("Authorization",`Bearer ${__sandRelayConfig.token}`);'
    'for(const[__sandHeader,__sandValue]of Object.entries('
    '__sandRelayConfig.headers||{}))"string"==typeof __sandValue&&'
    '__sandValue.length&&e.header.set(__sandHeader,__sandValue);'
    'e.header.set("x-cursor-client-type",String("sand")),'
    'e.header.set("x-cursor-client-source","sand-desktop"),'
    'e.header.set("x-cursor-client-version","0.44.0"),'
    'e.header.set("x-sand-box-namespace","prod");return}'
    "if(t.overrideAuthToken){"
)
MAX_TOKENS_ORIGINAL = (
    "t.resolveExtendedUsage({inputTokens:n.inputTokens,"
    "outputTokens:n.outputTokens,cacheReadTokens:n.cacheReadTokens,"
    "cacheWriteTokens:n.cacheWriteTokens,maxTokens:n.maxTokens})"
)
MAX_TOKENS_PATCHED = (
    "t.resolveExtendedUsage({inputTokens:n.inputTokens,"
    "outputTokens:n.outputTokens,cacheReadTokens:n.cacheReadTokens,"
    "cacheWriteTokens:n.cacheWriteTokens,maxTokens:(()=>{"
    'const c=this.requestedModel?.parameters?.find(p=>p.id==="context")?.value;'
    "if(void 0===c)return n.maxTokens;"
    "const s=String(c).trim().toLowerCase();const num=parseFloat(s);"
    "if(!Number.isFinite(num)||num<=0)return n.maxTokens;"
    'const mult=s.endsWith("k")?1e3:s.endsWith("m")?1e6:s.endsWith("b")?1e9:1;'
    "return num*mult})()})" + SAND_MAX_TOKENS_MARKER
)
AGENT_HOST_ENABLEMENT_RE = re.compile(
    r"(this\._agentHostEnabled=)([A-Za-z_$][A-Za-z0-9_$]*)(,)"
)
AGENT_HOST_ENABLEMENT_PATCH_RE = re.compile(
    rf"([A-Za-z_$][A-Za-z0-9_$]*)=!0;"
    rf"{re.escape(SAND_AGENT_HOST_ENABLEMENT_MARKER)}"
    rf"(this\._agentHostEnabled=)\1(,)"
)


def _managed_task_tool_props(
    custom_subagent_normalizer: str = "()=>[]",
    marker: str = SAND_MANAGED_TASK_TOOL_MARKER,
    model_catalog: str = (
        "new Map([[e.requestedModel.modelId,{slug:e.requestedModel.modelId}],"
        "[i,{slug:e.requestedModel.modelId}]])"
    ),
    parent_model_name: str = "e.requestedModel.modelId",
    is_model_valid: str = "t=>t===e.requestedModel.modelId||t===i",
) -> str:
    # V3 (defaults): in createAgentConfig `i` is
    # e.resolvedModel?.modelId ?? e.modelId.  On the session stream that is the
    # backend-resolved variant (e.g. "<model>-thinking-max"), so the parent id
    # must come from the client request e.requestedModel.modelId; the variant
    # is accepted only as an alias mapped back to it.  The isModelValid arrow
    # parameter is `t`, never `e`, so the outer `e` is not shadowed.
    # Subagents stay locked to the parent model (client id + parameters).
    return (
        "{"
        + marker
        + f"parentRequestedModelName:{parent_model_name},"
        "parentModelParameters:e.requestedModel.parameters,"
        "parentMaxMode:l,"
        "isModelBlocked:()=>!1,"
        f"isModelValid:{is_model_valid},"
        "requiresMaxMode:()=>!1,"
        "compareModelCosts:()=>0,"
        'subagentModelForcePolicy:"none",'
        "requireServerSideSubagent:!1,"
        f"subagentModels:{{modelsBySlug:{model_catalog}}},"
        f"normalizeCustomSubagents:{custom_subagent_normalizer},"
        "getTaskToolConfig:async()=>({})"
        "}"
    )


def _managed_task_tool_patched() -> str:
    return (
        "isGenerateImageModelRestricted:!1,taskToolProps:"
        "void 0!==e.runOptions.subagentTypeName?void 0:"
        + _managed_task_tool_props()
        + "},resolvers:"
    )


def _managed_task_tool_patched_v126() -> str:
    # v1.2.6 / v1.2.7.1 (V2) exact string, kept only for migration and uninstall.
    return (
        "isGenerateImageModelRestricted:!1,taskToolProps:"
        "void 0!==e.runOptions.subagentTypeName?void 0:"
        + _managed_task_tool_props(
            marker=LEGACY_SAND_MANAGED_TASK_TOOL_MARKER_V2,
            model_catalog="new Map([[i,{slug:i}]])",
            parent_model_name="i",
            is_model_valid="e=>e===i",
        )
        + "},resolvers:"
    )


def _managed_task_tool_patched_v124() -> str:
    return (
        "isGenerateImageModelRestricted:!1,taskToolProps:"
        + _managed_task_tool_props(
            "e=>e",
            LEGACY_SAND_MANAGED_TASK_TOOL_MARKER,
            "new Map",
            parent_model_name="i",
            is_model_valid="e=>e===i",
        )
        + "},resolvers:"
    )


def _managed_task_tool_patched_v125() -> str:
    return (
        "isGenerateImageModelRestricted:!1,taskToolProps:"
        "void 0!==e.runOptions.subagentTypeName?void 0:"
        + _managed_task_tool_props(
            marker=LEGACY_SAND_MANAGED_TASK_TOOL_MARKER,
            model_catalog="new Map",
            parent_model_name="i",
            is_model_valid="e=>e===i",
        )
        + "},resolvers:"
    )


def _direct_stream_injection() -> str:
    # Cursor 3.19.13 port of the v134 direct-stream injection.  The native me()
    # attempt factory uses e.runInference(...) (RunInference), which the Sand
    # backend rejects, so this early-returns a Stream-based attempt instead.
    #   Joe -> J   (Stream session provider: new J(client, requestedModel,
    #               modelConfig, inferenceReason).getSession() -> class H,
    #               whose executor class W issues this.client.stream(...))
    #   RK  -> o.Ycw (executor wrapper; o is a module import already in scope)
    # Locals are __sand-prefixed so they cannot shadow the module bindings
    # (o, c, p, ...) that me() already relies on.  resolvedModel falls back to
    # the requestedModel and resolvedModelMetadata is left undefined; the
    # downstream attempt normaliser defaults both, so prompt selection degrades
    # gracefully without the (reworked, schema-changed) 3.19.13 metadata builder.
    return (
        "{"
        + SAND_DIRECT_STREAM_MARKER
        + "const __sandModel=t.requestedModel;"
        'if(void 0===__sandModel)throw new Error("Sand direct Stream requires requestedModel");'
        'const __sandModelId=String(__sandModel.modelId||""),'
        "__sandLower=__sandModelId.toLowerCase(),"
        "__sandHas=e=>__sandLower.includes(e),"
        '__sandVendor=__sandHas("grok")?"xai":__sandHas("gemini")?"gemini":'
        '(__sandHas("claude")||__sandHas("opus")||__sandHas("sonnet")||'
        '__sandHas("haiku")||__sandHas("fable"))?"anthropic":'
        '(__sandHas("gpt")||__sandHas("codex"))?"openai":"unknown",'
        "__sandSession=new J(e,__sandModel,void 0,void 0).getSession(),"
        "__sandToolSession={getExecutor:e=>new o.Ycw(__sandSession.getExecutor(e))},"
        "__sandMeta={promptModelInfo:{vendor:__sandVendor,modelName:__sandModelId,"
        'promptVersion:"latest",'
        'isSonnet45:__sandHas("sonnet-4.5")||__sandHas("sonnet45"),'
        'isGemini3:__sandHas("gemini-3")||__sandHas("gemini3"),'
        'isGpt51:__sandHas("gpt-5.1")||__sandHas("gpt5.1"),'
        'isGpt52:__sandHas("gpt-5.2")||__sandHas("gpt5.2"),'
        'isGpt5:__sandHas("gpt-5"),'
        'isGpt55:__sandHas("gpt-5.5")||__sandHas("gpt5.5"),'
        'isGpt56:__sandHas("gpt-5.6")||__sandHas("gpt5.6"),'
        'isSonnet4:__sandHas("sonnet-4")||__sandHas("sonnet4"),'
        'isCodexFamily:__sandHas("codex"),'
        'isGpt54:__sandHas("gpt-5.4")||__sandHas("gpt5.4"),'
        'isGpt52Codex:__sandHas("gpt-5.2-codex"),'
        'isGpt53Codex:__sandHas("gpt-5.3-codex"),'
        'isGpt53CodexSpark:__sandHas("gpt-5.3-codex-spark"),'
        'isClaude4X:__sandHas("claude")||__sandHas("opus")||__sandHas("sonnet"),'
        'isOpus45:__sandHas("opus-4.5")||__sandHas("opus45"),'
        'isOpus46:__sandHas("opus-4.6")||__sandHas("opus46"),'
        'isOpus48:__sandHas("opus-4.8")||__sandHas("opus48"),'
        'isOpus5:__sandHas("opus-5")||__sandHas("opus5"),'
        'isFable5:__sandHas("fable-5")||__sandHas("fable5"),'
        'isFruitcake:__sandHas("fruitcake"),'
        'isGpt5Family:__sandHas("gpt-5"),'
        'isComposer1:__sandHas("composer-1")||__sandHas("composer1"),'
        'isComposer15:__sandHas("composer-1.5")||__sandHas("composer15"),'
        'isComposer2:__sandHas("composer-2")||__sandHas("composer2"),'
        'isComposerMatterhorn:__sandHas("matterhorn"),'
        'isGrok45ProductPrompt:__sandHas("grok")&&!__sandHas("grok-4.6")&&!__sandHas("grok46"),'
        'isGrok46ProductPrompt:__sandHas("grok-4.6")||__sandHas("grok46"),'
        "isRawTrainingSlug:!1},useDsv3Harness:!1,agentTokenLimit:void 0,"
        "estimatedCacheTtlMs:void 0,persona:void 0,featureFlags:void 0,"
        "promptConfig:void 0};"
        "return{promptSession:__sandSession,promptToolSession:__sandToolSession,"
        "attempt:{resolvedModel:__sandModel,supportsSelfSummary:!1,"
        "routedModelDisplayName:__sandModelId,resolvedModelMetadata:__sandMeta,"
        "finish:()=>Promise.resolve()}}}"
    )

def _platform_name() -> str:
    if sys.platform == "win32":
        return "windows"
    if sys.platform == "darwin":
        return "macos"
    raise SandToolError("当前仅支持 Windows 和 macOS")


def _enable_windows_ansi() -> bool:
    if sys.platform != "win32":
        return True
    try:
        kernel32 = ctypes.windll.kernel32
        for handle_id in (-11, -12):
            handle = kernel32.GetStdHandle(handle_id)
            if handle in (0, -1):
                continue
            mode = ctypes.c_uint32()
            if not kernel32.GetConsoleMode(handle, ctypes.byref(mode)):
                continue
            kernel32.SetConsoleMode(handle, mode.value | 0x0004)
        return True
    except Exception:
        return False


def _configure_console() -> None:
    global _COLOR_ENABLED
    for stream in (sys.stdout, sys.stderr):
        reconfigure = getattr(stream, "reconfigure", None)
        if callable(reconfigure):
            try:
                reconfigure(encoding="utf-8", errors="replace")
            except Exception:
                pass
    if os.environ.get("NO_COLOR"):
        _COLOR_ENABLED = False
        return
    _COLOR_ENABLED = _enable_windows_ansi() and sys.stdout.isatty()


def colorize(text: str, *codes: str) -> str:
    if not _COLOR_ENABLED or not codes:
        return text
    return "".join(codes) + text + ANSI_RESET


def print_warn(text: str) -> None:
    print(colorize(text, ANSI_YELLOW))


def print_error(text: str) -> None:
    print(colorize(text, ANSI_RED), file=sys.stderr)


def print_success(text: str) -> None:
    print(colorize(text, ANSI_GREEN, ANSI_BOLD))


class LoadingSpinner:
    def __init__(self, message: str = "处理中") -> None:
        self.message = message
        self._stop = threading.Event()
        self._thread: Optional[threading.Thread] = None

    def __enter__(self) -> "LoadingSpinner":
        if sys.stdout.isatty():
            self._thread = threading.Thread(target=self._run, daemon=True)
            self._thread.start()
        else:
            print(colorize(self.message + "...", ANSI_BLUE), flush=True)
        return self

    def __exit__(self, *_exc: object) -> None:
        self._stop.set()
        if self._thread is not None:
            self._thread.join()
            print("\r" + " " * 48 + "\r", end="", flush=True)

    def _run(self) -> None:
        frames = ("|", "/", "-", "\\")
        index = 0
        while not self._stop.wait(0.1):
            text = f"{frames[index % 4]} {self.message}"
            print("\r" + colorize(text, ANSI_BLUE), end="", flush=True)
            index += 1


def _config_dir() -> Path:
    if sys.platform == "win32":
        base = os.environ.get("LOCALAPPDATA") or os.environ.get("APPDATA")
        if base:
            return Path(base) / "SandClientModeStream" / "sand-client-cli"
        return (
            Path.home()
            / "AppData"
            / "Local"
            / "SandClientModeStream"
            / "sand-client-cli"
        )
    if sys.platform == "darwin":
        return (
            Path.home()
            / "Library"
            / "Application Support"
            / "SandClientModeStream"
            / "sand-client-cli"
        )
    return (
        Path.home()
        / ".config"
        / "SandClientModeStream"
        / "sand-client-cli"
    )


def _config_path() -> Path:
    return _config_dir() / "config.json"


def _relay_config_path() -> Path:
    return _config_dir() / "grok-box-relay.json"


GROK_GATEWAY_DESCRIPTOR_READER = r'''
const crypto=require("node:crypto"),fs=require("node:fs"),os=require("node:os"),path=require("node:path"),cp=require("node:child_process");
function password(){for(const service of ["Grok Bot Safe Storage","Grok Bot"]){try{const value=cp.execFileSync("/usr/bin/security",["find-generic-password","-s",service,"-w"],{encoding:"utf8",stdio:["ignore","pipe","ignore"]}).trim();if(value)return value}catch{}}throw new Error("safe-storage-unavailable")}
function decrypt(value,keyText){const encrypted=Buffer.from(value,"base64");if(encrypted.subarray(0,3).toString("ascii")!=="v10")throw new Error("unsupported-envelope");const key=crypto.pbkdf2Sync(keyText,"saltysalt",1003,16,"sha1"),decipher=crypto.createDecipheriv("aes-128-cbc",key,Buffer.alloc(16,32));return Buffer.concat([decipher.update(encrypted.subarray(3)),decipher.final()]).toString("utf8")}
function accountScope(userData,keyText){const disk=JSON.parse(fs.readFileSync(path.join(userData,"sand-secrets.json"),"utf8")),raw=disk["cursor-accounts"],accounts=typeof raw==="string"?JSON.parse(raw):raw,active=accounts?.accounts?.[accounts?.active],encrypted=active?.["cursor-access-token"];if(typeof encrypted!=="string"||!encrypted)throw new Error("active-account-unavailable");const token=decrypt(encrypted,keyText),parts=token.split(".");let principal=token;if(parts.length===3)try{const payload=JSON.parse(Buffer.from(parts[1],"base64url").toString("utf8"));if(typeof payload?.sub==="string"&&payload.sub)principal=payload.sub}catch{}return crypto.createHash("sha256").update(principal).digest("hex")}
const userData=path.join(os.homedir(),"Library","Application Support","Grok Bot"),descriptorPath=path.join(userData,"gateway-descriptor.json"),keyText=password(),scope=accountScope(userData,keyText),disk=JSON.parse(fs.readFileSync(descriptorPath,"utf8")),entry=disk.entries?.[scope];
if(!entry||typeof entry.encrypted!=="string")throw new Error("descriptor-active-account-mismatch");const connection=JSON.parse(decrypt(entry.encrypted,keyText));
if(typeof connection.baseUrl!=="string"||!connection.baseUrl||typeof connection.token!=="string"||!connection.token)throw new Error("descriptor-incomplete");
const headers={};for(const [name,value] of Object.entries(connection.headers||{}))if(typeof value==="string"&&value)headers[name]=value;
const accountFingerprint=crypto.createHash("sha256").update(scope).digest("hex").slice(0,16);
process.stdout.write(JSON.stringify({version:1,baseUrl:connection.baseUrl,token:connection.token,headers,relayPath:"/sand-stream-relay/aiserver.v1.InferenceService/Stream",accountFingerprint}));
'''


def _dpapi_decrypt(encrypted: bytes) -> bytes:
    """Decrypt one raw CurrentUser DPAPI blob (Windows only)."""
    import ctypes.wintypes

    class _BLOB(ctypes.Structure):
        _fields_ = [
            ("cbData", ctypes.wintypes.DWORD),
            ("pbData", ctypes.POINTER(ctypes.c_ubyte)),
        ]

    if not encrypted:
        raise SandToolError("Windows DPAPI 输入为空")
    crypt32 = ctypes.WinDLL("crypt32", use_last_error=True)
    kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
    crypt32.CryptUnprotectData.argtypes = [
        ctypes.POINTER(_BLOB),
        ctypes.c_void_p,
        ctypes.c_void_p,
        ctypes.c_void_p,
        ctypes.c_void_p,
        ctypes.wintypes.DWORD,
        ctypes.POINTER(_BLOB),
    ]
    crypt32.CryptUnprotectData.restype = ctypes.wintypes.BOOL
    kernel32.LocalFree.argtypes = [ctypes.c_void_p]
    kernel32.LocalFree.restype = ctypes.c_void_p

    buf = ctypes.create_string_buffer(encrypted, len(encrypted))
    blob_in = _BLOB(
        len(encrypted),
        ctypes.cast(buf, ctypes.POINTER(ctypes.c_ubyte)),
    )
    blob_out = _BLOB()

    if not crypt32.CryptUnprotectData(
        ctypes.byref(blob_in), None, None, None, None, 0, ctypes.byref(blob_out)
    ):
        error_code = ctypes.get_last_error()
        raise SandToolError(
            "Windows DPAPI CryptUnprotectData 解密失败"
            f"（WinError {error_code}）；请确认当前 Windows 用户即安装 Grok Bot 的用户"
        )

    try:
        return ctypes.string_at(blob_out.pbData, blob_out.cbData)
    finally:
        if blob_out.pbData:
            kernel32.LocalFree(ctypes.cast(blob_out.pbData, ctypes.c_void_p))


def _windows_os_crypt_key(user_data_dir: Path) -> bytes:
    """Recover Electron's AES key from Local State through CurrentUser DPAPI."""
    local_state_path = user_data_dir / "Local State"
    try:
        local_state = json.loads(local_state_path.read_text(encoding="utf-8"))
        encoded_key = local_state["os_crypt"]["encrypted_key"]
        wrapped_key = base64.b64decode(encoded_key, validate=True)
    except Exception as exc:
        raise SandToolError(
            f"无法读取 Grok Bot Windows OSCrypt 主密钥：{local_state_path}"
        ) from exc
    if not wrapped_key.startswith(b"DPAPI"):
        prefix = wrapped_key[:5].decode("ascii", errors="replace")
        raise SandToolError(
            "Grok Bot Windows OSCrypt 主密钥格式不受支持"
            f"（前缀 {prefix!r}，预期 'DPAPI'）"
        )
    key = _dpapi_decrypt(wrapped_key[5:])
    if len(key) != 32:
        raise SandToolError(
            f"Grok Bot Windows OSCrypt 主密钥长度异常：{len(key)}（预期 32）"
        )
    return key


def _windows_aes_gcm_decrypt(
    key: bytes,
    nonce: bytes,
    ciphertext: bytes,
    tag: bytes,
) -> bytes:
    """Decrypt Electron v10 data through Windows CNG, without Python packages."""
    import ctypes.wintypes

    class _AUTH_INFO(ctypes.Structure):
        _fields_ = [
            ("cbSize", ctypes.wintypes.ULONG),
            ("dwInfoVersion", ctypes.wintypes.ULONG),
            ("pbNonce", ctypes.c_void_p),
            ("cbNonce", ctypes.wintypes.ULONG),
            ("pbAuthData", ctypes.c_void_p),
            ("cbAuthData", ctypes.wintypes.ULONG),
            ("pbTag", ctypes.c_void_p),
            ("cbTag", ctypes.wintypes.ULONG),
            ("pbMacContext", ctypes.c_void_p),
            ("cbMacContext", ctypes.wintypes.ULONG),
            ("cbAAD", ctypes.wintypes.ULONG),
            ("cbData", ctypes.c_ulonglong),
            ("dwFlags", ctypes.wintypes.ULONG),
        ]

    if len(key) != 32 or len(nonce) != 12 or len(tag) != 16 or not ciphertext:
        raise SandToolError("Grok Bot Windows v10 AES-GCM 数据格式无效")

    bcrypt = ctypes.WinDLL("bcrypt", use_last_error=True)
    status_type = ctypes.c_long
    bcrypt.BCryptOpenAlgorithmProvider.argtypes = [
        ctypes.POINTER(ctypes.c_void_p),
        ctypes.c_wchar_p,
        ctypes.c_wchar_p,
        ctypes.wintypes.ULONG,
    ]
    bcrypt.BCryptOpenAlgorithmProvider.restype = status_type
    bcrypt.BCryptSetProperty.argtypes = [
        ctypes.c_void_p,
        ctypes.c_wchar_p,
        ctypes.c_void_p,
        ctypes.wintypes.ULONG,
        ctypes.wintypes.ULONG,
    ]
    bcrypt.BCryptSetProperty.restype = status_type
    bcrypt.BCryptGetProperty.argtypes = [
        ctypes.c_void_p,
        ctypes.c_wchar_p,
        ctypes.c_void_p,
        ctypes.wintypes.ULONG,
        ctypes.POINTER(ctypes.wintypes.ULONG),
        ctypes.wintypes.ULONG,
    ]
    bcrypt.BCryptGetProperty.restype = status_type
    bcrypt.BCryptImportKey.argtypes = [
        ctypes.c_void_p,
        ctypes.c_void_p,
        ctypes.c_wchar_p,
        ctypes.POINTER(ctypes.c_void_p),
        ctypes.c_void_p,
        ctypes.wintypes.ULONG,
        ctypes.c_void_p,
        ctypes.wintypes.ULONG,
        ctypes.wintypes.ULONG,
    ]
    bcrypt.BCryptImportKey.restype = status_type
    bcrypt.BCryptDecrypt.argtypes = [
        ctypes.c_void_p,
        ctypes.c_void_p,
        ctypes.wintypes.ULONG,
        ctypes.c_void_p,
        ctypes.c_void_p,
        ctypes.wintypes.ULONG,
        ctypes.c_void_p,
        ctypes.wintypes.ULONG,
        ctypes.POINTER(ctypes.wintypes.ULONG),
        ctypes.wintypes.ULONG,
    ]
    bcrypt.BCryptDecrypt.restype = status_type
    bcrypt.BCryptDestroyKey.argtypes = [ctypes.c_void_p]
    bcrypt.BCryptDestroyKey.restype = status_type
    bcrypt.BCryptCloseAlgorithmProvider.argtypes = [
        ctypes.c_void_p,
        ctypes.wintypes.ULONG,
    ]
    bcrypt.BCryptCloseAlgorithmProvider.restype = status_type

    def check(status: int, operation: str) -> None:
        if status != 0:
            raise SandToolError(
                f"Grok Bot Windows AES-GCM {operation}失败"
                f"（NTSTATUS 0x{status & 0xFFFFFFFF:08X}）"
            )

    algorithm = ctypes.c_void_p()
    key_handle = ctypes.c_void_p()
    try:
        check(
            bcrypt.BCryptOpenAlgorithmProvider(
                ctypes.byref(algorithm), "AES", None, 0
            ),
            "初始化",
        )
        chain_mode = ctypes.create_unicode_buffer("ChainingModeGCM")
        check(
            bcrypt.BCryptSetProperty(
                algorithm,
                "ChainingMode",
                ctypes.cast(chain_mode, ctypes.c_void_p),
                ctypes.sizeof(chain_mode),
                0,
            ),
            "设置模式",
        )
        object_length = ctypes.wintypes.ULONG()
        result_length = ctypes.wintypes.ULONG()
        check(
            bcrypt.BCryptGetProperty(
                algorithm,
                "ObjectLength",
                ctypes.byref(object_length),
                ctypes.sizeof(object_length),
                ctypes.byref(result_length),
                0,
            ),
            "读取密钥参数",
        )
        if object_length.value == 0:
            raise SandToolError("Grok Bot Windows AES-GCM 密钥对象长度无效")
        key_object = ctypes.create_string_buffer(object_length.value)
        key_blob_bytes = (
            (0x4D42444B).to_bytes(4, "little")
            + (1).to_bytes(4, "little")
            + len(key).to_bytes(4, "little")
            + key
        )
        key_blob = ctypes.create_string_buffer(key_blob_bytes, len(key_blob_bytes))
        check(
            bcrypt.BCryptImportKey(
                algorithm,
                None,
                "KeyDataBlob",
                ctypes.byref(key_handle),
                key_object,
                object_length.value,
                key_blob,
                len(key_blob_bytes),
                0,
            ),
            "导入密钥",
        )

        nonce_buffer = ctypes.create_string_buffer(nonce, len(nonce))
        ciphertext_buffer = ctypes.create_string_buffer(
            ciphertext, len(ciphertext)
        )
        tag_buffer = ctypes.create_string_buffer(tag, len(tag))
        plaintext_buffer = ctypes.create_string_buffer(len(ciphertext))
        auth_info = _AUTH_INFO()
        auth_info.cbSize = ctypes.sizeof(_AUTH_INFO)
        auth_info.dwInfoVersion = 1
        auth_info.pbNonce = ctypes.cast(nonce_buffer, ctypes.c_void_p)
        auth_info.cbNonce = len(nonce)
        auth_info.pbTag = ctypes.cast(tag_buffer, ctypes.c_void_p)
        auth_info.cbTag = len(tag)
        # Single-shot (non-chained) GCM: cbData must stay 0 per the
        # BCRYPT_AUTHENTICATED_CIPHER_MODE_INFO contract; a non-zero value can
        # yield STATUS_INVALID_PARAMETER on some providers.
        auth_info.cbData = 0
        plaintext_length = ctypes.wintypes.ULONG()
        check(
            bcrypt.BCryptDecrypt(
                key_handle,
                ciphertext_buffer,
                len(ciphertext),
                ctypes.byref(auth_info),
                None,
                0,
                plaintext_buffer,
                len(ciphertext),
                ctypes.byref(plaintext_length),
                0,
            ),
            "认证解密",
        )
        return plaintext_buffer.raw[: plaintext_length.value]
    finally:
        if key_handle.value:
            bcrypt.BCryptDestroyKey(key_handle)
        if algorithm.value:
            bcrypt.BCryptCloseAlgorithmProvider(algorithm, 0)


def _windows_decrypt_safe_storage_value(
    encrypted_base64: str,
    user_data_dir: Path,
) -> bytes:
    try:
        encrypted = base64.b64decode(encrypted_base64, validate=True)
    except Exception as exc:
        raise SandToolError("Grok Bot Windows 安全存储 base64 解码失败") from exc
    if encrypted.startswith(b"v20"):
        raise SandToolError(
            "Grok Bot 使用 Windows App-Bound Encryption v20，"
            "当前无法由外部安装器安全解密"
        )
    if encrypted.startswith((b"v10", b"v11")):
        if len(encrypted) <= 3 + 12 + 16:
            raise SandToolError("Grok Bot Windows v10 安全存储数据过短")
        payload = encrypted[3:]
        key = _windows_os_crypt_key(user_data_dir)
        return _windows_aes_gcm_decrypt(
            key,
            payload[:12],
            payload[12:-16],
            payload[-16:],
        )
    # Chromium/Electron legacy values without a version prefix are raw DPAPI.
    return _dpapi_decrypt(encrypted)


def _account_scope_from_access_token(access_token: str) -> str:
    principal = access_token
    parts = access_token.split(".")
    if len(parts) == 3:
        try:
            payload_bytes = base64.urlsafe_b64decode(
                parts[1] + "=" * (-len(parts[1]) % 4)
            )
            payload = json.loads(payload_bytes.decode("utf-8"))
            subject = payload.get("sub") if isinstance(payload, dict) else None
            if isinstance(subject, str) and subject:
                principal = subject
        except Exception:
            pass
    return hashlib.sha256(principal.encode("utf-8")).hexdigest()


def _windows_active_grok_account_scope(user_data_dir: Path) -> str:
    secrets_path = user_data_dir / "sand-secrets.json"
    try:
        disk = json.loads(secrets_path.read_text(encoding="utf-8"))
        accounts_value = disk["cursor-accounts"]
        accounts = (
            json.loads(accounts_value)
            if isinstance(accounts_value, str)
            else accounts_value
        )
        records = accounts["accounts"]
        active_key = accounts["active"]
        if isinstance(records, dict):
            active = records.get(active_key)
            if active is None:
                active = records.get(str(active_key))
        elif isinstance(records, list) and isinstance(active_key, int):
            active = records[active_key]
        else:
            active = None
        encrypted_access_token = active["cursor-access-token"]
        if not isinstance(encrypted_access_token, str) or not encrypted_access_token:
            raise ValueError("active access token is absent")
    except Exception as exc:
        raise SandToolError(
            f"无法识别 Grok Bot 当前登录账号：{secrets_path}"
        ) from exc
    access_token_bytes = _windows_decrypt_safe_storage_value(
        encrypted_access_token,
        user_data_dir,
    )
    try:
        access_token = access_token_bytes.decode("utf-8")
    except UnicodeDecodeError as exc:
        raise SandToolError("Grok Bot 当前账号凭据格式无效") from exc
    return _account_scope_from_access_token(access_token)


def _grok_descriptor_path_windows() -> Path:
    """Locate Grok Bot gateway-descriptor.json on Windows."""
    appdata = os.environ.get("APPDATA", "")
    localappdata = os.environ.get("LOCALAPPDATA", "")
    candidates: List[Path] = []
    if appdata:
        candidates.append(Path(appdata) / "Grok Bot" / "gateway-descriptor.json")
    if localappdata:
        candidates.append(
            Path(localappdata) / "Grok Bot" / "gateway-descriptor.json"
        )
    candidates.append(
        Path.home() / "AppData" / "Roaming" / "Grok Bot" / "gateway-descriptor.json"
    )
    existing: List[Path] = []
    for candidate in candidates:
        if candidate.is_file():
            existing.append(candidate)
            if (candidate.parent / "Local State").is_file():
                return candidate
    if existing:
        # Return the real descriptor so the next error names its missing
        # sibling Local State file rather than claiming the descriptor is absent.
        return existing[0]
    raise SandToolError(
        "未找到 Grok Bot gateway descriptor；"
        "请先保持 Grok Bot 已登录并打开，然后重新运行 install"
    )


def _write_grok_relay_config_windows() -> Path:
    """Decrypt Grok Bot's Windows descriptor via DPAPI-backed OSCrypt."""
    descriptor_path = _grok_descriptor_path_windows()
    try:
        disk = json.loads(descriptor_path.read_text(encoding="utf-8"))
    except Exception as exc:
        raise SandToolError(
            f"无法读取 Grok Bot gateway descriptor：{descriptor_path}"
        ) from exc

    entries = disk.get("entries") if isinstance(disk, dict) else None
    if not isinstance(entries, dict):
        raise SandToolError(
            "Grok Bot gateway descriptor 中无有效加密凭据；请先登录 Grok Bot"
        )
    account_scope = _windows_active_grok_account_scope(descriptor_path.parent)
    entry = entries.get(account_scope)
    if not isinstance(entry, dict) or not isinstance(entry.get("encrypted"), str):
        raise SandToolError(
            "当前 Grok Bot 登录账号没有对应的 gateway descriptor；"
            "请打开该账号的目标 Box，等待连接完成后重试"
        )

    try:
        plaintext = _windows_decrypt_safe_storage_value(
            entry["encrypted"],
            descriptor_path.parent,
        )
        connection = json.loads(plaintext.decode("utf-8"))
    except SandToolError:
        raise
    except Exception as exc:
        raise SandToolError(
            "Grok Bot gateway descriptor Windows 安全存储解密或解析失败"
        ) from exc

    if (
        not isinstance(connection, dict)
        or not isinstance(connection.get("baseUrl"), str)
        or not str(connection.get("baseUrl")).startswith("https://")
        or not isinstance(connection.get("token"), str)
        or not connection.get("token")
    ):
        raise SandToolError("Grok Bot gateway descriptor 解密结果缺少必要字段")

    headers: Dict[str, str] = {}
    for name, value in (connection.get("headers") or {}).items():
        if isinstance(value, str) and value:
            headers[name] = value

    relay_data: Dict[str, object] = {
        "version": 1,
        "baseUrl": connection["baseUrl"],
        "token": connection["token"],
        "headers": headers,
        "relayPath": "/sand-stream-relay/aiserver.v1.InferenceService/Stream",
        "accountFingerprint": hashlib.sha256(
            account_scope.encode("ascii")
        ).hexdigest()[:16],
    }

    path = _relay_config_path()
    _write_json_atomic(path, relay_data)
    return path


def _write_grok_relay_config_macos() -> Path:
    """Read Grok Bot gateway descriptor on macOS via Node + Keychain."""
    node = shutil.which("node")
    if not node:
        raise SandToolError("未找到 node，无法读取 Grok Bot gateway descriptor")
    try:
        result = subprocess.run(
            [node, "-e", GROK_GATEWAY_DESCRIPTOR_READER],
            check=False,
            capture_output=True,
            text=True,
            timeout=20,
        )
    except Exception as exc:
        raise SandToolError("读取 Grok Bot gateway descriptor 失败") from exc
    if result.returncode != 0:
        raise SandToolError(
            "无法读取 Grok Bot gateway descriptor；请先保持 Grok Bot 已登录并打开，"
            "然后重新运行 install"
        )
    try:
        value = json.loads(result.stdout)
    except Exception as exc:
        raise SandToolError("Grok Bot gateway descriptor 返回格式无效") from exc
    if (
        not isinstance(value, dict)
        or value.get("version") != 1
        or not isinstance(value.get("baseUrl"), str)
        or not str(value.get("baseUrl")).startswith("https://")
        or not isinstance(value.get("token"), str)
        or not value.get("token")
        or not isinstance(value.get("headers"), dict)
    ):
        raise SandToolError("Grok Bot gateway descriptor 缺少必要字段")
    path = _relay_config_path()
    _write_json_atomic(path, value)
    return path


def _write_grok_relay_config_from_box() -> Path:
    # Unified source: derive the relay target from EnsureSandBox using the Cursor
    # state.vscdb account token.  No Grok Bot desktop files are read, so the Grok
    # Bot client can be uninstalled; the Box and billing follow the Cursor login.
    response = _grok_rpc("EnsureSandBox", _pb_bool(2, True))
    gateway_url = ""
    gateway_token = ""
    network_token = ""
    run_state = 0
    for field, wire, value in _pb_iter_fields(response):
        if wire == 2 and field == 10:
            gateway_url = value.decode("utf-8", errors="replace")
        elif wire == 2 and field == 11:
            gateway_token = value.decode("utf-8", errors="replace")
        elif wire == 2 and field == 4:
            network_token = value.decode("utf-8", errors="replace")
        elif wire == 0 and field == 13:
            run_state = value
    if not gateway_url.startswith("https://") or not gateway_token:
        raise SandToolError(
            "EnsureSandBox 未返回可用的 Box gateway（Box 可能尚未就绪，稍后重试）；"
            f"runState={run_state}"
        )
    headers: Dict[str, str] = {}
    if network_token:
        headers["x-anyrun-network-token"] = network_token
    fingerprint = ""
    cursor_token = ""
    machine_id = ""
    try:
        cursor_token = _cursor_state_value("cursorAuth/accessToken")
        fingerprint = hashlib.sha256(
            _account_scope_from_access_token(cursor_token).encode("ascii")
        ).hexdigest()[:16]
    except SandToolError:
        cursor_token = ""
    try:
        machine_id = _cursor_state_value("storage.serviceMachineId")
    except SandToolError:
        machine_id = ""
    relay_data: Dict[str, object] = {
        "version": 1,
        "baseUrl": gateway_url,
        "token": gateway_token,
        "headers": headers,
        "relayPath": BOX_RELAY_PATH,
        "accountFingerprint": fingerprint,
        "runState": run_state,
        "mintedAtMs": int(time.time() * 1000),
        "refreshAfterMs": 3600000,
    }
    # Self-refresh block: lets the injected transport re-mint the short-lived
    # gateway token on its own via EnsureSandBox, using the (long-lived, ~months)
    # Cursor account token.  Stored 0600; enables removing the Grok Bot client.
    if cursor_token:
        relay_data["refresh"] = {
            "backendUrl": _grok_backend_url(),
            "accessToken": cursor_token,
            "machineId": machine_id,
        }
    path = _relay_config_path()
    _write_json_atomic(path, relay_data)
    return path


def _write_grok_relay_config() -> Path:
    # Unified on the Cursor account's Box via EnsureSandBox; the legacy Grok Bot
    # descriptor readers (_write_grok_relay_config_windows/_macos) are retained
    # only for reference and are no longer used.
    return _write_grok_relay_config_from_box()


def _remove_grok_relay_config() -> None:
    try:
        _relay_config_path().unlink(missing_ok=True)
    except OSError as exc:
        raise SandToolError("无法删除 Grok Bot gateway relay 配置") from exc


def _load_grok_relay_config(refresh: bool = True) -> Mapping[str, object]:
    path = _write_grok_relay_config() if refresh else _relay_config_path()
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except Exception as exc:
        raise SandToolError("无法读取本地 Grok Bot Box relay 配置") from exc
    if (
        not isinstance(value, dict)
        or not isinstance(value.get("baseUrl"), str)
        or not str(value.get("baseUrl")).startswith("https://")
        or not isinstance(value.get("token"), str)
        or not value.get("token")
        or not isinstance(value.get("headers"), dict)
    ):
        raise SandToolError("本地 Grok Bot Box relay 配置缺少必要字段")
    return value


def _box_gateway_url(config: Mapping[str, object], path: str) -> str:
    from urllib.parse import urlsplit, urlunsplit

    base = urlsplit(str(config["baseUrl"]))
    if (
        base.scheme != "https"
        or not base.netloc
        or base.username is not None
        or base.password is not None
    ):
        raise SandToolError("Grok Bot Box gateway 地址无效")
    # Preserve any base path prefix instead of discarding it: the Box relay and
    # the gateway /events, /api/sendPrompt endpoints all live under baseUrl.
    joined = base.path.rstrip("/") + "/" + path.lstrip("/")
    return urlunsplit((base.scheme, base.netloc, joined, "", ""))


def _box_gateway_headers(
    config: Mapping[str, object],
    extra: Optional[Mapping[str, str]] = None,
) -> Dict[str, str]:
    blocked = {"authorization", "host", "content-length", "transfer-encoding"}
    headers: Dict[str, str] = {}
    values = config.get("headers")
    if isinstance(values, dict):
        for name, value in values.items():
            if (
                isinstance(name, str)
                and isinstance(value, str)
                and value
                and name.casefold() not in blocked
            ):
                headers[name] = value
    headers["Authorization"] = "Bearer " + str(config["token"])
    if extra:
        overridden = {name.casefold() for name in extra}
        headers = {
            name: value
            for name, value in headers.items()
            if name.casefold() not in overridden
        }
        headers.update(extra)
    return headers


def _box_http_request(
    config: Mapping[str, object],
    path: str,
    *,
    method: str,
    data: Optional[bytes] = None,
    headers: Optional[Mapping[str, str]] = None,
    timeout: float = 20,
) -> Tuple[int, Dict[str, str], bytes]:
    from urllib.error import HTTPError, URLError
    from urllib.request import Request, urlopen

    request = Request(
        _box_gateway_url(config, path),
        data=data,
        headers=_box_gateway_headers(config, headers),
        method=method,
    )
    try:
        with urlopen(request, timeout=timeout) as response:
            body = response.read(1024 * 1024 + 1)
            if len(body) > 1024 * 1024:
                raise SandToolError("Grok Bot Box gateway 响应过大")
            return response.status, dict(response.headers.items()), body
    except HTTPError as exc:
        try:
            body = exc.read(64 * 1024)
        finally:
            exc.close()
        return exc.code, dict(exc.headers.items()), body
    except (URLError, TimeoutError, OSError) as exc:
        raise SandToolError("无法连接当前 Grok Bot Box gateway") from exc


def _connect_stream_frame_summary(body: bytes) -> Tuple[bool, bool, int]:
    # Connect streaming frames are 5-byte prefixed: flags(1) + length(4 BE).
    # The relay forwards the upstream Stream, which always terminates with an
    # end-stream frame (flags & 0x02).  A plain HTML/JSON 200 page produced by
    # some other handler will not parse into such a frame.
    offset = 0
    saw_end = False
    compressed_frames = 0
    while offset + 5 <= len(body):
        flags = body[offset]
        length = int.from_bytes(body[offset + 1 : offset + 5], "big")
        offset += 5
        if offset + length > len(body):
            return False, False, 0
        compressed_frames += int(bool(flags & 0x01))
        if flags & 0x02:
            saw_end = True
        offset += length
    return offset == len(body), saw_end, compressed_frames


def _connect_stream_has_end_frame(body: bytes) -> bool:
    valid, saw_end, _compressed_frames = _connect_stream_frame_summary(body)
    return valid and saw_end


def _response_content_type(headers: Mapping[str, str]) -> str:
    return next(
        (
            value
            for name, value in headers.items()
            if name.casefold() == "content-type"
        ),
        "",
    )


def _response_header(headers: Mapping[str, str], expected: str) -> str:
    return next(
        (
            value
            for name, value in headers.items()
            if name.casefold() == expected.casefold()
        ),
        "",
    )


def _probe_box_relay(
    config: Mapping[str, object],
) -> Tuple[int, str, bool, str]:
    import uuid

    status, response_headers, body = _box_http_request(
        config,
        BOX_RELAY_STATUS_PATH,
        method="GET",
        timeout=20,
    )
    content_type = _response_content_type(response_headers)
    if status != 200:
        return status, content_type, False, f"status endpoint HTTP {status}"
    try:
        relay_status = json.loads(body.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError):
        return status, content_type, False, "status endpoint did not return JSON"
    if not isinstance(relay_status, dict):
        return status, content_type, False, "status endpoint JSON was not an object"
    expected = {
        "routeVersion": BOX_RELAY_ROUTE_VERSION,
        "upstreamClientVersion": BOX_RELAY_CLIENT_VERSION,
        "configVersionMode": "strip",
        "requestCompression": "gzip-forwarded",
        "responseCompression": "gzip-declared",
    }
    mismatches = [
        f"{name}={relay_status.get(name)!r}"
        for name, value in expected.items()
        if relay_status.get(name) != value
    ]
    if mismatches:
        return (
            status,
            content_type,
            False,
            "stale relay contract: " + ", ".join(mismatches),
        )

    compressed_payload = gzip.compress(b"")
    compressed_frame = (
        b"\x01"
        + len(compressed_payload).to_bytes(4, "big")
        + compressed_payload
    )
    status, response_headers, body = _box_http_request(
        config,
        BOX_RELAY_PATH,
        method="POST",
        data=compressed_frame,
        headers={
            "Content-Type": "application/connect+proto",
            "Connect-Protocol-Version": "1",
            "Connect-Content-Encoding": "gzip",
            "Connect-Accept-Encoding": "gzip",
            "X-Request-Id": str(uuid.uuid4()),
        },
        timeout=25,
    )
    content_type = _response_content_type(response_headers)
    if b"ERROR_OUTDATED_CLIENT" in body or b'"actionRequired":"config"' in body:
        return status, content_type, False, "relay returned outdated/config action"
    if b"received compressed envelope, but do not know how to decompress" in body:
        return (
            status,
            content_type,
            False,
            "relay dropped the request connect-content-encoding: gzip declaration",
        )
    valid_frames, saw_end, compressed_frames = _connect_stream_frame_summary(body)
    connect_encoding = _response_header(
        response_headers, "connect-content-encoding"
    ).strip()
    if (
        status == 200
        and content_type.casefold().startswith("application/connect+proto")
        and connect_encoding.casefold() != "gzip"
    ):
        return (
            status,
            content_type,
            False,
            "missing connect-content-encoding: gzip on Connect response",
        )
    relay_ok = (
        status == 200
        and content_type.casefold().startswith("application/connect+proto")
        and valid_frames
        and saw_end
    )
    diagnosis = (
        f"{BOX_RELAY_ROUTE_VERSION} request/response gzip and Connect framing verified"
        if relay_ok
        else "invalid Connect response"
    )
    return status, content_type, relay_ok, diagnosis


def _active_agent_from_gateway_event(
    value: object,
) -> Tuple[Optional[str], Set[str]]:
    candidates: Set[str] = set()
    stack = [value]
    while stack:
        current = stack.pop()
        if isinstance(current, dict):
            active = current.get("activeAgentId")
            if isinstance(active, str) and active:
                return active, candidates
            agents = current.get("agents")
            if isinstance(agents, list):
                for agent in agents:
                    if isinstance(agent, dict):
                        agent_id = agent.get("id") or agent.get("agentId")
                        if isinstance(agent_id, str) and agent_id:
                            candidates.add(agent_id)
            for key in ("payload", "event"):
                nested = current.get(key)
                if isinstance(nested, (dict, list)):
                    stack.append(nested)
        elif isinstance(current, list):
            stack.extend(current)
    return None, candidates


def _read_active_box_agent_id(
    config: Mapping[str, object],
    timeout_seconds: float = 20,
) -> str:
    from urllib.error import HTTPError, URLError
    from urllib.request import Request, urlopen

    request = Request(
        _box_gateway_url(config, "/events"),
        headers=_box_gateway_headers(config, {"Accept": "text/event-stream"}),
        method="GET",
    )
    candidates: Set[str] = set()
    deadline = time.monotonic() + timeout_seconds
    data_lines: List[str] = []
    buffered_size = 0
    try:
        with urlopen(request, timeout=5) as response:
            if response.status != 200:
                raise SandToolError(
                    f"Grok Bot Box events 返回 HTTP {response.status}"
                )
            for raw_line in response:
                if time.monotonic() >= deadline:
                    break
                if len(raw_line) > 256 * 1024:
                    raise SandToolError("Grok Bot Box events 单行过大")
                line = raw_line.decode("utf-8", errors="replace").rstrip("\r\n")
                if line.startswith("data:"):
                    data = line[5:].lstrip()
                    data_lines.append(data)
                    buffered_size += len(data)
                    if buffered_size > 1024 * 1024:
                        raise SandToolError("Grok Bot Box events 数据过大")
                    continue
                if line or not data_lines:
                    continue
                try:
                    event = json.loads("\n".join(data_lines))
                except Exception:
                    data_lines = []
                    buffered_size = 0
                    continue
                data_lines = []
                buffered_size = 0
                active, found = _active_agent_from_gateway_event(event)
                if active:
                    return active
                candidates.update(found)
    except HTTPError as exc:
        raise SandToolError(
            f"Grok Bot Box events 返回 HTTP {exc.code}"
        ) from exc
    except (URLError, TimeoutError, OSError):
        pass
    if len(candidates) == 1:
        return next(iter(candidates))
    raise SandToolError(
        "无法确定当前 Grok Bot Box Agent；请在 Grok Bot 中打开目标 Box Agent 后重试"
    )


def _find_agent_id_in_record(value: object) -> str:
    # The gateway createAgent reply is a JSON "record"; locate its agent id.
    stack = [value]
    fallback = ""
    while stack:
        current = stack.pop()
        if isinstance(current, dict):
            for key in ("agentId", "id"):
                candidate = current.get(key)
                if isinstance(candidate, str) and candidate:
                    if key == "agentId":
                        return candidate
                    if not fallback:
                        fallback = candidate
            for nested in current.values():
                if isinstance(nested, (dict, list)):
                    stack.append(nested)
        elif isinstance(current, list):
            stack.extend(current)
    return fallback


def _gateway_create_box_agent(config: Mapping[str, object]) -> str:
    # Create the agent inside the Box via the gateway.  Backend-listed agents are
    # unknown to the Box coordinator ("Sand agent ... does not exist"); the Box
    # gateway's /api/createAgent instantiates it locally and returns its id.
    import uuid

    body = json.dumps(
        {
            "name": "Cursor Sand Relay",
            "description": "Hosts the Cursor Sand Stream relay route.",
            "creationRoute": {"kind": "box"},
            "harness": "box",
            "isIntroductionSuppressed": True,
            "isKickstartRequested": False,
            "clientNonce": str(uuid.uuid4()),
            "supportsTemporalHarness": True,
        },
        ensure_ascii=False,
    ).encode("utf-8")
    status, _headers, response_body = _box_http_request(
        config,
        "/api/createAgent",
        method="POST",
        data=body,
        headers={
            "Content-Type": "application/json",
            "x-sand-slim-avatars": "1",
        },
        timeout=30,
    )
    detail = response_body.decode("utf-8", errors="replace")
    if status < 200 or status >= 300:
        raise SandToolError(
            f"在 Box 内创建 relay agent 失败：HTTP {status} {detail[:400]}"
        )
    try:
        record = json.loads(detail)
    except Exception as exc:
        raise SandToolError("Box 网关 createAgent 返回格式无效") from exc
    agent_id = _find_agent_id_in_record(record)
    if not agent_id:
        raise SandToolError(
            f"Box 网关 createAgent 未返回 agent id：{detail[:300]}"
        )
    return agent_id


def _send_box_provision_prompt(
    config: Mapping[str, object],
    agent_id: str,
) -> None:
    import uuid
    from urllib.request import Request, urlopen

    # The Box coordinator rejects /api/sendPrompt (HTTP 500) unless a client
    # /events subscription is attached.  Open /events first and hold it open for
    # the duration of the POST, mirroring the desktop client's proven sequence.
    events_resp = None
    try:
        events_req = Request(
            _box_gateway_url(config, "/events"),
            headers=_box_gateway_headers(config, {"Accept": "text/event-stream"}),
            method="GET",
        )
        try:
            events_resp = urlopen(events_req, timeout=15)
        except Exception:
            events_resp = None

        body = json.dumps(
            {
                "prompt": BOX_RELAY_PROVISION_PROMPT,
                "agentId": agent_id,
                "clientNonce": str(uuid.uuid4()),
                "source": "desktop",
                "sessionId": "",
            },
            ensure_ascii=False,
        ).encode("utf-8")
        status, _headers, response_body = _box_http_request(
            config,
            "/api/sendPrompt",
            method="POST",
            data=body,
            headers={
                "Content-Type": "application/json",
                "x-sand-slim-avatars": "1",
            },
            timeout=25,
        )
    finally:
        if events_resp is not None:
            try:
                events_resp.close()
            except Exception:
                pass
    if status < 200 or status >= 300:
        detail = ""
        try:
            detail = response_body.decode("utf-8", errors="replace")[:400]
        except Exception:
            detail = ""
        raise SandToolError(
            f"向 Grok Bot Box Agent 发送初始化指令失败：HTTP {status} {detail}"
        )
    try:
        response = json.loads(response_body.decode("utf-8"))
    except Exception as exc:
        raise SandToolError("Grok Bot Box Agent 返回格式无效") from exc
    if not isinstance(response, dict) or response.get("accepted") is not True:
        raise SandToolError("Grok Bot Box Agent 未接受初始化指令")


GROK_BACKEND_DEFAULT_URL = "https://api2.cursor.sh"


def _cursor_state_db_path() -> Path:
    if sys.platform == "win32":
        base = os.environ.get("APPDATA")
        root = Path(base) if base else Path.home() / "AppData" / "Roaming"
    elif sys.platform == "darwin":
        root = Path.home() / "Library" / "Application Support"
    else:
        root = Path(os.environ.get("XDG_CONFIG_HOME") or (Path.home() / ".config"))
    return root / "Cursor" / "User" / "globalStorage" / "state.vscdb"


def _cursor_state_value(key: str) -> str:
    import sqlite3

    path = _cursor_state_db_path()
    if not path.is_file():
        raise SandToolError(f"未找到 Cursor 状态库：{path}")
    uri = "file:" + str(path).replace("?", "%3F").replace("#", "%23")
    uri += "?mode=ro&immutable=1"
    try:
        con = sqlite3.connect(uri, uri=True, timeout=5)
    except sqlite3.Error as exc:
        raise SandToolError("无法打开 Cursor 状态库") from exc
    try:
        row = con.execute(
            "select value from ItemTable where key=? limit 1", (key,)
        ).fetchone()
    except sqlite3.Error as exc:
        raise SandToolError("读取 Cursor 状态库失败") from exc
    finally:
        con.close()
    if not row or row[0] is None:
        raise SandToolError(f"Cursor 状态库缺少 {key}；请确认已在 Cursor 登录")
    raw = row[0]
    if isinstance(raw, bytes):
        raw = raw.decode("utf-8", errors="replace")
    raw = str(raw)
    try:
        decoded = json.loads(raw)
        if isinstance(decoded, str):
            return decoded
    except Exception:
        pass
    return raw


def _cursor_checksum(machine_id: str) -> str:
    # Byte-for-byte port of Grok Bot 0.44's createCursorChecksum, replicating the
    # JS 32-bit shift semantics (a>>b coerces to int32, shift count masked &31).
    epoch = int(time.time() * 1000) // 1_000_000

    def js_shr(value: int, count: int) -> int:
        v = value & 0xFFFFFFFF
        if v & 0x80000000:
            v -= 0x100000000
        return v >> (count & 31)

    raw = [
        js_shr(epoch, 40) & 255,
        js_shr(epoch, 32) & 255,
        js_shr(epoch, 24) & 255,
        js_shr(epoch, 16) & 255,
        js_shr(epoch, 8) & 255,
        epoch & 255,
    ]
    previous = 165
    for index in range(len(raw)):
        raw[index] = ((raw[index] ^ previous) + (index % 256)) & 255
        previous = raw[index]
    prefix = base64.urlsafe_b64encode(bytes(raw)).decode("ascii").rstrip("=")
    return prefix + machine_id


def _grok_backend_url() -> str:
    from urllib.parse import urlsplit

    value = (
        os.environ.get("SAND_BACKEND_URL")
        or os.environ.get("CURSOR_API_BASE_URL")
        or GROK_BACKEND_DEFAULT_URL
    )
    base = urlsplit(value)
    if base.scheme != "https" or not base.netloc:
        raise SandToolError("Grok Bot 后端地址无效")
    return value.rstrip("/")


def _pb_varint(value: int) -> bytes:
    if value < 0:
        value += 1 << 64
    out = bytearray()
    while True:
        byte = value & 0x7F
        value >>= 7
        if value:
            out.append(byte | 0x80)
        else:
            out.append(byte)
            return bytes(out)


def _pb_read_varint(data: bytes, offset: int) -> Tuple[int, int]:
    result = 0
    shift = 0
    while True:
        if offset >= len(data):
            raise SandToolError("protobuf varint 越界")
        byte = data[offset]
        offset += 1
        result |= (byte & 0x7F) << shift
        if not (byte & 0x80):
            return result, offset
        shift += 7
        if shift > 70:
            raise SandToolError("protobuf varint 过长")


def _pb_tag(field: int, wire: int) -> bytes:
    return _pb_varint((field << 3) | wire)


def _pb_str(field: int, value: str) -> bytes:
    data = value.encode("utf-8")
    return _pb_tag(field, 2) + _pb_varint(len(data)) + data


def _pb_uint(field: int, value: int) -> bytes:
    return _pb_tag(field, 0) + _pb_varint(value)


def _pb_bool(field: int, value: bool) -> bytes:
    return _pb_tag(field, 0) + _pb_varint(1 if value else 0)


def _pb_iter_fields(data: bytes):
    offset = 0
    length = len(data)
    while offset < length:
        key, offset = _pb_read_varint(data, offset)
        field = key >> 3
        wire = key & 7
        if wire == 0:
            value, offset = _pb_read_varint(data, offset)
            yield field, wire, value
        elif wire == 2:
            size, offset = _pb_read_varint(data, offset)
            end = offset + size
            if end > length:
                raise SandToolError("protobuf 长度越界")
            yield field, wire, data[offset:end]
            offset = end
        elif wire == 1:
            yield field, wire, data[offset:offset + 8]
            offset += 8
        elif wire == 5:
            yield field, wire, data[offset:offset + 4]
            offset += 4
        else:
            raise SandToolError(f"不支持的 protobuf wire 类型 {wire}")


def _jwt_payload(token: str) -> Optional[Mapping[str, object]]:
    parts = token.split(".")
    if len(parts) != 3:
        return None
    try:
        payload = json.loads(
            base64.urlsafe_b64decode(
                parts[1] + "=" * (-len(parts[1]) % 4)
            ).decode("utf-8")
        )
    except Exception:
        return None
    return payload if isinstance(payload, dict) else None


def _jwt_field(token: str, field: str) -> Optional[str]:
    payload = _jwt_payload(token)
    if payload is None:
        return None
    value = payload.get(field)
    return value if isinstance(value, str) and value else None


def _account_label_from_token(token: str) -> str:
    # Prefer a human email claim; fall back to any email-like claim, then to the
    # subject id.  Cursor access tokens do not always carry an email claim.
    payload = _jwt_payload(token)
    if payload is None:
        return "未知（token 非 JWT）"
    for key in ("email", "user_email", "https://cursor.com/email"):
        value = payload.get(key)
        if isinstance(value, str) and value:
            return value
    for key, value in payload.items():
        if "email" in key.lower() and isinstance(value, str) and value:
            return value
    subject = payload.get("sub")
    if isinstance(subject, str) and subject:
        return f"sub:{subject}"
    return "未知（无 email/sub 声明）"


GROK_ACCOUNT_READER = r'''
const crypto=require("node:crypto"),fs=require("node:fs"),os=require("node:os"),path=require("node:path"),cp=require("node:child_process");
function password(){for(const service of ["Grok Bot Safe Storage","Grok Bot"]){try{const value=cp.execFileSync("/usr/bin/security",["find-generic-password","-s",service,"-w"],{encoding:"utf8",stdio:["ignore","pipe","ignore"]}).trim();if(value)return value}catch{}}throw new Error("safe-storage-unavailable")}
function decrypt(value,keyText){const encrypted=Buffer.from(value,"base64");if(encrypted.subarray(0,3).toString("ascii")!=="v10")throw new Error("unsupported-envelope");const key=crypto.pbkdf2Sync(keyText,"saltysalt",1003,16,"sha1"),decipher=crypto.createDecipheriv("aes-128-cbc",key,Buffer.alloc(16,32));return Buffer.concat([decipher.update(encrypted.subarray(3)),decipher.final()]).toString("utf8")}
const userData=path.join(os.homedir(),"Library","Application Support","Grok Bot"),disk=JSON.parse(fs.readFileSync(path.join(userData,"sand-secrets.json"),"utf8")),keyText=password();
const accountsRaw=disk["cursor-accounts"],accounts=typeof accountsRaw==="string"?JSON.parse(accountsRaw):accountsRaw,active=accounts&&accounts.accounts?accounts.accounts[accounts.active]:null;
if(!active||typeof active["cursor-access-token"]!=="string")throw new Error("active-account-unavailable");
const accessToken=decrypt(active["cursor-access-token"],keyText);
let machineId="";try{if(typeof disk["cursor-machine-id"]==="string")machineId=decrypt(disk["cursor-machine-id"],keyText)}catch{}
let email="";try{const p=JSON.parse(Buffer.from(accessToken.split(".")[1],"base64url").toString("utf8"));if(typeof p.email==="string")email=p.email}catch{}
process.stdout.write(JSON.stringify({accessToken,machineId,email}));
'''


def _grok_userdata_dir_windows() -> Path:
    appdata = os.environ.get("APPDATA", "")
    localappdata = os.environ.get("LOCALAPPDATA", "")
    candidates: List[Path] = []
    if appdata:
        candidates.append(Path(appdata) / "Grok Bot")
    if localappdata:
        candidates.append(Path(localappdata) / "Grok Bot")
    candidates.append(Path.home() / "AppData" / "Roaming" / "Grok Bot")
    for candidate in candidates:
        if (candidate / "sand-secrets.json").is_file() and (
            candidate / "Local State"
        ).is_file():
            return candidate
    for candidate in candidates:
        if (candidate / "sand-secrets.json").is_file():
            return candidate
    raise SandToolError(
        "未找到 Grok Bot 账号数据（sand-secrets.json）；请先登录 Grok Bot"
    )


def _windows_grok_account_credentials() -> Tuple[str, str, str]:
    user_data_dir = _grok_userdata_dir_windows()
    secrets_path = user_data_dir / "sand-secrets.json"
    try:
        disk = json.loads(secrets_path.read_text(encoding="utf-8"))
        accounts_value = disk["cursor-accounts"]
        accounts = (
            json.loads(accounts_value)
            if isinstance(accounts_value, str)
            else accounts_value
        )
        records = accounts["accounts"]
        active_key = accounts["active"]
        if isinstance(records, dict):
            active = records.get(active_key)
            if active is None:
                active = records.get(str(active_key))
        elif isinstance(records, list) and isinstance(active_key, int):
            active = records[active_key]
        else:
            active = None
        encrypted_access_token = active["cursor-access-token"]
        encrypted_machine_id = disk.get("cursor-machine-id")
        if not isinstance(encrypted_access_token, str) or not encrypted_access_token:
            raise ValueError("active access token is absent")
    except Exception as exc:
        raise SandToolError(
            f"无法识别 Grok Bot 当前登录账号：{secrets_path}"
        ) from exc
    access_token = _windows_decrypt_safe_storage_value(
        encrypted_access_token, user_data_dir
    ).decode("utf-8", errors="strict")
    machine_id = ""
    if isinstance(encrypted_machine_id, str) and encrypted_machine_id:
        try:
            machine_id = _windows_decrypt_safe_storage_value(
                encrypted_machine_id, user_data_dir
            ).decode("utf-8", errors="strict")
        except SandToolError:
            machine_id = ""
    return access_token, machine_id, _jwt_field(access_token, "email") or ""


def _macos_grok_account_credentials() -> Tuple[str, str, str]:
    node = shutil.which("node")
    if not node:
        raise SandToolError("未找到 node，无法读取 Grok Bot 账号凭据")
    try:
        result = subprocess.run(
            [node, "-e", GROK_ACCOUNT_READER],
            check=False,
            capture_output=True,
            text=True,
            timeout=20,
        )
    except Exception as exc:
        raise SandToolError("读取 Grok Bot 账号凭据失败") from exc
    if result.returncode != 0:
        raise SandToolError(
            "无法读取 Grok Bot 账号凭据；请确认已登录 Grok Bot 并打开"
        )
    try:
        value = json.loads(result.stdout)
        access_token = value["accessToken"]
        machine_id = value.get("machineId", "")
        email = value.get("email", "")
    except Exception as exc:
        raise SandToolError("Grok Bot 账号凭据返回格式无效") from exc
    if not isinstance(access_token, str) or not access_token:
        raise SandToolError("Grok Bot 账号凭据缺少 access token")
    return access_token, str(machine_id or ""), str(email or "")


_GROK_CREDS_CACHE: Dict[str, Tuple[str, str, str]] = {}


def _grok_account_credentials() -> Tuple[str, str, str]:
    # (accessToken, machineId, email) of the Grok Bot active account — the same
    # account that backs the Box relay descriptor, so control-plane RPCs stay on
    # that account rather than Cursor's separate state.vscdb login.
    cached = _GROK_CREDS_CACHE.get("creds")
    if cached is not None:
        return cached
    if sys.platform == "win32":
        creds = _windows_grok_account_credentials()
    elif sys.platform == "darwin":
        creds = _macos_grok_account_credentials()
    else:
        raise SandToolError("Grok Bot 账号凭据当前仅支持 macOS 和 Windows")
    _GROK_CREDS_CACHE["creds"] = creds
    return creds


def _grok_rpc(
    method_name: str,
    request: bytes,
    timeout: float = 30,
    retries: int = 5,
) -> bytes:
    from urllib.error import HTTPError, URLError
    from urllib.request import Request, urlopen
    import uuid

    # Control plane uses the Cursor state.vscdb account token: that is the login
    # Cursor actually runs as, and the account we create/operate the Grok bot on.
    access_token = _cursor_state_value("cursorAuth/accessToken")
    machine_id = _cursor_state_value("storage.serviceMachineId")
    url = _grok_backend_url() + "/aiserver.v1.GrokBotService/" + method_name

    attempt = 0
    while True:
        headers = {
            "Authorization": "Bearer " + access_token,
            "Connect-Protocol-Version": "1",
            "Content-Type": "application/proto",
            "x-cursor-checksum": _cursor_checksum(machine_id),
            "x-cursor-client-type": "sand",
            "x-cursor-client-version": "0.46.0",
            "x-sand-box-namespace": "prod",
            "x-ghost-mode": "true",
            "x-request-id": str(uuid.uuid4()),
        }
        req = Request(url, data=request, headers=headers, method="POST")
        try:
            with urlopen(req, timeout=timeout) as response:
                body = response.read(4 * 1024 * 1024 + 1)
                if len(body) > 4 * 1024 * 1024:
                    raise SandToolError("Grok Bot 后端响应过大")
                return body
        except HTTPError as exc:
            detail = ""
            try:
                detail = exc.read(4096).decode("utf-8", errors="replace")
            finally:
                exc.close()
            # 503/429/500 are transient (the box is still starting or the backend
            # is briefly unavailable) and the server marks them isRetryable.
            retryable = exc.code in (429, 500, 502, 503, 504)
            if retryable and attempt < retries:
                attempt += 1
                time.sleep(min(2 ** attempt, 15))
                continue
            hint = ""
            if exc.code in (429, 500, 502, 503, 504):
                hint = "（服务暂时不可用，可稍后重试；请确认 Box 已启动）"
            raise SandToolError(
                f"Grok Bot {method_name} 失败：HTTP {exc.code}{hint} {detail[:300]}"
            ) from exc
        except (URLError, TimeoutError, OSError) as exc:
            if attempt < retries:
                attempt += 1
                time.sleep(min(2 ** attempt, 15))
                continue
            raise SandToolError(
                f"无法连接 Grok Bot 后端（{method_name}）"
            ) from exc


def _grok_ensure_box_running(
    wake: bool = True,
    wait_seconds: float = 150,
    log: Optional[Callable[[str], None]] = None,
) -> int:
    # Poll EnsureSandBox until the Box reports RUNNING (SandBoxRunState=3); a
    # STARTING/HIBERNATED box would otherwise make the first SendGrokBotUserMessage
    # return HTTP 503 unavailable.
    deadline = time.monotonic() + wait_seconds
    run_state = 0
    while True:
        request = _pb_bool(2, True) if wake else b""
        response = _grok_rpc("EnsureSandBox", request)
        run_state = 0
        for field, wire, value in _pb_iter_fields(response):
            if field == 13 and wire == 0:
                run_state = value
        if run_state == 3:  # SAND_BOX_RUN_STATE_RUNNING
            return run_state
        if time.monotonic() >= deadline:
            return run_state
        if log is not None:
            label = {1: "ABSENT", 2: "HIBERNATED", 4: "STARTING"}.get(
                run_state, str(run_state)
            )
            log(f"Box 尚未 RUNNING（状态 {label}），等待启动 ...")
        time.sleep(4)


def _grok_list_box_agents() -> List[Tuple[str, str, int]]:
    # (agentId, name, updatedAtMs) for agents whose harness scalar == "box".
    # SendGrokBotUserMessage routes by the agent_id field (no.12, a UUID), not
    # the display id field (no.1, which can be a short numeric); prefer field 12.
    response = _grok_rpc("ListGrokBotAgents", b"")
    agents: List[Tuple[str, str, int]] = []
    for field, wire, value in _pb_iter_fields(response):
        if field != 1 or wire != 2:
            continue
        id_field = ""
        agent_id_field = ""
        name = ""
        harness = ""
        updated = 0
        for afield, awire, avalue in _pb_iter_fields(value):
            if afield == 1 and awire == 2:
                id_field = avalue.decode("utf-8", errors="replace")
            elif afield == 12 and awire == 2:
                agent_id_field = avalue.decode("utf-8", errors="replace")
            elif afield == 3 and awire == 2:
                name = avalue.decode("utf-8", errors="replace")
            elif afield == 13 and awire == 2:
                harness = avalue.decode("utf-8", errors="replace")
            elif afield == 11 and awire == 0:
                updated = avalue
        routable = agent_id_field or id_field
        if routable and harness.casefold() == "box":
            agents.append((routable, name, updated))
    agents.sort(key=lambda item: item[2], reverse=True)
    return agents


def _grok_create_box_agent() -> str:
    import uuid

    agent_id = str(uuid.uuid4())
    request = (
        _pb_str(2, "Cursor Sand Relay")
        + _pb_str(3, "Auto-created to host the Cursor Sand Stream relay.")
        + _pb_str(4, "Cursor Sand Relay")
        + _pb_str(5, "circle")
        + _pb_str(6, "#5B8CFF")
        + _pb_str(8, agent_id)
        + _pb_uint(9, 1)  # GrokBotAgentHarnessKind.BOX
        + _pb_bool(11, True)  # introduction_suppressed
    )
    response = _grok_rpc("CreateGrokBotAgent", request)
    for field, wire, value in _pb_iter_fields(response):
        if field == 1 and wire == 2:
            created_id = ""
            created_agent_id = ""
            for afield, awire, avalue in _pb_iter_fields(value):
                if afield == 1 and awire == 2:
                    created_id = avalue.decode("utf-8", errors="replace")
                elif afield == 12 and awire == 2:
                    created_agent_id = avalue.decode("utf-8", errors="replace")
            routable = created_agent_id or created_id
            if routable:
                return routable
    # We set field 8 (agent_id) to our uuid in the request, so it is the
    # routable id even if the response did not echo it.
    return agent_id


def _grok_report_presence(agent_id: str, viewing: bool = True) -> None:
    # Mirror the desktop client "opening" an agent: reporting presence wakes /
    # instantiates the agent's session in the Box so that SendGrokBotUserMessage
    # (backend) and /api/sendPrompt (gateway) stop returning 503/500.
    request = (
        _pb_str(1, agent_id)
        + _pb_bool(2, viewing)
        + _pb_str(3, "desktop")
    )
    _grok_rpc("ReportGrokBotClientPresence", request)


def _grok_send_message(agent_id: str, text: str) -> None:
    import uuid

    message_id = str(uuid.uuid4())
    request = (
        _pb_str(1, agent_id)
        + _pb_str(2, message_id)
        + _pb_str(3, text)
        + _pb_uint(4, int(time.time() * 1000))  # sent_at_ms (int64 as varint)
        + _pb_uint(13, 1)  # GrokBotClientSurface.DESKTOP
    )
    response = _grok_rpc("SendGrokBotUserMessage", request)
    dispatched = False
    refusal = False
    for field, wire, value in _pb_iter_fields(response):
        if field == 1 and wire == 0:
            dispatched = bool(value)
        elif field == 5 and wire == 2:
            refusal = True
    if refusal:
        raise SandToolError("Grok Bot 拒绝了初始化指令（harness refusal）")
    if not dispatched:
        raise SandToolError("Grok Bot 未接受初始化指令")


def _sand_box_state_label(run_state: int) -> str:
    return {
        0: "UNSPECIFIED",
        1: "ABSENT",
        2: "HIBERNATED",
        3: "RUNNING",
        4: "STARTING",
    }.get(run_state, str(run_state))


def _grok_pick_or_create_box_agent(
    log: Optional[Callable[[str], None]] = None,
) -> str:
    run_state = _grok_ensure_box_running(wake=True, log=log)
    if log is not None:
        log(f"Box 运行状态：{_sand_box_state_label(run_state)}")
    if run_state != 3:
        raise SandToolError(
            f"当前 Cursor 账号的 Box 未进入 RUNNING（状态 "
            f"{_sand_box_state_label(run_state)}）。可能：该账号没有可用的 "
            "Grok Bot Box、无 Grok 资格、或启动超时。请在 Grok Bot/网页端确认该"
            "账号能正常启动 Box 后重试；确属超时可再跑一次。"
        )
    agents = _grok_list_box_agents()
    if log is not None:
        log(f"发现 {len(agents)} 个 Box Agent")
    if agents:
        return agents[0][0]
    if log is not None:
        log("无现有 Box Agent，创建一个新的 ...")
    return _grok_create_box_agent()


def _print_box_provision_manual_hint() -> None:
    # Clear any active spinner frame first (menu path runs under LoadingSpinner),
    # then print the exact instruction the user can paste into the current Box.
    print("\r" + " " * 60 + "\r", end="")
    print_warn(
        "无法自动初始化当前 Box。请在 Grok Bot 中打开【当前登录账号】的目标 Box Agent，"
        "把下面这段指令原样发给它；待它改完并重启 host 后，重新运行本项检查（菜单 2）。"
    )
    print(colorize("----- 手动初始化指令（复制以下全部内容）-----", ANSI_BOLD))
    print(colorize(BOX_RELAY_PROVISION_PROMPT, ANSI_BLUE))
    print(colorize("----- 指令结束 -----", ANSI_BOLD))


def _provision_state_path() -> Path:
    return _config_dir() / "box-provision-state.json"


def _load_provision_state() -> Dict[str, object]:
    try:
        value = json.loads(_provision_state_path().read_text(encoding="utf-8"))
        return value if isinstance(value, dict) else {}
    except Exception:
        return {}


def _save_provision_state(state: Mapping[str, object]) -> None:
    try:
        _write_json_atomic(_provision_state_path(), dict(state))
    except Exception:
        pass


def provision_box_relay(verbose: bool = False, wait_seconds: float = 240) -> str:
    def log(message: str) -> None:
        if verbose:
            print("\r" + " " * 60 + "\r", end="")
            print(colorize("  · " + message, ANSI_BLUE), flush=True)

    # Everything is keyed to the Cursor state.vscdb account: EnsureSandBox yields
    # that account's Box gateway, so the relay always targets the account Cursor
    # runs as.  The Grok Bot desktop client and its local files are not read.
    try:
        cursor_token = _cursor_state_value("cursorAuth/accessToken")
        log(f"Cursor 登录账号：{_account_label_from_token(cursor_token)}")
    except SandToolError as exc:
        log(f"读取 Cursor 登录账号失败：{exc}")

    log("通过 EnsureSandBox 获取当前 Cursor 账号的 Box gateway ...")
    try:
        config: Optional[Mapping[str, object]] = _load_grok_relay_config(
            refresh=True
        )
    except SandToolError as exc:
        config = None
        log(f"Box 尚未就绪（{exc}），将先触发初始化再等待")

    if config is not None:
        log("探测 Box relay 路由 ...")
        status, content_type, relay_ok, relay_diagnosis = _probe_box_relay(config)
        log(
            f"relay 探测：HTTP {status}，Content-Type={content_type or 'unknown'}，"
            f"{relay_diagnosis}"
        )
        if relay_ok:
            return "already-installed"
        if status in (401, 403):
            raise SandToolError(
                "Box gateway 拒绝鉴权（token 可能过期）；请确认 Cursor 已登录"
                "且账号有 Grok Bot 资格后重试"
            )
        if status == 200:
            log("检测到旧版或不完整 relay，继续原地升级")
        elif status != 404:
            raise SandToolError(
                "无法确认 Box relay 状态："
                f"HTTP {status}，Content-Type={content_type or 'unknown'}，"
                f"{relay_diagnosis}"
            )

    # Reuse a previously created relay agent for this account: re-runs must not
    # spawn a new agent nor re-burn tokens.  Create + send the instruction only
    # once; afterwards we just keep polling until the relay route answers 200.
    account_fp = str(config.get("accountFingerprint") or "") if config else ""
    state = _load_provision_state()
    entry = state.get(account_fp) if isinstance(state.get(account_fp), dict) else {}
    agent_id = entry.get("relayAgentId") if isinstance(entry.get("relayAgentId"), str) else ""
    last_sent_ms = entry.get("lastSentMs") if isinstance(entry.get("lastSentMs"), (int, float)) else 0
    try:
        log("确认 Box 处于 RUNNING ...")
        run_state = _grok_ensure_box_running(wake=True, log=log)
        log(f"Box 运行状态：{_sand_box_state_label(run_state)}")
        if run_state != 3:
            raise SandToolError(
                f"当前 Cursor 账号的 Box 未进入 RUNNING（状态 "
                f"{_sand_box_state_label(run_state)}）；该账号可能没有可用的 Grok "
                "Bot Box，或启动超时。请确认账号有 Grok 资格后重试。"
            )
        if config is None:
            config = _load_grok_relay_config(refresh=True)
            account_fp = str(config.get("accountFingerprint") or "")

        need_send = False
        if not agent_id:
            # Create the agent INSIDE the Box: backend-listed agents are unknown
            # to the Box coordinator ("Sand agent ... does not exist"), so we use
            # the Box gateway's own createAgent, then send to that box-local id.
            log("在 Box 内创建 relay agent（网关 /api/createAgent）...")
            agent_id = _gateway_create_box_agent(config)
            log(f"Box 内 relay agent：{agent_id}")
            need_send = True
        else:
            log(f"复用已创建的 relay agent：{agent_id}")
            # Re-nudge only if it has been idle for a while (the box agent may
            # have finished/stalled); otherwise just keep polling, no re-burn.
            if (
                entry.get("routeVersion") != BOX_RELAY_ROUTE_VERSION
                or (time.time() * 1000 - last_sent_ms) > 900000
            ):
                need_send = True

        if need_send:
            try:
                _send_box_provision_prompt(config, agent_id)
            except SandToolError as send_exc:
                if "does not exist" in str(send_exc):
                    log("原 relay agent 已不在 Box 内，重新创建 ...")
                    agent_id = _gateway_create_box_agent(config)
                    log(f"Box 内 relay agent：{agent_id}")
                    _send_box_provision_prompt(config, agent_id)
                else:
                    raise
            last_sent_ms = int(time.time() * 1000)
            log("指令已受理，Box 正在改写 host-main.cjs 并重启（可能数分钟）...")
        else:
            log("relay agent 已在处理中，直接轮询等待其完成 ...")

        state[account_fp] = {
            "relayAgentId": agent_id,
            "lastSentMs": last_sent_ms,
            "routeVersion": BOX_RELAY_ROUTE_VERSION,
        }
        _save_provision_state(state)
    except SandToolError:
        _print_box_provision_manual_hint()
        raise

    deadline = time.monotonic() + wait_seconds
    started = time.monotonic()
    last_status = 404
    while time.monotonic() < deadline:
        time.sleep(5)
        if config is None:
            try:
                config = _load_grok_relay_config(refresh=True)
            except SandToolError:
                log("Box gateway 尚未就绪，继续等待 ...")
                continue
        try:
            last_status, content_type, relay_ok, relay_diagnosis = _probe_box_relay(
                config
            )
        except SandToolError:
            continue
        log(
            f"等待中（{int(time.monotonic() - started)}s）relay 探测："
            f"HTTP {last_status}，{relay_diagnosis}"
            "（Ctrl+C 可随时中断，重跑会复用同一 agent 继续等）"
        )
        if relay_ok:
            return "installed"
        if last_status in (401, 403):
            raise SandToolError(
                "Box 初始化期间网关拒绝鉴权（token 可能过期），请确认 Cursor "
                "登录有效后重试"
            )
    # Not up yet, but the Box agent keeps working in the background.  Do not
    # re-create/re-send on the next run — just re-poll.
    return "provisioning"


def provision_and_install() -> int:
    # One-click: register the Box relay route, then inject Cursor.  Verbose,
    # no spinner, so each step is visible in the log.
    def step(message: str) -> None:
        print(colorize("[步骤] " + message, ANSI_BOLD, ANSI_BLUE), flush=True)

    step("解析 Cursor 安装路径 ...")
    layout = resolve_cursor_layout()
    step(f"目标 Cursor {layout.version}  |  {layout.install_root}")
    if not _cursor_version_supported(layout.version):
        raise SandToolError(
            f"当前 Cursor 版本为 {layout.version}，本工具仅适配 "
            f"{SUPPORTED_CURSOR_VERSION_LABEL}；请更换后再运行一键流程"
        )

    step("第 1/2 步：检查并初始化当前 Grok Bot 远程 Box relay（详细日志如下）")
    relay_result = provision_box_relay(verbose=True)
    if relay_result == "already-installed":
        print_success("  ✓ Box relay 已存在，无需重复写入")
    elif relay_result == "installed":
        print_success("  ✓ Box relay 已初始化并通过路由探测")
    else:  # "provisioning"
        print_warn(
            "  … Box 仍在后台改写 host 并重启（未等到 200）。已注入 Cursor；"
            "几分钟后重跑菜单 3 确认即可（会复用同一 agent，不重复发送/扣费）。"
        )

    step("第 2/2 步：向 Cursor 注入 Grok Bot Box Relay Stream 补丁 ...")
    install(layout)
    print_success("  ✓ " + _restart_outcome("注入完成"))

    if relay_result == "provisioning":
        print_success("✓ 一键流程完成：Cursor 已注入；Box relay 后台进行中")
        print_warn(
            "Box relay 就绪前 Cursor 出模型会报 404，属正常；就绪后自动生效。"
            "过几分钟用菜单 3 确认是否已 200。"
        )
    else:
        print_success("✓ 一键流程完成：Box relay 已就绪 + Cursor 已注入")
        print_warn("请在 Cursor 中出一次模型验证。")
    return 0


def _is_within(path: Path, root: Path) -> bool:
    try:
        path.relative_to(root)
        return True
    except ValueError:
        return False


def _path_key(path: Path) -> str:
    normalized = str(path.resolve())
    return os.path.normcase(normalized)


def _sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _product_checksum(data: bytes) -> str:
    digest = hashlib.sha256(data).digest()
    return base64.b64encode(digest).decode("ascii").rstrip("=")


def _atomic_write(path: Path, data: bytes, mode: Optional[int] = None) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temp = path.parent / (
        f".{path.name}.sand-client-{os.getpid()}-{time.time_ns()}.tmp"
    )
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    fd: Optional[int] = None
    try:
        fd = os.open(str(temp), flags, 0o600)
        with os.fdopen(fd, "wb", closefd=True) as handle:
            fd = None
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        if mode is not None:
            os.chmod(temp, stat.S_IMODE(mode))
        try:
            os.replace(temp, path)
        except PermissionError:
            original_mode: Optional[int] = None
            if path.exists():
                original_mode = stat.S_IMODE(path.stat().st_mode)
                os.chmod(path, original_mode | stat.S_IWRITE)
            try:
                os.replace(temp, path)
            except BaseException:
                if original_mode is not None and path.exists():
                    try:
                        os.chmod(path, original_mode)
                    except OSError:
                        pass
                raise
        if mode is not None:
            os.chmod(path, stat.S_IMODE(mode))
    finally:
        if fd is not None:
            os.close(fd)
        try:
            if temp.exists():
                temp.unlink()
        except OSError:
            pass


def _write_json_atomic(path: Path, value: Mapping[str, object]) -> None:
    data = (json.dumps(value, ensure_ascii=False, indent=2) + "\n").encode("utf-8")
    _atomic_write(path, data, 0o600)


def _load_config() -> Mapping[str, object]:
    path = _config_path()
    if not path.exists():
        return {}
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except Exception as exc:
        raise SandToolError(
            f"配置文件损坏：{path}\n请运行 set-path auto 后重新检测"
        ) from exc
    if not isinstance(value, dict) or value.get("version") != CONFIG_VERSION:
        raise SandToolError(
            f"不支持的配置文件：{path}\n请运行 set-path auto 后重新检测"
        )
    return value


def _read_product(product_path: Path) -> Mapping[str, object]:
    try:
        size = product_path.stat().st_size
        if size <= 0 or size > 1024 * 1024:
            raise SandToolError(f"product.json 大小异常：{product_path}")
        raw = product_path.read_bytes()
        value = json.loads(raw.decode("utf-8-sig"))
    except SandToolError:
        raise
    except Exception as exc:
        raise SandToolError(f"无法读取 Cursor product.json：{product_path}") from exc
    if not isinstance(value, dict):
        raise SandToolError(f"Cursor product.json 格式错误：{product_path}")
    name = str(value.get("applicationName") or value.get("nameShort") or "")
    if name.casefold() != "cursor":
        raise SandToolError(f"所选目录不是 Cursor 安装：{product_path}")
    return value


def _find_app_bundle(app_root: Path) -> Optional[Path]:
    for item in (app_root, *app_root.parents):
        if item.suffix.casefold() == ".app":
            return item
    return None


def _candidate_app_roots(raw_path: Path) -> Iterable[Path]:
    path = raw_path
    if path.is_file():
        if path.name.casefold() == "product.json":
            path = path.parent
        else:
            path = path.parent
    current = path
    for _ in range(8):
        yield current
        yield current / "resources" / "app"
        yield current / "Resources" / "app"
        yield current / "Contents" / "Resources" / "app"
        if current.parent == current:
            break
        current = current.parent


def _resolve_executable(app_root: Path) -> Tuple[Path, Path]:
    if sys.platform == "win32":
        if app_root.parent.name.casefold() == "resources":
            install_root = app_root.parent.parent
        else:
            install_root = app_root
        candidates = (
            install_root / "Cursor.exe",
            install_root / "cursor.exe",
        )
    elif sys.platform == "darwin":
        bundle = _find_app_bundle(app_root)
        if bundle is None:
            raise SandToolError("macOS Cursor 路径必须位于 Cursor.app 内")
        install_root = bundle
        candidates = (bundle / "Contents" / "MacOS" / "Cursor",)
    else:
        raise SandToolError("当前仅支持 Windows 和 macOS")

    for executable in candidates:
        try:
            resolved = executable.resolve(strict=True)
        except (FileNotFoundError, OSError):
            continue
        if resolved.is_file() and _is_within(resolved, install_root.resolve()):
            return install_root.resolve(), resolved
    raise SandToolError(f"未找到 Cursor 可执行文件：{install_root}")


def layout_from_path(value: Union[str, Path]) -> CursorLayout:
    raw_text = str(value).strip().strip('"')
    if not raw_text:
        raise SandToolError("Cursor 路径不能为空")
    if sys.platform == "win32" and (
        raw_text.startswith("\\\\") or raw_text.startswith("\\\\?\\")
    ):
        raise SandToolError("不支持 UNC 或 Windows 设备路径")

    raw = Path(raw_text).expanduser()
    if not raw.is_absolute():
        raise SandToolError(f"Cursor 路径必须是绝对路径：{raw}")
    try:
        raw = raw.resolve(strict=True)
    except (FileNotFoundError, OSError) as exc:
        raise SandToolError(f"Cursor 路径不存在：{raw}") from exc

    seen: Set[str] = set()
    last_error: Optional[Exception] = None
    for candidate in _candidate_app_roots(raw):
        try:
            app_root = candidate.resolve(strict=True)
        except (FileNotFoundError, OSError):
            continue
        key = _path_key(app_root)
        if key in seen:
            continue
        seen.add(key)

        product_json = app_root / "product.json"
        if not product_json.is_file():
            continue
        try:
            product_real = product_json.resolve(strict=True)
            if not _is_within(product_real, app_root):
                raise SandToolError("product.json 符号链接逃逸出 Cursor app 目录")
            product = _read_product(product_real)
            install_root, executable = _resolve_executable(app_root)

            targets: List[Path] = []
            for rel, _extension_name in TARGET_SPECS:
                target = app_root.joinpath(*rel.split("/"))
                if not target.is_file():
                    continue
                target_real = target.resolve(strict=True)
                if not _is_within(target_real, app_root):
                    raise SandToolError(f"目标文件符号链接逃逸：{target}")
                targets.append(target_real)
            if not targets:
                raise SandToolError(
                    "Cursor 使用 app.asar 或当前版本没有可识别的 Sand 目标文件"
                )

            ext_host = app_root.joinpath(*EXT_HOST_REL.split("/"))
            ext_host_real = ext_host.resolve(strict=True) if ext_host.is_file() else None
            version = str(product.get("version") or product.get("commit") or "未知")
            return CursorLayout(
                install_root=install_root,
                app_root=app_root,
                product_json=product_real,
                executable=executable,
                target_paths=tuple(targets),
                ext_host_path=ext_host_real,
                version=version,
            )
        except SandToolError as exc:
            last_error = exc
            continue

    if last_error:
        raise SandToolError(f"Cursor 路径校验失败：{last_error}") from last_error
    raise SandToolError(f"路径中未找到 Cursor resources/app：{raw}")


def _powershell_executable() -> Optional[str]:
    return shutil.which("powershell.exe") or shutil.which("powershell") or shutil.which("pwsh")


def _windows_running_candidates() -> List[str]:
    powershell = _powershell_executable()
    if not powershell:
        return []
    script = (
        "$ErrorActionPreference='SilentlyContinue';"
        "[Console]::OutputEncoding=[System.Text.UTF8Encoding]::new();"
        "Get-CimInstance Win32_Process -Filter \"Name='Cursor.exe'\" | "
        "ForEach-Object { if ($_.ExecutablePath) { $_.ExecutablePath } }"
    )
    try:
        result = subprocess.run(
            [powershell, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script],
            capture_output=True,
            text=True,
            encoding="utf-8",
            errors="replace",
            timeout=15,
            check=False,
        )
    except (OSError, subprocess.TimeoutExpired):
        return []
    if result.returncode != 0:
        return []
    return [line.strip() for line in result.stdout.splitlines() if line.strip()]


def _windows_registry_candidates() -> List[str]:
    if sys.platform != "win32":
        return []
    try:
        import winreg
    except ImportError:
        return []

    candidates: List[str] = []
    roots = (winreg.HKEY_CURRENT_USER, winreg.HKEY_LOCAL_MACHINE)
    views = (winreg.KEY_WOW64_64KEY, winreg.KEY_WOW64_32KEY)
    uninstall = r"Software\Microsoft\Windows\CurrentVersion\Uninstall"
    for root in roots:
        for view in views:
            try:
                parent = winreg.OpenKey(root, uninstall, 0, winreg.KEY_READ | view)
            except OSError:
                continue
            with parent:
                index = 0
                while True:
                    try:
                        name = winreg.EnumKey(parent, index)
                    except OSError:
                        break
                    index += 1
                    try:
                        child = winreg.OpenKey(parent, name)
                    except OSError:
                        continue
                    with child:
                        def read(name_: str) -> str:
                            try:
                                return str(winreg.QueryValueEx(child, name_)[0] or "")
                            except OSError:
                                return ""

                        display_name = read("DisplayName").strip()
                        publisher = read("Publisher").strip()
                        if display_name.casefold() != "cursor" and "anysphere" not in publisher.casefold():
                            continue
                        install_location = read("InstallLocation").strip().strip('"')
                        display_icon = read("DisplayIcon").strip().strip('"')
                        if install_location:
                            candidates.append(install_location)
                        if display_icon:
                            icon_path = re.sub(r",\s*-?\d+$", "", display_icon).strip('"')
                            candidates.append(icon_path)
    return candidates


def _mac_process_paths(strict: bool = False) -> List[Tuple[int, Path]]:
    try:
        libproc = ctypes.CDLL("/usr/lib/libproc.dylib")
        proc_pidpath = libproc.proc_pidpath
        proc_pidpath.argtypes = [ctypes.c_int, ctypes.c_void_p, ctypes.c_uint32]
        proc_pidpath.restype = ctypes.c_int
        result = subprocess.run(
            ["ps", "-axo", "pid="],
            capture_output=True,
            text=True,
            encoding="utf-8",
            errors="replace",
            timeout=10,
            check=False,
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        if strict:
            raise SandToolError("无法读取 macOS 进程可执行路径") from exc
        return []
    if result.returncode != 0:
        if strict:
            raise SandToolError("无法读取 macOS 进程可执行路径")
        return []
    values: List[Tuple[int, Path]] = []
    for line in result.stdout.splitlines():
        try:
            pid = int(line.strip())
        except ValueError:
            continue
        buffer = ctypes.create_string_buffer(4096)
        length = proc_pidpath(pid, buffer, len(buffer))
        if length <= 0:
            continue
        try:
            executable = Path(os.fsdecode(buffer.value)).resolve(strict=False)
        except (OSError, ValueError):
            continue
        values.append((pid, executable))
    return values


def _bundle_for_executable(executable: Path) -> Optional[Path]:
    for item in (executable, *executable.parents):
        if item.name.casefold() == "cursor.app":
            return item
    return None


def _mac_running_candidates() -> List[str]:
    values: Dict[str, str] = {}
    for _pid, executable in _mac_process_paths():
        bundle = _bundle_for_executable(executable)
        if bundle is not None:
            values.setdefault(_path_key(bundle), str(bundle))
    return list(values.values())


def _mac_spotlight_candidates() -> List[str]:
    mdfind = shutil.which("mdfind")
    if not mdfind:
        return []
    try:
        result = subprocess.run(
            [
                mdfind,
                "kMDItemCFBundleIdentifier == 'com.todesktop.230313mzl4w4u92'",
            ],
            capture_output=True,
            text=True,
            encoding="utf-8",
            errors="replace",
            timeout=15,
            check=False,
        )
    except (OSError, subprocess.TimeoutExpired):
        return []
    if result.returncode != 0:
        return []
    return [line.strip() for line in result.stdout.splitlines() if line.strip()]


def _default_candidate_groups() -> Iterable[Tuple[str, Sequence[str]]]:
    env_candidate = os.environ.get("SAND_CURSOR_INSTALL_DIR", "").strip()
    if env_candidate:
        yield "环境变量 SAND_CURSOR_INSTALL_DIR", (env_candidate,)

    if sys.platform == "win32":
        yield "运行中的 Cursor", _windows_running_candidates()
        yield "Windows 安装登记", _windows_registry_candidates()
        local = os.environ.get("LOCALAPPDATA", "")
        program_files = os.environ.get("ProgramFiles", r"C:\Program Files")
        program_files_x86 = os.environ.get("ProgramFiles(x86)", "")
        defaults = [
            str(Path(local) / "Programs" / "Cursor") if local else "",
            str(Path(local) / "Programs" / "cursor") if local else "",
            str(Path(local) / "Cursor") if local else "",
            str(Path(program_files) / "Cursor"),
            str(Path(program_files_x86) / "Cursor") if program_files_x86 else "",
        ]
        yield "Windows 默认目录", tuple(x for x in defaults if x)
    elif sys.platform == "darwin":
        yield "运行中的 Cursor", _mac_running_candidates()
        yield "macOS Spotlight", _mac_spotlight_candidates()
        yield "macOS 默认目录", (
            "/Applications/Cursor.app",
            str(Path.home() / "Applications" / "Cursor.app"),
        )

    path_cursor = shutil.which("cursor")
    if path_cursor:
        yield "PATH", (path_cursor,)


def _valid_layouts(values: Sequence[str]) -> List[CursorLayout]:
    layouts: Dict[str, CursorLayout] = {}
    for value in values:
        if not value:
            continue
        try:
            layout = layout_from_path(value)
        except SandToolError:
            continue
        layouts.setdefault(_path_key(layout.app_root), layout)
    return list(layouts.values())


def resolve_cursor_layout() -> CursorLayout:
    explicit = os.environ.get("SAND_CURSOR_INSTALL_DIR", "").strip()
    if explicit:
        return layout_from_path(explicit)

    configured = _load_config().get("cursorInstallRoot")
    if isinstance(configured, str) and configured.strip():
        try:
            return layout_from_path(configured)
        except SandToolError as exc:
            raise SandToolError(
                f"已设置的 Cursor 路径失效：{configured}\n"
                "请运行 set-path <新路径>，或运行 set-path auto 恢复自动检测"
            ) from exc

    for source, values in _default_candidate_groups():
        layouts = _valid_layouts(tuple(values))
        if len(layouts) == 1:
            return layouts[0]
        if len(layouts) > 1:
            options = "\n".join(f"  - {item.install_root}" for item in layouts)
            raise SandToolError(
                f"{source}检测到多个 Cursor 安装，请先在菜单中选择 3 设置路径：\n{options}"
            )
    raise SandToolError(
        "未检测到 Cursor 安装，请在菜单中选择 3 设置 Cursor 路径"
        "（Cursor.exe、Cursor.app 或 resources/app）"
    )


def save_cursor_path(value: str) -> Optional[CursorLayout]:
    if value.strip().casefold() in {"auto", "clear", "reset"}:
        _write_json_atomic(
            _config_path(),
            {
                "version": CONFIG_VERSION,
                "cursorInstallRoot": "",
                "lastVerifiedVersion": "",
                "updatedAt": datetime.now(timezone.utc).isoformat(),
            },
        )
        return None

    layout = layout_from_path(value)
    _write_json_atomic(
        _config_path(),
        {
            "version": CONFIG_VERSION,
            "cursorInstallRoot": str(layout.install_root),
            "lastVerifiedVersion": layout.version,
            "updatedAt": datetime.now(timezone.utc).isoformat(),
        },
    )
    return layout


def apply_patch_to_content(content: str) -> Tuple[str, PatchStats]:
    stats = PatchStats()
    next_content = content
    legacy_client_re = re.compile(
        rf"([\"'])sand\1{LEGACY_CLIENT_MARKER_PATTERN}"
    )
    next_content, stats.migrated_client = legacy_client_re.subn(
        lambda match: match.group(1)
        + "sand"
        + match.group(1)
        + SAND_CLIENT_MARKER,
        next_content,
    )
    legacy_eligibility = "return!1;" + LEGACY_SAND_ELIGIBILITY_MARKER
    stats.migrated_eligibility = next_content.count(legacy_eligibility)
    next_content = next_content.replace(
        legacy_eligibility,
        "return!1;" + SAND_ELIGIBILITY_MARKER,
    )
    for key, rule in CLIENT_RULES:
        def replace_client(match: re.Match[str], stat_key: str = key) -> str:
            current = match.group(3)
            setattr(stats, stat_key, getattr(stats, stat_key) + 1)
            if current == "sand":
                stats.adopted_sand += 1
                marker = SAND_CLIENT_EXISTING_MARKER
            else:
                marker = SAND_CLIENT_MARKER
            return (
                match.group(1)
                + match.group(2)
                + "sand"
                + match.group(2)
                + marker
            )

        next_content = rule.sub(replace_client, next_content)

    def replace_glass_client(match: re.Match[str]) -> str:
        return (
            match.group(1)
            + match.group(2)
            + "sand"
            + match.group(2)
            + SAND_GLASS_CLIENT_MARKER
            + match.group(3)
            + match.group(4)
            + "sand"
            + match.group(4)
        )

    next_content, glass_client_count = GLASS_CLIENT_RE.subn(
        replace_glass_client,
        next_content,
    )
    stats.glass_client += glass_client_count

    for prefix in ELIGIBILITY_PREFIXES:
        count = next_content.count(prefix)
        if count == 0:
            continue
        patched = prefix.replace(
            "{const{adminSettingsService:",
            "{return!1;" + SAND_ELIGIBILITY_MARKER + "const{adminSettingsService:",
        )
        next_content = next_content.replace(prefix, patched)
        stats.eligibility += count

    route_count = next_content.count(MANAGED_LOCAL_ROUTE_ORIGINAL)
    if route_count:
        next_content = next_content.replace(
            MANAGED_LOCAL_ROUTE_ORIGINAL,
            MANAGED_LOCAL_ROUTE_PATCHED,
        )
        stats.managed_local_route += route_count

    runtime_load_count = next_content.count(LOCAL_RUNTIME_LOAD_ORIGINAL)
    if runtime_load_count:
        next_content = next_content.replace(
            LOCAL_RUNTIME_LOAD_ORIGINAL,
            LOCAL_RUNTIME_LOAD_PATCHED,
        )
        stats.local_runtime_load += runtime_load_count

    legacy_grok_runtime_auth_count = next_content.count(
        GROK_RUNTIME_AUTH_PATCHED_V132
    )
    if legacy_grok_runtime_auth_count:
        next_content = next_content.replace(
            GROK_RUNTIME_AUTH_PATCHED_V132,
            GROK_RUNTIME_AUTH_PATCHED,
        )
        stats.grok_runtime_auth += legacy_grok_runtime_auth_count

    v133a_grok_runtime_auth_count = next_content.count(
        GROK_RUNTIME_AUTH_PATCHED_V133A
    )
    if v133a_grok_runtime_auth_count:
        next_content = next_content.replace(
            GROK_RUNTIME_AUTH_PATCHED_V133A,
            GROK_RUNTIME_AUTH_PATCHED,
        )
        stats.grok_runtime_auth += v133a_grok_runtime_auth_count

    # Migrate every owned relay generation in place.  Same var-order pairing,
    # same two transport sites; only the relay body changed between versions.
    for _auth_legacy_set in (
        GROK_RUNTIME_AUTH_PATCHES_V141,
        GROK_RUNTIME_AUTH_PATCHES_V144,
        GROK_RUNTIME_AUTH_PATCHES_V145,
        GROK_RUNTIME_AUTH_PATCHES_V147,
    ):
        for _auth_legacy, _auth_patched in zip(
            _auth_legacy_set, GROK_RUNTIME_AUTH_PATCHES_V3113
        ):
            if _auth_legacy == _auth_patched:
                continue
            legacy_auth_count = next_content.count(_auth_legacy)
            if legacy_auth_count:
                next_content = next_content.replace(_auth_legacy, _auth_patched)
                stats.grok_runtime_auth += legacy_auth_count

    for _auth_original, _auth_patched in zip(
        GROK_RUNTIME_AUTH_ORIGINALS_V3113, GROK_RUNTIME_AUTH_PATCHES_V3113
    ):
        grok_runtime_auth_count = next_content.count(_auth_original)
        if grok_runtime_auth_count:
            next_content = next_content.replace(_auth_original, _auth_patched)
            stats.grok_runtime_auth += grok_runtime_auth_count

    if SAND_GROK_RUNTIME_AUTH_MARKER in next_content:
        for retired_patched, retired_original in (
            (
                CONNECT_GZIP_FALLBACK_PATCHED_V150A,
                CONNECT_GZIP_FALLBACK_ORIGINAL,
            ),
            (CONNECT_GZIP_FALLBACK_PATCHED, CONNECT_GZIP_FALLBACK_ORIGINAL),
            (
                CONNECT_GZIP_METHOD_FALLBACK_PATCHED,
                CONNECT_GZIP_METHOD_FALLBACK_ORIGINAL,
            ),
        ):
            retired_count = next_content.count(retired_patched)
            if retired_count:
                next_content = next_content.replace(
                    retired_patched,
                    retired_original,
                )
                stats.migrated_connect_gzip_fallback += retired_count
        connect_gzip_fallback_count = next_content.count(
            CONNECT_GZIP_ENVELOPE_ORIGINAL
        )
        if connect_gzip_fallback_count:
            next_content = next_content.replace(
                CONNECT_GZIP_ENVELOPE_ORIGINAL,
                CONNECT_GZIP_ENVELOPE_PATCHED,
            )
            stats.connect_gzip_fallback += connect_gzip_fallback_count

    move_exec_count = next_content.count(AGENT_HOST_MOVE_EXEC_ORIGINAL)
    if move_exec_count:
        next_content = next_content.replace(
            AGENT_HOST_MOVE_EXEC_ORIGINAL,
            AGENT_HOST_MOVE_EXEC_PATCHED,
        )
        stats.agent_host_move_exec += move_exec_count

    managed_subagent_route_count = next_content.count(
        MANAGED_SUBAGENT_ROUTE_ORIGINAL
    )
    if managed_subagent_route_count:
        next_content = next_content.replace(
            MANAGED_SUBAGENT_ROUTE_ORIGINAL,
            MANAGED_SUBAGENT_ROUTE_PATCHED,
        )
        stats.managed_subagent_route += managed_subagent_route_count

    migrated_action_route_count = next_content.count(
        MANAGED_ACTION_ROUTE_PATCHED_V1
    )
    if migrated_action_route_count:
        next_content = next_content.replace(
            MANAGED_ACTION_ROUTE_PATCHED_V1,
            MANAGED_ACTION_ROUTE_PATCHED,
        )
        stats.migrated_action_route += migrated_action_route_count

    managed_action_route_count = next_content.count(MANAGED_ACTION_ROUTE_ORIGINAL)
    if managed_action_route_count:
        next_content = next_content.replace(
            MANAGED_ACTION_ROUTE_ORIGINAL,
            MANAGED_ACTION_ROUTE_PATCHED,
        )
        stats.managed_action_route += managed_action_route_count

    subagent_resume_mode_count = next_content.count(SUBAGENT_RESUME_MODE_ORIGINAL)
    if subagent_resume_mode_count:
        next_content = next_content.replace(
            SUBAGENT_RESUME_MODE_ORIGINAL,
            SUBAGENT_RESUME_MODE_PATCHED,
        )
        stats.subagent_resume_mode += subagent_resume_mode_count

    if SAND_SUBAGENT_COMPLETION_WAKE_MARKER not in next_content:
        def enable_subagent_completion_wake(match: re.Match[str]) -> str:
            variable = match.group(1)
            return (
                variable
                + '.source==="subagent"'
                + SAND_SUBAGENT_COMPLETION_WAKE_MARKER
                + "||"
                + match.group(0)
            )

        next_content, subagent_completion_wake_count = (
            SUBAGENT_COMPLETION_WAKE_RE.subn(
                enable_subagent_completion_wake,
                next_content,
            )
        )
        stats.subagent_completion_wake += subagent_completion_wake_count

    managed_subagent_session_count = next_content.count(
        MANAGED_SUBAGENT_SESSION_ORIGINAL
    )
    if managed_subagent_session_count:
        next_content = next_content.replace(
            MANAGED_SUBAGENT_SESSION_ORIGINAL,
            MANAGED_SUBAGENT_SESSION_PATCHED,
        )
        stats.managed_subagent_session += managed_subagent_session_count

    for previous_task_tool in (
        _managed_task_tool_patched_v126(),
        _managed_task_tool_patched_v125(),
        _managed_task_tool_patched_v124(),
    ):
        migrated_task_tool_count = next_content.count(previous_task_tool)
        if migrated_task_tool_count:
            next_content = next_content.replace(
                previous_task_tool,
                _managed_task_tool_patched(),
            )
            stats.migrated_task_tool += migrated_task_tool_count

    managed_task_tool_count = next_content.count(MANAGED_TASK_TOOL_ORIGINAL)
    if managed_task_tool_count:
        next_content = next_content.replace(
            MANAGED_TASK_TOOL_ORIGINAL,
            _managed_task_tool_patched(),
        )
        stats.managed_task_tool += managed_task_tool_count

    identity_count = next_content.count(AGENT_HOST_IDENTITY_ORIGINAL)
    if identity_count:
        next_content = next_content.replace(
            AGENT_HOST_IDENTITY_ORIGINAL,
            AGENT_HOST_IDENTITY_PATCHED,
        )
        stats.agent_host_identity += identity_count

    max_tokens_count = next_content.count(MAX_TOKENS_ORIGINAL)
    if max_tokens_count:
        next_content = next_content.replace(
            MAX_TOKENS_ORIGINAL,
            MAX_TOKENS_PATCHED,
        )
        stats.max_tokens += max_tokens_count

    # Grok Bot Box Relay Stream: remove the v1.2.7+ empty session marker that
    # re-enters InferenceService.RunInference.  Keep an already-current direct
    # injection; replace older direct variants through the marker-bounded
    # fallback, then inject immediately after the original hre() anchor.
    migrated_session_stream_count = next_content.count(
        SAND_SESSION_STREAM_MARKER
    )
    if migrated_session_stream_count:
        next_content = next_content.replace(SAND_SESSION_STREAM_MARKER, "")
        stats.migrated_session_stream += migrated_session_stream_count

    direct_injection = _direct_stream_injection()
    if (
        SAND_DIRECT_STREAM_MARKER in next_content
        and direct_injection not in next_content
    ):
        next_content, stripped_direct_stream_count = (
            DIRECT_STREAM_INJECTION_RE.subn("", next_content)
        )
        stats.migrated_direct_stream += stripped_direct_stream_count
    if (
        SAND_DIRECT_STREAM_MARKER not in next_content
        and DIRECT_STREAM_ANCHOR in next_content
    ):
        next_content = next_content.replace(
            DIRECT_STREAM_ANCHOR,
            DIRECT_STREAM_ANCHOR + direct_injection,
            1,
        )
        stats.direct_stream += 1

    rules_skills_count = next_content.count(RULES_SKILLS_EXEC_ORIGINAL)
    if rules_skills_count:
        next_content = next_content.replace(
            RULES_SKILLS_EXEC_ORIGINAL,
            RULES_SKILLS_EXEC_PATCHED,
        )
        stats.rules_skills += rules_skills_count

    mcp_filesystem_count = next_content.count(MCP_FILESYSTEM_ORIGINAL_V3113)
    if mcp_filesystem_count:
        next_content = next_content.replace(
            MCP_FILESYSTEM_ORIGINAL_V3113,
            MCP_FILESYSTEM_PATCHED_V3113,
        )
        stats.mcp_filesystem += mcp_filesystem_count

    def enable_user_rules(match: re.Match[str]) -> str:
        return (
            "injectLocalModeNonFileRules("
            + match.group("arg")
            + "){if(!1&&!"
            + match.group("flags")
            + ".localMode)return;"
            + SAND_USER_RULES_MARKER
        )

    next_content, user_rules_count = USER_RULES_ORIGINAL_RE.subn(
        enable_user_rules,
        next_content,
    )
    stats.user_rules += user_rules_count

    if SAND_AGENT_HOST_ENABLEMENT_MARKER not in next_content:
        def enable_agent_host(match: re.Match[str]) -> str:
            variable = match.group(2)
            return (
                variable
                + "=!0;"
                + SAND_AGENT_HOST_ENABLEMENT_MARKER
                + match.group(1)
                + variable
                + match.group(3)
            )

        next_content, agent_host_count = AGENT_HOST_ENABLEMENT_RE.subn(
            enable_agent_host,
            next_content,
            count=1,
        )
        stats.agent_host_enablement += agent_host_count
    return next_content, stats


def remove_patch_from_content(content: str) -> Tuple[str, RemoveStats]:
    stats = RemoveStats()
    legacy_client_re = re.compile(
        rf"([\"'])sand\1{LEGACY_CLIENT_MARKER_PATTERN}"
    )
    next_content, legacy_client_count = legacy_client_re.subn(
        lambda match: match.group(1) + "ide" + match.group(1),
        content,
    )
    stats.client_type += legacy_client_count
    legacy_eligibility = "return!1;" + LEGACY_SAND_ELIGIBILITY_MARKER
    legacy_eligibility_count = next_content.count(legacy_eligibility)
    next_content = next_content.replace(legacy_eligibility, "")
    stats.eligibility += legacy_eligibility_count
    # Restore the isGlass true branch first; the false branch is restored by
    # the regular "sand"+CLIENT_MODE marker -> "ide" pass below.
    next_content, glass_client_count = GLASS_CLIENT_RESTORE_RE.subn(
        lambda match: match.group(1) + match.group(2) + "glass" + match.group(2),
        next_content,
    )
    stats.glass_client += glass_client_count
    # The generic "sand"+CLIENT_MODE marker -> "ide" pass below also covers the
    # bare header.set("x-cursor-client-type","sand"/*SAND_CLIENT_MODE_V1*/)
    # sites patched by the set_header_bare rule: they go back to the bare
    # "ide" form (SandClaimer's uninstall residue), not to the original
    # <id>??"ide" -- that prefix was dropped by SandClaimer and cannot be
    # reconstructed here.  Reinstall Cursor 3.18.9 to get the original wording.
    client_re = re.compile(rf"([\"'])sand\1{CLIENT_MARKER_PATTERN}")
    existing_re = re.compile(
        rf"([\"'])sand\1{CLIENT_EXISTING_MARKER_PATTERN}"
    )

    def remove_client(match: re.Match[str]) -> str:
        stats.client_type += 1
        return match.group(1) + "ide" + match.group(1)

    next_content = client_re.sub(remove_client, next_content)
    next_content, existing_count = existing_re.subn(
        lambda match: match.group(1) + "sand" + match.group(1),
        next_content,
    )
    stats.client_type += existing_count
    eligibility_re = re.compile(rf"return!1;{ELIGIBILITY_MARKER_PATTERN}")
    next_content, eligibility_count = eligibility_re.subn("", next_content)
    stats.eligibility += eligibility_count

    route_count = next_content.count(MANAGED_LOCAL_ROUTE_PATCHED)
    if route_count:
        next_content = next_content.replace(
            MANAGED_LOCAL_ROUTE_PATCHED,
            MANAGED_LOCAL_ROUTE_ORIGINAL,
        )
        stats.managed_local_route += route_count

    runtime_load_count = next_content.count(LOCAL_RUNTIME_LOAD_PATCHED)
    if runtime_load_count:
        next_content = next_content.replace(
            LOCAL_RUNTIME_LOAD_PATCHED,
            LOCAL_RUNTIME_LOAD_ORIGINAL,
        )
        stats.local_runtime_load += runtime_load_count

    connect_gzip_fallback_count = next_content.count(
        CONNECT_GZIP_ENVELOPE_PATCHED
    )
    if connect_gzip_fallback_count:
        next_content = next_content.replace(
            CONNECT_GZIP_ENVELOPE_PATCHED,
            CONNECT_GZIP_ENVELOPE_ORIGINAL,
        )
        stats.connect_gzip_fallback += connect_gzip_fallback_count
    for retired_patched, retired_original in (
        (CONNECT_GZIP_FALLBACK_PATCHED_V150A, CONNECT_GZIP_FALLBACK_ORIGINAL),
        (CONNECT_GZIP_FALLBACK_PATCHED, CONNECT_GZIP_FALLBACK_ORIGINAL),
        (
            CONNECT_GZIP_METHOD_FALLBACK_PATCHED,
            CONNECT_GZIP_METHOD_FALLBACK_ORIGINAL,
        ),
    ):
        retired_count = next_content.count(retired_patched)
        if retired_count:
            next_content = next_content.replace(retired_patched, retired_original)
            stats.connect_gzip_fallback += retired_count

    legacy_grok_runtime_auth_count = next_content.count(
        GROK_RUNTIME_AUTH_PATCHED_V132
    )
    if legacy_grok_runtime_auth_count:
        next_content = next_content.replace(
            GROK_RUNTIME_AUTH_PATCHED_V132,
            GROK_RUNTIME_AUTH_ORIGINAL,
        )
        stats.grok_runtime_auth += legacy_grok_runtime_auth_count

    v133a_grok_runtime_auth_count = next_content.count(
        GROK_RUNTIME_AUTH_PATCHED_V133A
    )
    if v133a_grok_runtime_auth_count:
        next_content = next_content.replace(
            GROK_RUNTIME_AUTH_PATCHED_V133A,
            GROK_RUNTIME_AUTH_ORIGINAL,
        )
        stats.grok_runtime_auth += v133a_grok_runtime_auth_count

    for _auth_original, _auth_patched in zip(
        GROK_RUNTIME_AUTH_ORIGINALS_V3113, GROK_RUNTIME_AUTH_PATCHES_V3113
    ):
        grok_runtime_auth_count = next_content.count(_auth_patched)
        if grok_runtime_auth_count:
            next_content = next_content.replace(_auth_patched, _auth_original)
            stats.grok_runtime_auth += grok_runtime_auth_count

    # Also restore any older owned relay generation to the original.
    for _auth_legacy_set in (
        GROK_RUNTIME_AUTH_PATCHES_V141,
        GROK_RUNTIME_AUTH_PATCHES_V144,
        GROK_RUNTIME_AUTH_PATCHES_V145,
        GROK_RUNTIME_AUTH_PATCHES_V147,
    ):
        for _auth_original, _auth_legacy in zip(
            GROK_RUNTIME_AUTH_ORIGINALS_V3113, _auth_legacy_set
        ):
            if _auth_legacy == _auth_original:
                continue
            legacy_auth_count = next_content.count(_auth_legacy)
            if legacy_auth_count:
                next_content = next_content.replace(_auth_legacy, _auth_original)
                stats.grok_runtime_auth += legacy_auth_count

    move_exec_count = next_content.count(AGENT_HOST_MOVE_EXEC_PATCHED)
    if move_exec_count:
        next_content = next_content.replace(
            AGENT_HOST_MOVE_EXEC_PATCHED,
            AGENT_HOST_MOVE_EXEC_ORIGINAL,
        )
        stats.agent_host_move_exec += move_exec_count

    managed_subagent_route_count = next_content.count(
        MANAGED_SUBAGENT_ROUTE_PATCHED
    )
    if managed_subagent_route_count:
        next_content = next_content.replace(
            MANAGED_SUBAGENT_ROUTE_PATCHED,
            MANAGED_SUBAGENT_ROUTE_ORIGINAL,
        )
        stats.managed_subagent_route += managed_subagent_route_count

    managed_action_route_count = next_content.count(MANAGED_ACTION_ROUTE_PATCHED)
    if managed_action_route_count:
        next_content = next_content.replace(
            MANAGED_ACTION_ROUTE_PATCHED,
            MANAGED_ACTION_ROUTE_ORIGINAL,
        )
        stats.managed_action_route += managed_action_route_count

    legacy_action_route_count = next_content.count(MANAGED_ACTION_ROUTE_PATCHED_V1)
    if legacy_action_route_count:
        next_content = next_content.replace(
            MANAGED_ACTION_ROUTE_PATCHED_V1,
            MANAGED_ACTION_ROUTE_ORIGINAL,
        )
        stats.managed_action_route += legacy_action_route_count

    subagent_resume_mode_count = next_content.count(SUBAGENT_RESUME_MODE_PATCHED)
    if subagent_resume_mode_count:
        next_content = next_content.replace(
            SUBAGENT_RESUME_MODE_PATCHED,
            SUBAGENT_RESUME_MODE_ORIGINAL,
        )
        stats.subagent_resume_mode += subagent_resume_mode_count

    def disable_subagent_completion_wake(match: re.Match[str]) -> str:
        variable = match.group(1)
        return (
            variable
            + '.source==="interactive-child"||'
            + variable
            + '.payload.notificationContext==="user_driven_interactive_child"'
        )

    next_content, subagent_completion_wake_count = (
        SUBAGENT_COMPLETION_WAKE_PATCH_RE.subn(
            disable_subagent_completion_wake,
            next_content,
        )
    )
    stats.subagent_completion_wake += subagent_completion_wake_count

    managed_task_tool_patched = _managed_task_tool_patched()
    managed_task_tool_count = next_content.count(managed_task_tool_patched)
    if managed_task_tool_count:
        next_content = next_content.replace(
            managed_task_tool_patched,
            MANAGED_TASK_TOOL_ORIGINAL,
        )
        stats.managed_task_tool += managed_task_tool_count

    previous_task_tool = _managed_task_tool_patched_v126()
    previous_task_tool_count = next_content.count(previous_task_tool)
    if previous_task_tool_count:
        next_content = next_content.replace(
            previous_task_tool,
            MANAGED_TASK_TOOL_ORIGINAL,
        )
        stats.managed_task_tool += previous_task_tool_count

    previous_task_tool = _managed_task_tool_patched_v124()
    previous_task_tool_count = next_content.count(previous_task_tool)
    if previous_task_tool_count:
        next_content = next_content.replace(
            previous_task_tool,
            MANAGED_TASK_TOOL_ORIGINAL,
        )
        stats.managed_task_tool += previous_task_tool_count

    previous_task_tool = _managed_task_tool_patched_v125()
    previous_task_tool_count = next_content.count(previous_task_tool)
    if previous_task_tool_count:
        next_content = next_content.replace(
            previous_task_tool,
            MANAGED_TASK_TOOL_ORIGINAL,
        )
        stats.managed_task_tool += previous_task_tool_count

    managed_subagent_session_count = next_content.count(
        MANAGED_SUBAGENT_SESSION_PATCHED
    )
    if managed_subagent_session_count:
        next_content = next_content.replace(
            MANAGED_SUBAGENT_SESSION_PATCHED,
            MANAGED_SUBAGENT_SESSION_ORIGINAL,
        )
        stats.managed_subagent_session += managed_subagent_session_count

    identity_count = next_content.count(AGENT_HOST_IDENTITY_PATCHED)
    if identity_count:
        next_content = next_content.replace(
            AGENT_HOST_IDENTITY_PATCHED,
            AGENT_HOST_IDENTITY_ORIGINAL,
        )
        stats.agent_host_identity += identity_count

    max_tokens_count = next_content.count(MAX_TOKENS_PATCHED)
    if max_tokens_count:
        next_content = next_content.replace(
            MAX_TOKENS_PATCHED,
            MAX_TOKENS_ORIGINAL,
        )
        stats.max_tokens += max_tokens_count

    session_count = next_content.count(SAND_SESSION_STREAM_MARKER)
    if session_count:
        next_content = next_content.replace(SAND_SESSION_STREAM_MARKER, "")
        stats.session_stream += session_count

    # Strip the current direct injection exactly first, then use the
    # marker-bounded fallback for compatible historical variants.
    direct_injection = _direct_stream_injection()
    direct_count = next_content.count(direct_injection)
    if direct_count:
        next_content = next_content.replace(direct_injection, "")
        stats.direct_stream += direct_count
    if SAND_DIRECT_STREAM_MARKER in next_content:
        next_content, stripped_direct_count = DIRECT_STREAM_INJECTION_RE.subn(
            "", next_content
        )
        stats.direct_stream += stripped_direct_count

    rules_skills_count = next_content.count(RULES_SKILLS_EXEC_PATCHED)
    if rules_skills_count:
        next_content = next_content.replace(
            RULES_SKILLS_EXEC_PATCHED,
            RULES_SKILLS_EXEC_ORIGINAL,
        )
        stats.rules_skills += rules_skills_count

    mcp_filesystem_count = next_content.count(MCP_FILESYSTEM_PATCHED_V3113)
    if mcp_filesystem_count:
        next_content = next_content.replace(
            MCP_FILESYSTEM_PATCHED_V3113,
            MCP_FILESYSTEM_ORIGINAL_V3113,
        )
        stats.mcp_filesystem += mcp_filesystem_count

    def disable_user_rules(match: re.Match[str]) -> str:
        return (
            "injectLocalModeNonFileRules("
            + match.group("arg")
            + "){if(!"
            + match.group("flags")
            + ".localMode)return;"
        )

    next_content, user_rules_count = USER_RULES_PATCHED_RE.subn(
        disable_user_rules,
        next_content,
    )
    stats.user_rules += user_rules_count

    next_content, agent_host_count = AGENT_HOST_ENABLEMENT_PATCH_RE.subn(
        lambda match: match.group(2) + match.group(1) + match.group(3),
        next_content,
    )
    stats.agent_host_enablement += agent_host_count
    return next_content, stats


def _decode_js(data: bytes, path: Path) -> str:
    try:
        return data.decode("utf-8")
    except UnicodeDecodeError as exc:
        raise SandToolError(f"目标文件不是 UTF-8，拒绝修改：{path}") from exc


def _read_planned_file(path: Path) -> PlannedFile:
    original = path.read_bytes()
    return PlannedFile(
        original=original,
        next_bytes=original,
        mode=stat.S_IMODE(path.stat().st_mode),
    )


def _target_extension_name(layout: CursorLayout, file_path: Path) -> Optional[str]:
    for rel, extension_name in TARGET_SPECS:
        if not extension_name:
            continue
        candidate = layout.app_root.joinpath(*rel.split("/")).resolve()
        if candidate == file_path.resolve():
            return extension_name
    return None


def _update_extension_hashes(
    layout: CursorLayout,
    plan: Dict[Path, PlannedFile],
) -> None:
    changed_extensions: List[Tuple[str, bytes]] = []
    for file_path, planned in plan.items():
        extension_name = _target_extension_name(layout, file_path)
        if extension_name:
            changed_extensions.append((extension_name, planned.next_bytes))
    if not changed_extensions or layout.ext_host_path is None:
        return

    ext_path = layout.ext_host_path
    existing = plan.get(ext_path) or _read_planned_file(ext_path)
    next_content = _decode_js(existing.next_bytes, ext_path)
    original_content = _decode_js(existing.original, ext_path)

    for extension_name, next_main in changed_extensions:
        extension_id = "anysphere." + extension_name
        if f'"{extension_id}"' not in next_content:
            continue
        digest = hashlib.sha256(next_main).hexdigest()
        pattern = re.compile(
            rf'(\"{re.escape(extension_id)}\"\s*:\s*\{{[\s\S]{{0,2400}}?'
            rf'\"main\.js\"\s*:\s*\")[0-9a-f]{{64}}(\")'
        )
        next_content, count = pattern.subn(
            lambda match: match.group(1) + digest + match.group(2),
            next_content,
            count=1,
        )
        if count > 1:
            raise SandToolError(f"{extension_id} 的内嵌 main.js 哈希不唯一")

    if next_content != original_content:
        plan[ext_path] = PlannedFile(
            original=existing.original,
            next_bytes=next_content.encode("utf-8"),
            mode=existing.mode,
        )


def _sync_product_checksums(
    layout: CursorLayout,
    plan: Dict[Path, PlannedFile],
) -> None:
    product_file = _read_planned_file(layout.product_json)
    has_bom = product_file.original.startswith(b"\xef\xbb\xbf")
    try:
        product = json.loads(product_file.original.decode("utf-8-sig"))
    except Exception as exc:
        raise SandToolError("product.json 无法解析，拒绝提交补丁") from exc
    if not isinstance(product, dict):
        raise SandToolError("product.json 顶层必须是对象")
    checksums = product.get("checksums")
    if not isinstance(checksums, dict):
        return

    out_root = (layout.app_root / "out").resolve()
    changed = False
    for key in list(checksums.keys()):
        if not isinstance(key, str):
            continue
        parts = [part for part in re.split(r"[\\/]", key) if part]
        target = out_root.joinpath(*parts).resolve()
        if not _is_within(target, out_root):
            raise SandToolError(f"product.json checksum 路径逃逸：{key}")
        planned = plan.get(target)
        if planned is not None:
            data = planned.next_bytes
        elif target.is_file():
            data = target.read_bytes()
        else:
            continue
        digest = _product_checksum(data)
        if checksums.get(key) != digest:
            checksums[key] = digest
            changed = True

    if not changed:
        return
    text = json.dumps(product, ensure_ascii=False, indent="\t")
    next_bytes = text.encode("utf-8")
    if has_bom:
        next_bytes = b"\xef\xbb\xbf" + next_bytes
    plan[layout.product_json] = PlannedFile(
        original=product_file.original,
        next_bytes=next_bytes,
        mode=product_file.mode,
    )


def _planned_extension_names(
    layout: CursorLayout,
    plan: Mapping[Path, PlannedFile],
) -> Set[str]:
    names: Set[str] = set()
    for file_path in plan:
        extension_name = _target_extension_name(layout, file_path)
        if extension_name:
            names.add(extension_name)
    return names


def _verify_extension_hashes(
    layout: CursorLayout,
    extension_names: Iterable[str],
) -> None:
    names = set(extension_names)
    if layout.ext_host_path is None or not names:
        return
    ext_content = _decode_js(layout.ext_host_path.read_bytes(), layout.ext_host_path)
    for rel, extension_name in TARGET_SPECS:
        if not extension_name or extension_name not in names:
            continue
        main_path = layout.app_root.joinpath(*rel.split("/"))
        if not main_path.is_file():
            continue
        extension_id = "anysphere." + extension_name
        if f'"{extension_id}"' not in ext_content:
            continue
        pattern = re.compile(
            rf'\"{re.escape(extension_id)}\"\s*:\s*\{{[\s\S]{{0,2400}}?'
            rf'\"main\.js\"\s*:\s*\"([0-9a-f]{{64}})\"'
        )
        match = pattern.search(ext_content)
        if not match:
            continue
        expected = hashlib.sha256(main_path.read_bytes()).hexdigest()
        if match.group(1) != expected:
            raise SandToolError(f"{extension_id} 的内嵌哈希校验失败")


def _verify_product_checksums(layout: CursorLayout) -> int:
    product = json.loads(layout.product_json.read_bytes().decode("utf-8-sig"))
    checksums = product.get("checksums") if isinstance(product, dict) else None
    if not isinstance(checksums, dict):
        return 0
    out_root = (layout.app_root / "out").resolve()
    checked = 0
    for key, written in checksums.items():
        if not isinstance(key, str):
            continue
        parts = [part for part in re.split(r"[\\/]", key) if part]
        target = out_root.joinpath(*parts).resolve()
        if not _is_within(target, out_root) or not target.is_file():
            continue
        checked += 1
        if written != _product_checksum(target.read_bytes()):
            raise SandToolError(f"product.json 完整性哈希校验失败：{key}")
    return checked


def inspect_status(layout: CursorLayout) -> PatchStatus:
    client_markers = 0
    glass_client_markers = 0
    rules_skills_markers = 0
    mcp_filesystem_markers = 0
    user_rules_markers = 0
    eligibility_markers = 0
    managed_local_route_markers = 0
    local_runtime_load_markers = 0
    direct_stream_markers = 0
    grok_runtime_auth_markers = 0
    connect_gzip_fallback_markers = 0
    session_stream_markers = 0
    max_tokens_markers = 0
    agent_host_enablement_markers = 0
    agent_host_identity_markers = 0
    agent_host_move_exec_markers = 0
    managed_subagent_route_markers = 0
    managed_subagent_session_markers = 0
    managed_task_tool_markers = 0
    legacy_managed_task_tool_markers = 0
    managed_action_route_markers = 0
    legacy_managed_action_route_markers = 0
    subagent_resume_mode_markers = 0
    subagent_completion_wake_markers = 0
    legacy_client_markers = 0
    legacy_eligibility_markers = 0
    ide_matches = 0
    external_sand_matches = 0
    external_marker_count = 0
    bare_header_sites = 0
    foreign_counts: Dict[str, int] = {}
    foreign_files: Dict[str, Set[str]] = {}
    patched_files: List[Path] = []
    for target in layout.target_paths:
        content = _decode_js(target.read_bytes(), target)
        client_count = content.count(SAND_CLIENT_MARKER) + content.count(
            SAND_CLIENT_EXISTING_MARKER
        )
        glass_client_count = content.count(SAND_GLASS_CLIENT_MARKER)
        rules_skills_count = content.count(SAND_RULES_SKILLS_MARKER)
        mcp_filesystem_count = content.count(SAND_MCP_FILESYSTEM_MARKER)
        user_rules_count = content.count(SAND_USER_RULES_MARKER)
        eligibility_count = content.count(SAND_ELIGIBILITY_MARKER)
        managed_local_route_count = content.count(SAND_MANAGED_LOCAL_ROUTE_MARKER)
        local_runtime_load_count = content.count(SAND_LOCAL_RUNTIME_LOAD_MARKER)
        direct_stream_count = content.count(SAND_DIRECT_STREAM_MARKER)
        grok_runtime_auth_count = content.count(SAND_GROK_RUNTIME_AUTH_MARKER)
        connect_gzip_fallback_count = content.count(
            SAND_CONNECT_GZIP_FALLBACK_MARKER
        )
        session_stream_count = content.count(SAND_SESSION_STREAM_MARKER)
        max_tokens_count = content.count(SAND_MAX_TOKENS_MARKER)
        agent_host_enablement_count = content.count(
            SAND_AGENT_HOST_ENABLEMENT_MARKER
        )
        agent_host_identity_count = content.count(
            SAND_AGENT_HOST_IDENTITY_MARKER
        )
        agent_host_move_exec_count = content.count(
            SAND_AGENT_HOST_MOVE_EXEC_MARKER
        )
        managed_subagent_route_count = content.count(
            SAND_MANAGED_SUBAGENT_ROUTE_MARKER
        )
        managed_subagent_session_count = content.count(
            SAND_MANAGED_SUBAGENT_SESSION_MARKER
        )
        managed_task_tool_count = content.count(
            SAND_MANAGED_TASK_TOOL_MARKER
        )
        legacy_managed_task_tool_count = content.count(
            LEGACY_SAND_MANAGED_TASK_TOOL_MARKER
        ) + content.count(LEGACY_SAND_MANAGED_TASK_TOOL_MARKER_V2)
        managed_action_route_count = content.count(
            SAND_MANAGED_ACTION_ROUTE_MARKER
        )
        legacy_managed_action_route_count = content.count(
            LEGACY_SAND_MANAGED_ACTION_ROUTE_MARKER
        )
        subagent_resume_mode_count = content.count(
            SAND_SUBAGENT_RESUME_MODE_MARKER
        )
        subagent_completion_wake_count = content.count(
            SAND_SUBAGENT_COMPLETION_WAKE_MARKER
        )
        legacy_client_count = len(
            re.findall(
                rf"([\"'])sand\1{LEGACY_CLIENT_MARKER_PATTERN}",
                content,
            )
        )
        legacy_eligibility_count = content.count(
            "return!1;" + LEGACY_SAND_ELIGIBILITY_MARKER
        )
        external_marker_count += max(
            0,
            len(re.findall(CLIENT_MARKER_GUARD_PATTERN, content))
            - client_count
            - legacy_client_count,
        )
        external_marker_count += max(
            0,
            len(re.findall(ELIGIBILITY_MARKER_GUARD_PATTERN, content))
            - eligibility_count
            - legacy_eligibility_count,
        )
        if (
            client_count
            + glass_client_count
            + rules_skills_count
            + mcp_filesystem_count
            + user_rules_count
            + eligibility_count
            + legacy_client_count
            + legacy_eligibility_count
            + managed_local_route_count
            + local_runtime_load_count
            + direct_stream_count
            + grok_runtime_auth_count
            + connect_gzip_fallback_count
            + session_stream_count
            + max_tokens_count
            + agent_host_enablement_count
            + agent_host_identity_count
            + agent_host_move_exec_count
            + managed_subagent_route_count
            + managed_subagent_session_count
            + managed_task_tool_count
            + legacy_managed_task_tool_count
            + managed_action_route_count
            + legacy_managed_action_route_count
            + subagent_resume_mode_count
            + subagent_completion_wake_count
        ):
            patched_files.append(target)
        client_markers += client_count
        glass_client_markers += glass_client_count
        rules_skills_markers += rules_skills_count
        mcp_filesystem_markers += mcp_filesystem_count
        user_rules_markers += user_rules_count
        eligibility_markers += eligibility_count
        legacy_client_markers += legacy_client_count
        legacy_eligibility_markers += legacy_eligibility_count
        managed_local_route_markers += managed_local_route_count
        local_runtime_load_markers += local_runtime_load_count
        direct_stream_markers += direct_stream_count
        grok_runtime_auth_markers += grok_runtime_auth_count
        connect_gzip_fallback_markers += connect_gzip_fallback_count
        session_stream_markers += session_stream_count
        max_tokens_markers += max_tokens_count
        agent_host_enablement_markers += agent_host_enablement_count
        agent_host_identity_markers += agent_host_identity_count
        agent_host_move_exec_markers += agent_host_move_exec_count
        managed_subagent_route_markers += managed_subagent_route_count
        managed_subagent_session_markers += managed_subagent_session_count
        managed_task_tool_markers += managed_task_tool_count
        legacy_managed_task_tool_markers += legacy_managed_task_tool_count
        managed_action_route_markers += managed_action_route_count
        legacy_managed_action_route_markers += legacy_managed_action_route_count
        subagent_resume_mode_markers += subagent_resume_mode_count
        subagent_completion_wake_markers += subagent_completion_wake_count
        for _key, rule in CLIENT_RULES:
            for match in rule.finditer(content):
                if match.group(3) == "sand":
                    external_sand_matches += 1
                else:
                    ide_matches += 1
        bare_header_sites += len(BARE_HEADER_SITE_RE.findall(content))
        # Foreign markers: every Sand-style comment this script does not own,
        # aggregated by name.  The two guard sums above are kept as they are;
        # a foreign CLIENT/ELIGIBILITY-shaped marker is still listed here but
        # left out of the total below, since the guards already counted it.
        label = _target_label(target)
        for marker in ANY_SAND_MARKER_RE.findall(content):
            if marker in KNOWN_SAND_MARKERS:
                continue
            foreign_counts[marker] = foreign_counts.get(marker, 0) + 1
            foreign_files.setdefault(marker, set()).add(label)
    foreign_markers = tuple(
        (marker, count, tuple(sorted(foreign_files[marker])))
        for marker, count in sorted(foreign_counts.items())
    )
    external_marker_count += sum(
        count
        for marker, count, _files in foreign_markers
        if not (
            re.fullmatch(CLIENT_MARKER_GUARD_PATTERN, marker)
            or re.fullmatch(ELIGIBILITY_MARKER_GUARD_PATTERN, marker)
        )
    )
    return PatchStatus(
        client_markers=client_markers,
        glass_client_markers=glass_client_markers,
        rules_skills_markers=rules_skills_markers,
        mcp_filesystem_markers=mcp_filesystem_markers,
        user_rules_markers=user_rules_markers,
        eligibility_markers=eligibility_markers,
        ide_matches=ide_matches,
        external_sand_matches=external_sand_matches,
        external_marker_count=external_marker_count,
        legacy_client_markers=legacy_client_markers,
        legacy_eligibility_markers=legacy_eligibility_markers,
        patched_files=tuple(patched_files),
        managed_local_route_markers=managed_local_route_markers,
        local_runtime_load_markers=local_runtime_load_markers,
        direct_stream_markers=direct_stream_markers,
        grok_runtime_auth_markers=grok_runtime_auth_markers,
        connect_gzip_fallback_markers=connect_gzip_fallback_markers,
        session_stream_markers=session_stream_markers,
        max_tokens_markers=max_tokens_markers,
        agent_host_enablement_markers=agent_host_enablement_markers,
        agent_host_identity_markers=agent_host_identity_markers,
        agent_host_move_exec_markers=agent_host_move_exec_markers,
        managed_subagent_route_markers=managed_subagent_route_markers,
        managed_subagent_session_markers=managed_subagent_session_markers,
        managed_task_tool_markers=managed_task_tool_markers,
        legacy_managed_task_tool_markers=legacy_managed_task_tool_markers,
        managed_action_route_markers=managed_action_route_markers,
        legacy_managed_action_route_markers=legacy_managed_action_route_markers,
        subagent_resume_mode_markers=subagent_resume_mode_markers,
        subagent_completion_wake_markers=subagent_completion_wake_markers,
        bare_header_sites=bare_header_sites,
        foreign_markers=foreign_markers,
    )


def _format_foreign_markers(status: PatchStatus) -> str:
    # All files are listed, in TARGET_SPECS order (the labels _target_label
    # produces for those paths); a label outside the table sorts last.
    label_order = {
        _target_label(Path(rel)): index
        for index, (rel, _ext) in enumerate(TARGET_SPECS)
    }
    parts: List[str] = []
    for marker, count, files in status.foreign_markers:
        ordered = sorted(
            files,
            key=lambda label: (label_order.get(label, len(label_order)), label),
        )
        shown = "、".join(ordered)
        parts.append(f"{marker} ×{count}（{_marker_owner(marker)}，{shown}）")
    if not parts:
        # Only the CLIENT/ELIGIBILITY guard fired (a registered marker sitting
        # in an unexpected position); there is nothing to attribute.
        return f"{status.external_marker_count} 处无法归类的客户端/资格标记"
    return "，".join(parts)


def _bare_header_notice(count: int) -> str:
    return (
        f"检测到 {count} 处裸字面量 header.set 站点（疑为 SandClaimer 卸载残留），"
        "将按正常站点处理；卸载后保持该形态，如需恢复原始写法请重装 Cursor 3.18.9"
    )


def _create_backup(
    layout: CursorLayout,
    plan: Mapping[Path, PlannedFile],
    operation: str,
) -> Tuple[Path, Dict[str, object]]:
    app_hash = hashlib.sha256(str(layout.app_root).encode("utf-8")).hexdigest()[:16]
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S.%fZ")
    backup_dir = _config_dir() / "backups" / app_hash / f"{stamp}-{operation}"
    files_dir = backup_dir / "files"
    entries: List[Dict[str, object]] = []
    for path, planned in plan.items():
        try:
            relative = path.resolve().relative_to(layout.app_root.resolve())
        except ValueError as exc:
            raise SandToolError(f"计划文件逃逸出 Cursor app：{path}") from exc
        backup_file = files_dir / relative
        _atomic_write(backup_file, planned.original, planned.mode)
        entries.append(
            {
                "path": relative.as_posix(),
                "originalSha256": _sha256(planned.original),
                "nextSha256": _sha256(planned.next_bytes),
                "mode": planned.mode,
            }
        )
    manifest: Dict[str, object] = {
        "version": 1,
        "toolVersion": TOOL_VERSION,
        "operation": operation,
        "status": "prepared",
        "appRoot": str(layout.app_root),
        "cursorVersion": layout.version,
        "createdAt": datetime.now(timezone.utc).isoformat(),
        "files": entries,
    }
    _write_json_atomic(backup_dir / "manifest.json", manifest)
    return backup_dir, manifest


def _update_backup_manifest(
    backup_dir: Path,
    manifest: Dict[str, object],
    status_value: str,
    error: Optional[str] = None,
) -> None:
    manifest["status"] = status_value
    manifest["finishedAt"] = datetime.now(timezone.utc).isoformat()
    if error:
        manifest["error"] = error[:1000]
    _write_json_atomic(backup_dir / "manifest.json", manifest)


def _latest_committed_install_backup(
    layout: CursorLayout,
) -> Optional[Tuple[Path, Dict[str, object]]]:
    """Return the newest committed install backup for this exact app."""
    app_hash = hashlib.sha256(str(layout.app_root).encode("utf-8")).hexdigest()[:16]
    root = _config_dir() / "backups" / app_hash
    if not root.is_dir():
        return None
    for directory in sorted(root.glob("*-install"), reverse=True):
        try:
            manifest = json.loads(
                (directory / "manifest.json").read_text(encoding="utf-8")
            )
        except (OSError, ValueError):
            continue
        if not isinstance(manifest, dict):
            continue
        if (
            manifest.get("status") == "committed"
            and manifest.get("appRoot") == str(layout.app_root)
            and manifest.get("cursorVersion") == layout.version
            and isinstance(manifest.get("files"), list)
            and manifest["files"]
        ):
            return directory, manifest
    return None


def _restore_committed_install_plan(
    layout: CursorLayout,
    backup: Tuple[Path, Dict[str, object]],
) -> Dict[Path, PlannedFile]:
    """Build a byte-exact uninstall plan from an owned install manifest."""
    directory, manifest = backup
    entries = manifest.get("files")
    if not isinstance(entries, list) or not entries:
        raise SandToolError("安装备份缺少文件清单，拒绝卸载")
    plan: Dict[Path, PlannedFile] = {}
    for entry in entries:
        if not isinstance(entry, dict):
            raise SandToolError("安装备份包含无效文件项，拒绝卸载")
        relative = entry.get("path")
        if not isinstance(relative, str) or not relative:
            raise SandToolError("安装备份包含无效路径，拒绝卸载")
        path = (layout.app_root / Path(relative)).resolve()
        backup_file = (directory / "files" / Path(relative)).resolve()
        if not _is_within(path, layout.app_root) or not path.is_file():
            raise SandToolError(f"安装备份路径无效：{relative}")
        if not _is_within(backup_file, directory) or not backup_file.is_file():
            raise SandToolError(f"安装备份文件缺失：{relative}")
        original = backup_file.read_bytes()
        current = path.read_bytes()
        if entry.get("originalSha256") != _sha256(original):
            raise SandToolError(f"安装备份哈希损坏：{relative}")
        if entry.get("nextSha256") != _sha256(current):
            raise SandToolError(f"文件已在安装后被外部修改，拒绝覆盖：{relative}")
        mode = int(entry.get("mode") or stat.S_IMODE(path.stat().st_mode))
        plan[path] = PlannedFile(
            original=current,
            next_bytes=original,
            mode=mode,
        )
    return plan


def _commit_plan(
    layout: CursorLayout,
    plan: Mapping[Path, PlannedFile],
    operation: str,
    validator,
) -> Tuple[Tuple[Path, ...], Path]:
    if not plan:
        raise SandToolError("内部错误：提交计划为空")
    for path, planned in plan.items():
        if _sha256(path.read_bytes()) != _sha256(planned.original):
            raise SandToolError(f"文件在计划生成后发生变化，已停止操作：{path}")
    backup_dir, manifest = _create_backup(layout, plan, operation)
    attempted: List[Path] = []
    written: List[Path] = []
    try:
        for path, planned in plan.items():
            if _sha256(path.read_bytes()) != _sha256(planned.original):
                raise SandToolError(f"文件在写入前发生变化，已停止操作：{path}")
            attempted.append(path)
            _atomic_write(path, planned.next_bytes, planned.mode)
            written.append(path)
        validator()
        for path, planned in plan.items():
            if _sha256(path.read_bytes()) != _sha256(planned.next_bytes):
                raise SandToolError(f"写入后哈希校验失败：{path}")
        _update_backup_manifest(backup_dir, manifest, "committed")
        return tuple(written), backup_dir
    except (Exception, KeyboardInterrupt) as exc:
        rollback_errors: List[str] = []
        for path in reversed(attempted):
            planned = plan[path]
            try:
                current_hash = _sha256(path.read_bytes())
                original_hash = _sha256(planned.original)
                next_hash = _sha256(planned.next_bytes)
                if current_hash == original_hash:
                    continue
                if current_hash != next_hash:
                    rollback_errors.append(f"{path}: 文件已被外部修改，未覆盖")
                    continue
                _atomic_write(path, planned.original, planned.mode)
            except Exception as rollback_exc:
                rollback_errors.append(f"{path}: {rollback_exc}")
        if attempted and sys.platform == "darwin":
            try:
                _mac_seal(layout)
            except Exception as rollback_exc:
                rollback_errors.append(f"macOS rollback signing: {rollback_exc}")
        message = str(exc)
        if rollback_errors:
            message += "; rollback errors: " + " | ".join(rollback_errors)
        try:
            _update_backup_manifest(backup_dir, manifest, "rolled_back", message)
        except Exception:
            pass
        if rollback_errors:
            raise SandToolError(
                "补丁失败且有文件未能自动回滚，请保留备份目录："
                f"{backup_dir}\n{message}"
            ) from exc
        raise


def _windows_close_cursor(layout: CursorLayout) -> int:
    powershell = _powershell_executable()
    if not powershell:
        raise SandToolError("未找到 PowerShell，无法安全关闭 Cursor")
    script = r"""
$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = [System.Text.UTF8Encoding]::new()
$target = [System.IO.Path]::GetFullPath($env:SAND_CURSOR_EXE)
function Get-SandCursorTargets {
  @(Get-CimInstance Win32_Process -Filter "Name='Cursor.exe'" -ErrorAction Stop | Where-Object {
    $_.ExecutablePath -and [string]::Equals(
      [System.IO.Path]::GetFullPath($_.ExecutablePath),
      $target,
      [System.StringComparison]::OrdinalIgnoreCase
    )
  })
}
$before = @(Get-SandCursorTargets)
foreach ($item in $before) {
  try {
    $process = Get-Process -Id $item.ProcessId -ErrorAction Stop
    if ($process.MainWindowHandle -ne 0) { [void]$process.CloseMainWindow() }
  } catch {}
}
$deadline = [DateTime]::UtcNow.AddSeconds(12)
while ([DateTime]::UtcNow -lt $deadline -and @(Get-SandCursorTargets).Count -gt 0) {
  Start-Sleep -Milliseconds 250
}
$remaining = @(Get-SandCursorTargets)
foreach ($item in $remaining) {
  try { Stop-Process -Id $item.ProcessId -Force -ErrorAction Stop } catch {}
}
Start-Sleep -Milliseconds 500
if (@(Get-SandCursorTargets).Count -gt 0) { exit 3 }
Write-Output ("CLOSED=" + $before.Count)
""".strip()
    env = dict(os.environ)
    env["SAND_CURSOR_EXE"] = str(layout.executable)
    try:
        result = subprocess.run(
            [powershell, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script],
            capture_output=True,
            text=True,
            encoding="utf-8",
            errors="replace",
            env=env,
            timeout=25,
            check=False,
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        raise SandToolError("无法安全关闭所选 Cursor 进程，请手动退出后重试") from exc
    if result.returncode != 0:
        raise SandToolError(
            "无法安全关闭所选 Cursor 进程，请手动退出后重试"
        )
    match = re.search(r"CLOSED=(\d+)", result.stdout)
    return int(match.group(1)) if match else 0


def _mac_bundle_pids(layout: CursorLayout) -> List[int]:
    bundle = _find_app_bundle(layout.app_root)
    if bundle is None:
        return []
    contents = (bundle.resolve() / "Contents").resolve()
    pids: List[int] = []
    for pid, executable in _mac_process_paths(strict=True):
        if pid != os.getpid() and _is_within(executable, contents):
            pids.append(pid)
    return pids


def _wait_for_mac_exit(layout: CursorLayout, timeout_seconds: float) -> bool:
    deadline = time.monotonic() + timeout_seconds
    while time.monotonic() < deadline:
        if not _mac_bundle_pids(layout):
            return True
        time.sleep(0.25)
    return not _mac_bundle_pids(layout)


def _mac_close_cursor(layout: CursorLayout) -> int:
    before = _mac_bundle_pids(layout)
    if not before:
        return 0
    selected_bundle = _find_app_bundle(layout.app_root)
    running_bundles: Dict[str, Path] = {}
    for _pid, executable in _mac_process_paths(strict=True):
        bundle = _bundle_for_executable(executable)
        if bundle is not None:
            running_bundles.setdefault(_path_key(bundle), bundle)
    if selected_bundle is not None and len(running_bundles) == 1:
        osascript = shutil.which("osascript") or "/usr/bin/osascript"
        try:
            subprocess.run(
                [
                    osascript,
                    "-e",
                    'tell application id "com.todesktop.230313mzl4w4u92" to quit',
                ],
                capture_output=True,
                timeout=10,
                check=False,
            )
        except (OSError, subprocess.TimeoutExpired):
            pass
        if _wait_for_mac_exit(layout, 12):
            return len(before)

    for pid in _mac_bundle_pids(layout):
        try:
            os.kill(pid, signal.SIGTERM)
        except (ProcessLookupError, PermissionError):
            pass
    if _wait_for_mac_exit(layout, 3):
        return len(before)

    for pid in _mac_bundle_pids(layout):
        try:
            os.kill(pid, signal.SIGKILL)
        except (ProcessLookupError, PermissionError):
            pass
    if not _wait_for_mac_exit(layout, 2):
        raise SandToolError("无法安全关闭所选 Cursor 进程，请手动退出后重试")
    return len(before)


def close_cursor(layout: CursorLayout) -> int:
    if sys.platform == "win32":
        return _windows_close_cursor(layout)
    if sys.platform == "darwin":
        return _mac_close_cursor(layout)
    raise SandToolError("当前仅支持 Windows 和 macOS")


def start_cursor(layout: CursorLayout) -> bool:
    try:
        if sys.platform == "win32":
            startupinfo = subprocess.STARTUPINFO()
            startupinfo.dwFlags |= subprocess.STARTF_USESHOWWINDOW
            subprocess.Popen(
                [str(layout.executable)],
                cwd=str(layout.install_root),
                stdin=subprocess.DEVNULL,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                startupinfo=startupinfo,
                creationflags=0x00000008 | 0x00000200,
            )
            return True
        if sys.platform == "darwin":
            bundle = _find_app_bundle(layout.app_root)
            if bundle is None:
                return False
            subprocess.run(
                [shutil.which("open") or "/usr/bin/open", "-a", str(bundle)],
                stdin=subprocess.DEVNULL,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                timeout=20,
                check=False,
            )
            return True
    except (OSError, subprocess.TimeoutExpired):
        return False
    return False


def _skip_restart() -> bool:
    return os.environ.get("SAND_PATCH_SKIP_RESTART", "").strip() == "1"


def _restart_outcome(completed: str) -> str:
    if _skip_restart():
        return f"{completed}，未自动重启 Cursor"
    return f"{completed}，Cursor 已重新启动"


def _mac_seal(layout: CursorLayout) -> None:
    """Remove stale attributes, ad-hoc sign, and verify the modified app."""
    if sys.platform != "darwin":
        return
    bundle = _find_app_bundle(layout.app_root)
    if bundle is None:
        raise SandToolError("无法定位 Cursor.app，不能完成 macOS 重签名")
    bundle_text = str(bundle)
    xattr = subprocess.run(
        [shutil.which("xattr") or "/usr/bin/xattr", "-cr", bundle_text],
        capture_output=True,
        text=True,
        timeout=180,
        check=False,
    )
    if xattr.returncode != 0:
        raise SandToolError(f"清理 Cursor 扩展属性失败：{xattr.stderr.strip()[:300]}")
    codesign = shutil.which("codesign") or "/usr/bin/codesign"
    signed = subprocess.run(
        [codesign, "--force", "--deep", "--sign", "-", "--timestamp=none", bundle_text],
        capture_output=True,
        text=True,
        timeout=180,
        check=False,
    )
    if signed.returncode != 0:
        raise SandToolError(f"Cursor ad-hoc 重签名失败：{signed.stderr.strip()[:300]}")
    verified = subprocess.run(
        [codesign, "--verify", "--deep", "--strict", bundle_text],
        capture_output=True,
        text=True,
        timeout=180,
        check=False,
    )
    if verified.returncode != 0:
        raise SandToolError(f"Cursor 重签名校验失败：{verified.stderr.strip()[:300]}")


def _build_install_plan(
    layout: CursorLayout,
) -> Tuple[Dict[Path, PlannedFile], PatchStats]:
    plan: Dict[Path, PlannedFile] = {}
    total = PatchStats()
    for target in layout.target_paths:
        original = _read_planned_file(target)
        content = _decode_js(original.original, target)
        next_content, stats = apply_patch_to_content(content)
        if next_content != content:
            plan[target] = PlannedFile(
                original=original.original,
                next_bytes=next_content.encode("utf-8"),
                mode=original.mode,
            )
        total.is_glass += stats.is_glass
        total.glass_client += stats.glass_client
        total.rules_skills += stats.rules_skills
        total.mcp_filesystem += stats.mcp_filesystem
        total.user_rules += stats.user_rules
        total.object_header += stats.object_header
        total.set_header += stats.set_header
        total.set_header_bare += stats.set_header_bare
        total.eligibility += stats.eligibility
        total.adopted_sand += stats.adopted_sand
        total.migrated_client += stats.migrated_client
        total.migrated_eligibility += stats.migrated_eligibility
        total.migrated_task_tool += stats.migrated_task_tool
        total.migrated_action_route += stats.migrated_action_route
        total.migrated_direct_stream += stats.migrated_direct_stream
        total.migrated_session_stream += stats.migrated_session_stream
        total.migrated_connect_gzip_fallback += (
            stats.migrated_connect_gzip_fallback
        )
        total.managed_local_route += stats.managed_local_route
        total.local_runtime_load += stats.local_runtime_load
        total.direct_stream += stats.direct_stream
        total.grok_runtime_auth += stats.grok_runtime_auth
        total.connect_gzip_fallback += stats.connect_gzip_fallback
        total.session_stream += stats.session_stream
        total.max_tokens += stats.max_tokens
        total.agent_host_enablement += stats.agent_host_enablement
        total.agent_host_identity += stats.agent_host_identity
        total.agent_host_move_exec += stats.agent_host_move_exec
        total.managed_subagent_route += stats.managed_subagent_route
        total.managed_subagent_session += stats.managed_subagent_session
        total.managed_task_tool += stats.managed_task_tool
        total.managed_action_route += stats.managed_action_route
        total.subagent_resume_mode += stats.subagent_resume_mode
        total.subagent_completion_wake += stats.subagent_completion_wake
    if plan:
        _update_extension_hashes(layout, plan)
        _sync_product_checksums(layout, plan)
    return plan, total


def _build_uninstall_plan(
    layout: CursorLayout,
) -> Tuple[Dict[Path, PlannedFile], RemoveStats]:
    plan: Dict[Path, PlannedFile] = {}
    total = RemoveStats()
    for target in layout.target_paths:
        original = _read_planned_file(target)
        content = _decode_js(original.original, target)
        next_content, stats = remove_patch_from_content(content)
        if next_content != content:
            plan[target] = PlannedFile(
                original=original.original,
                next_bytes=next_content.encode("utf-8"),
                mode=original.mode,
            )
        total.client_type += stats.client_type
        total.glass_client += stats.glass_client
        total.rules_skills += stats.rules_skills
        total.mcp_filesystem += stats.mcp_filesystem
        total.user_rules += stats.user_rules
        total.eligibility += stats.eligibility
        total.managed_local_route += stats.managed_local_route
        total.local_runtime_load += stats.local_runtime_load
        total.direct_stream += stats.direct_stream
        total.grok_runtime_auth += stats.grok_runtime_auth
        total.connect_gzip_fallback += stats.connect_gzip_fallback
        total.session_stream += stats.session_stream
        total.max_tokens += stats.max_tokens
        total.agent_host_enablement += stats.agent_host_enablement
        total.agent_host_identity += stats.agent_host_identity
        total.agent_host_move_exec += stats.agent_host_move_exec
        total.managed_subagent_route += stats.managed_subagent_route
        total.managed_subagent_session += stats.managed_subagent_session
        total.managed_task_tool += stats.managed_task_tool
        total.managed_action_route += stats.managed_action_route
        total.subagent_resume_mode += stats.subagent_resume_mode
        total.subagent_completion_wake += stats.subagent_completion_wake
    if plan:
        _update_extension_hashes(layout, plan)
        _sync_product_checksums(layout, plan)
    return plan, total


def _grok_runtime_auth_ready_count(contents: Iterable[str]) -> int:
    variants = frozenset(
        (
            *GROK_RUNTIME_AUTH_ORIGINALS_V3113,
            *GROK_RUNTIME_AUTH_PATCHES_V3113,
            *GROK_RUNTIME_AUTH_PATCHES_V141,
            *GROK_RUNTIME_AUTH_PATCHES_V144,
            *GROK_RUNTIME_AUTH_PATCHES_V145,
            *GROK_RUNTIME_AUTH_PATCHES_V147,
        )
    )
    return sum(content.count(variant) for content in contents for variant in variants)


def install(layout: CursorLayout) -> int:
    if not _cursor_version_supported(layout.version):
        raise SandToolError(
            f"当前 Cursor 版本为 {layout.version}，"
            f"本工具仅适配 Cursor {SUPPORTED_CURSOR_VERSION_LABEL}。"
            "请更换为适配版本后再安装"
        )
    _write_grok_relay_config()
    # Foreign markers and the bare-site notice come before the anchor checks,
    # so a machine holding another tool's install sees the marker list rather
    # than a "未唯一匹配" anchor failure.
    before = inspect_status(layout)
    if before.external_marker_count:
        raise SandToolError(
            "检测到其他工具的 Sand 标记："
            + _format_foreign_markers(before)
            + "；请先用该工具卸载，或恢复原始 3.18.9 文件后再安装"
        )
    if before.bare_header_sites:
        # Interactive runs are inside LoadingSpinner, whose "\r" frames would
        # otherwise overwrite the start of the notice; clear the line first.
        print("\r" + " " * 60 + "\r", end="")
        print_warn(_bare_header_notice(before.bare_header_sites))
    target_contents = {
        target: _decode_js(target.read_bytes(), target)
        for target in layout.target_paths
    }
    grok_runtime_auth_ready_count = _grok_runtime_auth_ready_count(
        target_contents.values()
    )
    if grok_runtime_auth_ready_count != EXPECTED_GROK_RUNTIME_AUTH_SITES:
        raise SandToolError(
            "当前 Cursor 未唯一匹配到 Grok Bot Box Relay Stream 鉴权入口："
            f"runtimeAuthReady={grok_runtime_auth_ready_count}"
        )
    connect_gzip_fallback_ready_count = sum(
        target_content.count(CONNECT_GZIP_ENVELOPE_ORIGINAL)
        + target_content.count(CONNECT_GZIP_ENVELOPE_PATCHED)
        for target_content in target_contents.values()
    )
    if (
        connect_gzip_fallback_ready_count
        != EXPECTED_CONNECT_GZIP_FALLBACK_MARKERS
    ):
        raise SandToolError(
            "当前 Cursor 未完整匹配到 Connect gzip 解码入口："
            f"gzipFallbackReady={connect_gzip_fallback_ready_count}"
        )
    move_exec_ready_count = sum(
        target_content.count(AGENT_HOST_MOVE_EXEC_READY_ANCHOR)
        for target_content in target_contents.values()
    )
    if move_exec_ready_count != 1:
        raise SandToolError(
            "当前 Cursor 未唯一匹配到 Agent Host 工具执行资源分支："
            f"moveExecReady={move_exec_ready_count}"
        )
    # Cursor 3.19.13: the separate subagent-route and action-route eligibility
    # gates from 3.18.9 no longer exist; both are consolidated into the router's
    # A(...) evaluator, which the managed-local route forcing bypasses.  These
    # ready checks are therefore retired (see MANAGED_LOCAL_ROUTE_PATCHED).
    subagent_resume_mode_ready_count = sum(
        target_content.count(SUBAGENT_RESUME_MODE_ORIGINAL)
        + target_content.count(SUBAGENT_RESUME_MODE_PATCHED)
        for target_content in target_contents.values()
    )
    if subagent_resume_mode_ready_count != 1:
        raise SandToolError(
            "当前 Cursor 未唯一匹配到 Task resume 模式规则："
            f"resumeModeReady={subagent_resume_mode_ready_count}"
        )
    subagent_completion_wake_ready_count = 0
    for target_content in target_contents.values():

        def normalize_completion_wake(match: re.Match[str]) -> str:
            variable = match.group(1)
            return (
                variable
                + '.source==="interactive-child"||'
                + variable
                + '.payload.notificationContext==="user_driven_interactive_child"'
            )

        normalized_content = SUBAGENT_COMPLETION_WAKE_PATCH_RE.sub(
            normalize_completion_wake,
            target_content,
        )
        subagent_completion_wake_ready_count += len(
            SUBAGENT_COMPLETION_WAKE_RE.findall(normalized_content)
        )
    if (
        subagent_completion_wake_ready_count
        != EXPECTED_BACKGROUND_COMPLETION_WAKE_MARKERS
    ):
        raise SandToolError(
            "当前 Cursor 未完整匹配到子代理后台完成唤醒规则："
            f"completionWakeReady={subagent_completion_wake_ready_count}"
        )
    # Cursor 3.19.13 constructs taskToolProps natively (Ne({parentModelId,
    # modelInfo})) and defaults useClientSideSubagent on, so the 3.18.9
    # task-tool forcing is no longer required; its ready checks are retired.
    plan, _stats = _build_install_plan(layout)
    if not plan:
        if before.installed and before.stream_mode_installed:
            if not _skip_restart():
                start_cursor(layout)
            return 0
        raise SandToolError("当前 Cursor 版本未匹配到 Sand 客户端模式规则")
    if (
        before.managed_local_route_markers + _stats.managed_local_route != 1
        or (
            before.glass_client_markers + _stats.glass_client
            != EXPECTED_GLASS_CLIENT_MARKERS
        )
        or before.rules_skills_markers + _stats.rules_skills != 0
        or (
            before.mcp_filesystem_markers + _stats.mcp_filesystem
            != EXPECTED_MCP_FILESYSTEM_MARKERS
        )
        or (
            before.user_rules_markers + _stats.user_rules
            != EXPECTED_USER_RULES_MARKERS
        )
        or (
            before.local_runtime_load_markers
            + _stats.local_runtime_load
            != 1
        )
        or (
            before.agent_host_identity_markers
            + _stats.agent_host_identity
            != 1
        )
        or (
            before.session_stream_markers
            - _stats.migrated_session_stream
            + _stats.session_stream
            != 0
        )
        or (
            before.direct_stream_markers
            - _stats.migrated_direct_stream
            + _stats.direct_stream
            != 1
        )
        or (
            max(
                before.grok_runtime_auth_markers,
                _stats.grok_runtime_auth,
            )
            != EXPECTED_GROK_RUNTIME_AUTH_SITES
        )
        or (
            before.connect_gzip_fallback_markers
            - _stats.migrated_connect_gzip_fallback
            + _stats.connect_gzip_fallback
            != EXPECTED_CONNECT_GZIP_FALLBACK_MARKERS
        )
        or before.max_tokens_markers + _stats.max_tokens != 1
        or (
            before.agent_host_enablement_markers
            + _stats.agent_host_enablement
            != 2
        )
        or (
            before.agent_host_move_exec_markers
            + _stats.agent_host_move_exec
            != 1
        )
        or (
            before.managed_subagent_route_markers
            + _stats.managed_subagent_route
            != 0
        )
        or (
            before.managed_subagent_session_markers
            + _stats.managed_subagent_session
            != 0
        )
        or (
            before.managed_task_tool_markers
            + _stats.managed_task_tool
            + _stats.migrated_task_tool
            != 0
        )
        or (
            before.managed_action_route_markers
            + _stats.managed_action_route
            + _stats.migrated_action_route
            != 0
        )
        or (
            before.legacy_managed_action_route_markers
            - _stats.migrated_action_route
            != 0
        )
        or (
            before.subagent_resume_mode_markers
            + _stats.subagent_resume_mode
            != 1
        )
        or (
            before.subagent_completion_wake_markers
            + _stats.subagent_completion_wake
            != EXPECTED_BACKGROUND_COMPLETION_WAKE_MARKERS
        )
    ):
        raise SandToolError(
            "当前 Cursor 版本未完整匹配 Sand Stream 规则："
            f"route={before.managed_local_route_markers + _stats.managed_local_route}, "
            "glassClient="
            f"{before.glass_client_markers + _stats.glass_client}, "
            "rulesSkills="
            f"{before.rules_skills_markers + _stats.rules_skills}, "
            "mcpFs="
            f"{before.mcp_filesystem_markers + _stats.mcp_filesystem}, "
            "userRules="
            f"{before.user_rules_markers + _stats.user_rules}, "
            "runtimeLoad="
            f"{before.local_runtime_load_markers + _stats.local_runtime_load}, "
            "identity="
            f"{before.agent_host_identity_markers + _stats.agent_host_identity}, "
            "sessionStream="
            f"{before.session_stream_markers - _stats.migrated_session_stream + _stats.session_stream}, "
            "directStream="
            f"{before.direct_stream_markers - _stats.migrated_direct_stream + _stats.direct_stream}, "
            "runtimeAuth="
            f"{max(before.grok_runtime_auth_markers, _stats.grok_runtime_auth)}, "
            "gzipFallback="
            f"{before.connect_gzip_fallback_markers - _stats.migrated_connect_gzip_fallback + _stats.connect_gzip_fallback}, "
            "maxTokens="
            f"{before.max_tokens_markers + _stats.max_tokens}, "
            "agentHost="
            f"{before.agent_host_enablement_markers + _stats.agent_host_enablement}, "
            "moveExec="
            f"{before.agent_host_move_exec_markers + _stats.agent_host_move_exec}, "
            "subagentRoute="
            f"{before.managed_subagent_route_markers + _stats.managed_subagent_route}, "
            "subagentSession="
            f"{before.managed_subagent_session_markers + _stats.managed_subagent_session}, "
            "taskTool="
            f"{before.managed_task_tool_markers + _stats.managed_task_tool + _stats.migrated_task_tool}, "
            "actionRoute="
            f"{before.managed_action_route_markers + _stats.managed_action_route + _stats.migrated_action_route}, "
            "legacyActionRoute="
            f"{before.legacy_managed_action_route_markers - _stats.migrated_action_route}, "
            "resumeMode="
            f"{before.subagent_resume_mode_markers + _stats.subagent_resume_mode}, "
            "completionWake="
            f"{before.subagent_completion_wake_markers + _stats.subagent_completion_wake}"
        )

    close_cursor(layout)
    changed_extensions = _planned_extension_names(layout, plan)

    def validate() -> None:
        status = inspect_status(layout)
        if (
            not status.installed
            or not status.stream_mode_installed
            or status.glass_client_markers != EXPECTED_GLASS_CLIENT_MARKERS
            or status.rules_skills_markers != 0
            or status.mcp_filesystem_markers != EXPECTED_MCP_FILESYSTEM_MARKERS
            or status.user_rules_markers != EXPECTED_USER_RULES_MARKERS
            or status.grok_runtime_auth_markers != EXPECTED_GROK_RUNTIME_AUTH_SITES
            or status.connect_gzip_fallback_markers
            != EXPECTED_CONNECT_GZIP_FALLBACK_MARKERS
            or status.agent_host_move_exec_markers != 1
            or status.managed_subagent_route_markers != 0
            or status.managed_subagent_session_markers != 0
            or status.managed_task_tool_markers != 0
            or status.legacy_managed_task_tool_markers != 0
            or status.managed_action_route_markers != 0
            or status.legacy_managed_action_route_markers != 0
            or status.session_stream_markers != 0
            or status.direct_stream_markers != 1
            or status.max_tokens_markers != 1
            or status.subagent_resume_mode_markers != 1
            or status.subagent_completion_wake_markers
            != EXPECTED_BACKGROUND_COMPLETION_WAKE_MARKERS
            or status.ide_matches != 0
            or status.external_marker_count != 0
            or status.legacy_client_markers != 0
            or status.legacy_eligibility_markers != 0
        ):
            raise SandToolError(
                "安装后状态校验失败："
                f"markers={status.client_markers + status.eligibility_markers}, "
                f"remainingIde={status.ide_matches}, "
                f"glassClient={status.glass_client_markers}, "
                f"rulesSkills={status.rules_skills_markers}, "
                f"mcpFs={status.mcp_filesystem_markers}, "
                f"userRules={status.user_rules_markers}, "
                f"streamMode={status.stream_mode_installed}, "
                f"sessionStream={status.session_stream_markers}, "
                f"directStream={status.direct_stream_markers}, "
                f"gzipFallback={status.connect_gzip_fallback_markers}, "
                f"maxTokens={status.max_tokens_markers}, "
                f"moveExec={status.agent_host_move_exec_markers}, "
                f"subagentRoute={status.managed_subagent_route_markers}, "
                "managedTask="
                f"{status.managed_subagent_session_markers}/"
                f"{status.managed_task_tool_markers}, "
                f"actionRoute={status.managed_action_route_markers}, "
                f"legacyActionRoute={status.legacy_managed_action_route_markers}, "
                f"resumeMode={status.subagent_resume_mode_markers}, "
                f"completionWake={status.subagent_completion_wake_markers}, "
                "remainingLegacy="
                f"{status.legacy_client_markers + status.legacy_eligibility_markers + status.legacy_managed_task_tool_markers + status.legacy_managed_action_route_markers}"
            )
        _verify_extension_hashes(layout, changed_extensions)
        _verify_product_checksums(layout)
        _mac_seal(layout)

    _commit_plan(layout, plan, "install", validate)
    close_cursor(layout)
    if not _skip_restart():
        start_cursor(layout)
    return 0


def uninstall(layout: CursorLayout) -> int:
    before = inspect_status(layout)
    if before.external_marker_count:
        raise SandToolError(
            "检测到其他工具的 Sand 标记："
            + _format_foreign_markers(before)
            + "；拒绝修改，请先用该工具卸载，或恢复原始 3.18.9 文件"
        )
    backup = _latest_committed_install_backup(layout) if before.installed else None
    if backup is not None:
        plan = _restore_committed_install_plan(layout, backup)
    else:
        plan, _stats = _build_uninstall_plan(layout)
    if not plan:
        _remove_grok_relay_config()
        if not _skip_restart():
            start_cursor(layout)
        return 0

    close_cursor(layout)
    changed_extensions = _planned_extension_names(layout, plan)

    def validate() -> None:
        status = inspect_status(layout)
        if status.installed or status.external_marker_count:
            raise SandToolError(
                "卸载后仍有 Sand marker："
                f"{status.client_markers + status.eligibility_markers}，"
                f"external={status.external_marker_count}"
            )
        _verify_extension_hashes(layout, changed_extensions)
        _verify_product_checksums(layout)
        _mac_seal(layout)

    _commit_plan(layout, plan, "uninstall", validate)
    _remove_grok_relay_config()
    close_cursor(layout)
    if not _skip_restart():
        start_cursor(layout)
    return 0


def _permission_hint() -> str:
    script = Path(__file__).resolve()
    if sys.platform == "win32":
        return "请右键以管理员身份打开 PowerShell/终端后重新运行命令。"
    return f'请使用管理员权限重试：sudo python3 "{script}" <命令>'


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description=f"{TOOL_NAME} - Cursor 客户端模式管理器",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=(
            "示例：\n"
            "  python sand_stream_installer_tools_grokbot_box_relay_v136.py install\n"
            "  python sand_stream_installer_tools_grokbot_box_relay_v136.py provision-box\n"
            "  python sand_stream_installer_tools_grokbot_box_relay_v136.py uninstall\n"
            "  python sand_stream_installer_tools_grokbot_box_relay_v136.py set-path "
            "\"E:\\Development\\IDE\\cursor\"\n"
            "  python3 sand_stream_installer_tools_grokbot_box_relay_v136.py set-path "
            "/Applications/Cursor.app\n"
            "  python sand_stream_installer_tools_grokbot_box_relay_v136.py set-path auto"
        ),
    )
    parser.add_argument("--version", action="version", version=f"%(prog)s {TOOL_VERSION}")
    parser.add_argument(
        "--path",
        default="",
        help="显式指定 Cursor.app（也可通过 SAND_CURSOR_INSTALL_DIR 指定）",
    )
    commands = parser.add_subparsers(dest="command", required=True)
    commands.add_parser("status", help="只读检查当前 bundle 和 relay 配置状态")
    commands.add_parser("plan", help="只读生成安装计划和锚点统计")
    commands.add_parser(
        "install",
        help=(
            "安装 Grok Bot Box Relay Stream，Stream token 始终保留在 Box；"
            "保留 Task 模型、恢复、摘要与后台完成回调，覆盖 maxTokens 并放行全部模式与 Plan Build；"
            "可从 v1.2.7/v1.3.0 会话流或旧直连流原地迁移；"
            "识别 SandClaimer 卸载残留与其他工具标记"
        ),
    )
    commands.add_parser(
        "provision-box",
        help="检查并幂等初始化当前 Grok Bot 远程 Box 的 Stream relay",
    )
    commands.add_parser(
        "setup-all",
        help="一键：注册 Box relay + 注入 Cursor（带详细日志）",
    )
    commands.add_parser("uninstall", help="卸载 Sand 客户端模式")
    set_path = commands.add_parser("set-path", help="设置 Cursor 路径；auto 恢复自动检测")
    set_path.add_argument(
        "path",
        help="Cursor.exe、Cursor.app、resources/app、安装根目录，或 auto",
    )
    return parser


def collect_status_lines() -> List[Tuple[str, str]]:
    try:
        layout = resolve_cursor_layout()
        status = inspect_status(layout)
    except SandToolError as exc:
        return [(str(exc), ANSI_YELLOW)]

    lines: List[Tuple[str, str]] = [
        (f"[环境] Cursor {layout.version}  |  {layout.install_root}", ANSI_BLUE),
        (f"[适配] 支持 Cursor {SUPPORTED_CURSOR_VERSION_LABEL}", ANSI_GREEN),
    ]
    if not _cursor_version_supported(layout.version):
        lines.append(
            (
                f"[警告] 当前版本 {layout.version} 不受支持，请勿安装",
                ANSI_RED,
            )
        )
    if status.installed:
        if status.stream_mode_installed:
            lines.append(("[状态] Grok Bot Box Relay Stream + Subagent Lifecycle 修复模式已启用（token 留在 Box，含 Agent 窗口身份、Rules/Skills、User Rules、MCP 提示块）", ANSI_GREEN))
        elif (
            status.glass_client_markers == EXPECTED_GLASS_CLIENT_MARKERS
            and (
                status.rules_skills_markers == 0
                or status.mcp_filesystem_markers == 0
                or status.user_rules_markers == 0
            )
        ):
            lines.append(
                (
                    "[状态] 已启用 v1.2.8；运行安装可恢复 Rules/Skills、User Rules 与 MCP 提示块",
                    ANSI_YELLOW,
                )
            )
        elif (
            status.session_stream_markers >= 1
            and status.direct_stream_markers == 0
        ):
            lines.append(
                (
                    "[状态] 检测到 v1.2.7/v1.3.0 会话流；其 RunInference 入口会拒绝 Sand 流量。"
                    "运行安装可迁移到 Grok Bot Box Relay Stream 并补齐其余修复",
                    ANSI_YELLOW,
                )
            )
        elif (
            status.managed_subagent_session_markers == 1
            and status.legacy_managed_task_tool_markers == 1
            and status.managed_subagent_route_markers == 1
        ):
            lines.append(("[状态] 已启用 v1.2.5；运行安装可补齐模型/恢复/后台回调", ANSI_YELLOW))
        elif (
            status.direct_stream_markers >= 1
            and status.session_stream_markers == 0
        ):
            lines.append(
                (
                    "[状态] 检测到旧版直连流；运行安装可升级到当前 Grok Bot Box Relay Stream"
                    "（保留直连路径、覆盖 maxTokens、放行全部模式与 Plan Build）",
                    ANSI_YELLOW,
                )
            )
        else:
            lines.append(("[状态] 检测到旧版客户端模式", ANSI_YELLOW))
    else:
        lines.append(("[状态] Cursor 原版模式", ANSI_YELLOW))
    if status.external_marker_count:
        lines.append(
            (
                "[注意] 检测到其他工具的 Sand 标记："
                + _format_foreign_markers(status)
                + "；请先用该工具卸载，或恢复原始 3.18.9 文件后再安装",
                ANSI_YELLOW,
            )
        )
    if status.bare_header_sites:
        lines.append(
            ("[注意] " + _bare_header_notice(status.bare_header_sites), ANSI_YELLOW)
        )
    return lines


def print_banner() -> None:
    width = 60
    print(colorize("=" * width, ANSI_BLUE))
    print(colorize(f"  {TOOL_NAME}  v{TOOL_VERSION}", ANSI_BOLD, ANSI_GREEN))
    print(colorize("  Cursor managed-local Grok Bot Box 流代理 + 子代理生命周期兼容版（macOS/Windows）", ANSI_BOLD))
    print(colorize("=" * width, ANSI_BLUE))
    for text, code in collect_status_lines():
        print(colorize(text, code))
    print(colorize("[账号] 请使用订阅中已包含 Grok Bot 额度的 Cursor 账号", ANSI_YELLOW))
    print(colorize("[回滚] 启用后必须使用本修正版的 uninstall/选项 4 恢复", ANSI_YELLOW))
    print()


def apply_set_path(value: str) -> int:
    save_cursor_path(value)
    return 0


def print_menu() -> None:
    print(colorize("请选择操作：", ANSI_BOLD))
    print(colorize("  [1]", ANSI_BOLD, ANSI_GREEN) + " 一键：注册 Box relay + 注入 Cursor（带详细日志，推荐）")
    print(colorize("  [2]", ANSI_BOLD, ANSI_GREEN) + " 仅注入 Cursor（启用修复模式，会话流/旧直连流可原地迁移）")
    print(colorize("  [3]", ANSI_BOLD, ANSI_GREEN) + " 仅检查并初始化当前 Grok Bot 远程 Box Relay")
    print(colorize("  [4]", ANSI_BOLD, ANSI_GREEN) + " 恢复 Cursor 原版")
    print(colorize("  [5]", ANSI_BOLD, ANSI_GREEN) + " 指定 Cursor 路径")
    print(colorize("  [0]", ANSI_BOLD, ANSI_BLUE) + " 退出")


def prompt_set_path() -> int:
    value = input(colorize("路径> ", ANSI_BLUE)).strip()
    if not value:
        return 0
    with LoadingSpinner("正在设置路径"):
        result = apply_set_path(value)
    print_success("✓ Cursor 路径已保存")
    return result


def run_choice(choice: str) -> Optional[int]:
    if choice == "1":
        return provision_and_install()
    if choice == "2":
        with LoadingSpinner("正在启用 Grok Bot Box Relay Stream + Subagent Lifecycle 修复模式"):
            result = install(resolve_cursor_layout())
        print_success("✓ " + _restart_outcome("Grok Bot Box Relay Stream + Subagent Lifecycle 修复模式已启用"))
        print_warn("若提示 HTTP 404，请选 3 初始化当前 Box；其他失败请选 4 恢复。")
        return result
    if choice == "3":
        result = provision_box_relay(verbose=True)
        if result == "already-installed":
            print_success("✓ 当前 Grok Bot Box Relay 已存在，无需重复写入")
        elif result == "installed":
            print_success("✓ 当前 Grok Bot Box Relay 已初始化并通过路由探测")
        else:  # "provisioning"
            print_warn(
                "… Box 仍在后台改写 host 并重启，本轮未等到 200。"
                "过几分钟再跑一次本项即可（复用同一 agent，不重复发送/扣费）。"
            )
        print_warn("无需重新安装本地补丁，可直接在 Cursor 中重试请求。")
        return 0
    if choice == "4":
        with LoadingSpinner("正在恢复 Cursor 原版"):
            result = uninstall(resolve_cursor_layout())
        print_success("✓ " + _restart_outcome("已恢复 Cursor 原版"))
        return result
    if choice == "5":
        return prompt_set_path()
    print_warn("无效选项，请输入 0-5。")
    return 0


def interactive_loop() -> int:
    while True:
        print_banner()
        print_menu()
        try:
            choice = input(colorize("请输入编号> ", ANSI_BLUE)).strip()
        except EOFError:
            print()
            return 0
        if choice == "0":
            print(colorize("已退出。", ANSI_BLUE))
            return 0
        try:
            run_choice(choice)
        except PermissionError as exc:
            print_error(f"错误：没有写入权限：{exc}")
            print_error(_permission_hint())
        except SandToolError as exc:
            print_error(f"错误：{exc}")
        except KeyboardInterrupt:
            print()
            return 0
        except Exception as exc:
            print_error(f"未预期错误：{exc}")
        print()


def main(argv: Optional[Sequence[str]] = None) -> int:
    _configure_console()
    args_list = list(sys.argv[1:] if argv is None else argv)
    try:
        _platform_name()
        if not args_list:
            return interactive_loop()

        args = build_parser().parse_args(args_list)
        if args.path:
            os.environ["SAND_CURSOR_INSTALL_DIR"] = args.path
        if os.environ.get("CURSORCTL_SILENT", "") != "1":
            print_banner()
        if args.command == "set-path":
            result = apply_set_path(args.path)
            print_success("✓ Cursor 路径已保存")
            return result
        if args.command == "provision-box":
            result = provision_box_relay(verbose=True)
            if result == "already-installed":
                print_success("✓ 当前 Grok Bot Box Relay 已存在，无需重复写入")
            elif result == "installed":
                print_success("✓ 当前 Grok Bot Box Relay 已初始化并通过路由探测")
            else:  # "provisioning"
                print_warn(
                    "… Box 仍在后台改写 host 并重启，本轮未等到 200；"
                    "过几分钟重跑 provision-box 即可（复用同一 agent，不重复发送/扣费）。"
                )
            print_warn("无需重新安装本地补丁，可直接在 Cursor 中重试请求。")
            return 0
        if args.command == "setup-all":
            return provision_and_install()

        layout = resolve_cursor_layout()
        if args.command == "status":
            status = inspect_status(layout)
            print(json.dumps({
                "tool_version": TOOL_VERSION,
                "version": layout.version,
                "app_root": str(layout.app_root),
                "installed": status.installed,
                "stream_mode_installed": status.stream_mode_installed,
                "relay_config_present": _relay_config_path().is_file(),
                **asdict(status),
            }, ensure_ascii=False, default=str))
            return 0
        if args.command == "plan":
            status = inspect_status(layout)
            plan, stats = _build_install_plan(layout)
            print(json.dumps({
                "tool_version": TOOL_VERSION,
                "version": layout.version,
                "app_root": str(layout.app_root),
                "supported": _cursor_version_supported(layout.version),
                "installed": status.installed,
                "stream_mode_installed": status.stream_mode_installed,
                "relay_config_present": _relay_config_path().is_file(),
                "target_count": len(layout.target_paths),
                "plan_files": [
                    str(path.relative_to(layout.app_root)) for path in plan
                ],
                "stats": asdict(stats),
            }, ensure_ascii=False, default=str))
            return 0
        if args.command == "install":
            result = install(layout)
            print_success("✓ " + _restart_outcome("Grok Bot Box Relay Stream + Subagent Lifecycle 修复模式已启用"))
            print_warn("请测试 read/shell、显式模型 Task、同 ID resume、Summarize、后台 Task 回调、Plan Build 与 1M 上下文识别；失败请运行 uninstall 恢复。")
            return result
        if args.command == "uninstall":
            result = uninstall(layout)
            print_success("✓ " + _restart_outcome("已恢复 Cursor 原版"))
            return result
        raise SandToolError(f"未知命令：{args.command}")
    except PermissionError as exc:
        print_error(f"错误：没有写入权限：{exc}")
        print_error(_permission_hint())
        return 3
    except SandToolError as exc:
        print_error(f"错误：{exc}")
        return 2
    except KeyboardInterrupt:
        print_error("操作已取消。")
        return 130
    except Exception as exc:
        print_error(f"未预期错误：{exc}")
        return 1


_assert_known_markers_complete()


if __name__ == "__main__":
    raise SystemExit(main())

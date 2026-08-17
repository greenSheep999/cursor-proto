# Claude/Codex/Cursor 工具契约与 Anthropic 长上下文流规范

日期：2026-08-17

## 结论摘要

当前故障不能靠“再补几个固定工具名”彻底解决。Claude Code、Codex、Cursor 和 MCP 没有一套共享的系统级工具名称：

- Claude Code 的常用内置工具采用 `Glob`、`Grep`、`Read`、`Edit`、`Bash`、`WebSearch` 等大小写敏感名称；其可用集合会随版本、模型、模式、配置和延迟加载状态变化。
- Codex 的内置工具采用 `exec_command`、`apply_patch`、`update_plan`、`web_search`、`tool_search` 等小写名称，而且工具集合由运行模式和 feature flags 决定。
- Cursor 官方只公开“搜索文件、读取文件、编辑文件、运行 shell、浏览器”等能力，不公开稳定的原生 wire tool 名称；官方明确说明 Cursor 会针对每个模型调整指令和工具。
- MCP 工具名由各 MCP server 声明，规范明确要求按大小写敏感处理；它们不是 Cursor、Claude Code 或 Codex 的固定内置工具。
- Anthropic Messages API 的 client tool 名称也是调用方自定义字符串。Anthropic 不定义 `Glob` 或 `glob`；网关必须把上游工具事件恢复成当前请求中实际声明的名称和 schema。

因此，`No such tool available: glob` 最直接的协议解释是：调用方注册的是 `Glob`，但转换层输出了 `glob`，或者本轮请求根本没有向模型提供这个工具。正确方向是“请求作用域的工具目录 + 精确名称恢复 + schema 驱动参数映射”，而不是全局 lowercase 或永远增长的硬编码别名表。

另外，Anthropic 原生 WebSearch 与 Claude Code 的 `WebSearch` 是两种不同契约：

- Claude Code `WebSearch`：普通 client tool，CLI/宿主负责执行，模型返回 `tool_use`，下一轮由客户端发 `tool_result`。
- Anthropic `web_search_*`：server tool，Anthropic 在同一个请求内执行，返回 `server_tool_use` 和 `web_search_tool_result`，客户端不能等待本地执行，也不应补发 user `tool_result`。

长上下文故障同样不是 Claude 规定的行为。官方 SDK 默认请求超时为 10 分钟，没有“60 秒内必须出现首个 text/thinking/tool block”的协议承诺。`upstream produced no content before first-output timeout` 是网关策略超时，不是 Anthropic 终止原因。

## 1. 不存在跨宿主的“统一系统工具表”

工具调用实际有四层名字：

```text
模型看到的 tools[].name
        ↓
模型输出的 tool_use.name
        ↓
宿主路由表中的可执行工具名
        ↓
底层实现（本地函数、Cursor 原生工具、MCP、server tool）
```

只有前两项必须遵守 Anthropic Messages 的 client-tool 契约：模型输出的名称应对应本次请求声明的工具。后两项是宿主实现细节，可以完全不同，但转换层必须维护可逆映射。

Anthropic 官方 `ToolUseBlock.name` 类型只是任意字符串；工具定义由请求中的 `name`、`description` 和 `input_schema` 构成。[官方 ToolUseBlock](https://github.com/anthropics/anthropic-sdk-python/blob/ad53cac8eeeb1608c162081f883755427ac3a26f/src/anthropic/types/tool_use_block.py#L19-L29) 和 [Messages 工具定义/续轮示例](https://github.com/anthropics/anthropic-sdk-python/blob/ad53cac8eeeb1608c162081f883755427ac3a26f/src/anthropic/resources/messages/messages.py#L304-L379) 均没有规定 `Glob`、`Bash` 等固定名称。

MCP 规范更明确：工具通过 `tools/list` 动态发现，通过 `tools/call` 按服务器声明的 `name` 调用；名称应视为大小写敏感。[MCP Tools 规范](https://modelcontextprotocol.io/specification/2025-11-25/server/tools#tool-names)

实现结论：

1. 每次请求都要构建 `declaredToolsByExactName`，不能只看全局固定表。
2. 可以另建 normalized index 用于匹配 Cursor 原生事件，但最终输出必须恢复调用方原始名称。
3. exact match 优先；只有 exact 不存在时才允许做受控 alias/case-insensitive match。
4. alias 命中必须唯一；`read` 同时可能对应 `Read`、MCP 的 `read`、自定义 `read_file` 时不能猜。
5. 参数转换必须以目标工具的入站 `input_schema` 为准，并对转换结果做 schema 验证。
6. 若无法唯一映射，应在 HTTP 尚未提交时重试兼容路由，或返回明确错误；不能制造一个调用方未声明的工具名。

## 2. Claude Code / Claude Agent SDK 的工具契约

### 2.1 名称和 casing

Anthropic 发布的 `@anthropic-ai/claude-agent-sdk` 0.3.233 包含自动生成的 Claude CLI tool schemas。常见工具名称及对应输入包括：

| Claude Code 名称 | 关键输入字段 |
|---|---|
| `Bash` | `command`, `timeout?`, `description?`, `run_in_background?`, `dangerouslyDisableSandbox?` |
| `Read` | `file_path`, `offset?`, `limit?`, `pages?` |
| `Edit` | `file_path`, `old_string`, `new_string`, `replace_all?` |
| `Write` | `file_path`, `content` |
| `Glob` | `pattern`, `path?` |
| `Grep` | `pattern`, `path?`, `glob?`, `output_mode?`, `-A?`, `-B?`, `-C?`, `-i?`, `-n?`, `-o?`, `type?`, `head_limit?`, `offset?`, `multiline?` |
| `NotebookEdit` | `notebook_path`, `cell_id?`, `new_source`, `cell_type?`, `edit_mode?` |
| `WebFetch` | `url`, `prompt` |
| `WebSearch` | `query`, `allowed_domains?`, `blocked_domains?` |
| `AskUserQuestion` | `questions`（结构化问题数组） |
| `TodoWrite` | `todos` |
| `TaskCreate` | `subject`, `description`, `activeForm?`, `metadata?` |
| `TaskGet` | `taskId` |
| `TaskUpdate` | `taskId` 及更新字段 |
| `TaskList` | 空对象 |

一手来源：[@anthropic-ai/claude-agent-sdk 0.3.233 的生成 schema](https://unpkg.com/@anthropic-ai/claude-agent-sdk@0.3.233/sdk-tools.d.ts) 和 [SDK options 类型](https://unpkg.com/@anthropic-ai/claude-agent-sdk@0.3.233/sdk.d.ts)。

这些名称大小写是宿主路由键的一部分。Claude Agent SDK 甚至提供 `toolAliases`，专门在模型输出名称与实际宿主工具不同时做单跳重定向；官方示例是把 `Bash` 映射为 `mcp__workspace__bash`。这直接证明名称适配应发生在宿主/网关边界，而不是假设所有 CLI 共用名称。[SDK `toolAliases` 文档](https://unpkg.com/@anthropic-ai/claude-agent-sdk@0.3.233/sdk.d.ts)

### 2.2 工具集合是动态的，不能硬编码“全量清单”

SDK 的 `tools` option 支持：

- 指定字符串数组，如 `['Bash', 'Read', 'Edit']`；
- 空数组，禁用所有 built-in tools；
- `{type:'preset', preset:'claude_code'}`，使用默认 Claude Code 工具。

同一份官方类型还说明，native build 可能使用 Bash 的 `find`/`grep` 而不提供独立 `Grep`/`Glob`；若调用方需要它们，应在 `tools` 或 `allowedTools` 明确列出。[SDK options](https://unpkg.com/@anthropic-ai/claude-agent-sdk@0.3.233/sdk.d.ts)

Claude Code 自身也在持续改变可用工具。例如 2.1.231 的 changelog 说明，新模型默认不再提供 Todo/task-tracking 工具，需 `CLAUDE_CODE_ENABLE_TODO_TOOLS=1` 才恢复；历史版本也多次修复 deferred tools、ToolSearch、schema 丢失和 compaction 后工具不可用的问题。[Claude Code 官方 changelog](https://github.com/anthropics/claude-code/blob/ae58f7a0a23f0fddedd668de74622ff46646526b/CHANGELOG.md)

因此所谓“覆盖全”应实现为：

```text
透传并登记当前请求全部 tools[]
        +
动态接收 ToolSearch/MCP 后加载的新 schema
        +
按本轮声明精确恢复名称
```

而不是在网关维护一个永远不可能完整的 Claude Code 工具枚举。

### 2.3 Claude Code deferred tools / ToolSearch

Claude Agent SDK 的 MCP server 配置默认可在启用 tool search 时延迟工具 schema；`alwaysLoad` 等价于 `defer_loading:false`，会让工具从第一轮就进入 prompt。SDK 还把 `deferredBuiltinTools` 和 `mcpTools.isLoaded` 作为上下文统计的一部分公开。[SDK deferred/alwaysLoad 类型](https://unpkg.com/@anthropic-ai/claude-agent-sdk@0.3.233/sdk.d.ts)

Claude Code changelog 记录了以下一手行为：

- deferred tools 通过 `ToolSearch` 加载；
- compaction 后曾出现已加载工具 schema 丢失；
- 自定义 gateway 上 ToolSearch 需要正确的 feature detection；
- 工具 schema 在 prompt 尾部注入曾导致空回复；
- MCP server 晚连接时 ToolSearch 需要刷新可见工具。

来源：[Claude Code 官方 changelog](https://github.com/anthropics/claude-code/blob/ae58f7a0a23f0fddedd668de74622ff46646526b/CHANGELOG.md)

网关必须保持下面两种状态不同：

```text
registered but deferred     # 已注册，暂未暴露给模型
loaded/model-visible        # schema 已在当前 turn 提供
```

如果模型只看到了工具引用/描述，却没有拿到 schema，或网关在 compaction/重试后丢失 loaded 状态，就会产生“模型尝试调用，但 CLI 报 No such tool”或参数 schema 错误。

## 3. Codex / OpenAI CLI 工具契约

Codex 的工具名不是 Claude Code 的别名集。OpenAI 官方 Codex 源码当前注册的典型名称包括：

- `exec_command` / `write_stdin`（unified exec 模式）；
- `shell_command`（另一 shell 模式，可注册但对模型隐藏）；
- `apply_patch`；
- `update_plan`；
- `request_user_input`；
- `view_image`；
- `list_mcp_resources`；
- `read_mcp_resource`；
- `web_search`；
- `tool_search`。

具体来源：

- [`exec_command` 和 `shell_command` specs](https://github.com/openai/codex/blob/c6058ccaa91ab17159cf805bf4d6d4edd87fe5fc/codex-rs/core/src/tools/handlers/shell_spec.rs)
- [`apply_patch`](https://github.com/openai/codex/blob/c6058ccaa91ab17159cf805bf4d6d4edd87fe5fc/codex-rs/core/src/tools/handlers/apply_patch_spec.rs)
- [`update_plan`](https://github.com/openai/codex/blob/c6058ccaa91ab17159cf805bf4d6d4edd87fe5fc/codex-rs/core/src/tools/handlers/plan_spec.rs)
- [MCP resource tools](https://github.com/openai/codex/blob/c6058ccaa91ab17159cf805bf4d6d4edd87fe5fc/codex-rs/core/src/tools/handlers/mcp_resource_spec.rs)
- [`request_user_input`](https://github.com/openai/codex/blob/c6058ccaa91ab17159cf805bf4d6d4edd87fe5fc/codex-rs/core/src/tools/handlers/request_user_input_spec.rs)
- [工具可见性/feature 组合测试](https://github.com/openai/codex/blob/c6058ccaa91ab17159cf805bf4d6d4edd87fe5fc/codex-rs/core/src/tools/spec_plan_tests.rs)

Codex 并不要求存在 Claude Code 式的 `Glob`、`Read` 或 `Grep`；相同能力可能通过 `exec_command`/shell 完成。把 Cursor 的原生 glob 一律输出为 `Glob` 会破坏 Codex；一律输出为 `glob` 同样会破坏 Claude Code。

Codex app-server 还支持 dynamic tools。动态函数名允许调用方定义；设置 `deferLoading:true` 后仍注册但不进入普通 turn 的模型工具列表，并在 `tool_search` 命中后暴露。[Codex app-server dynamic tool contract](https://github.com/openai/codex/blob/c6058ccaa91ab17159cf805bf4d6d4edd87fe5fc/codex-rs/app-server/README.md#dynamic-tool-calls-experimental)

因此 OpenAI/Codex 路径也需要“以请求声明为准”的动态目录，而不能复用 Claude Code canonical names。

## 4. Cursor 原生工具与 MCP 工具的边界

Cursor 官方 Agent 文档列出的能力是：搜索文件和目录、读取文件、编辑文件、执行 shell、浏览器、图像生成等；它同时明确说明 Cursor 会针对每一个受支持模型协调组件并调整 instructions/tools。[Cursor Agent tools](https://cursor.com/docs/agent/tools)

这个公开契约描述的是能力，不是稳定 wire schema。它没有承诺原生工具在不同 Cursor 版本、模型路由或 Agent 模式中使用固定名称。因此：

- Cursor 原生 `glob/find/search` 事件只能视为“能力标签”；
- 对外名称必须从调用方 tools catalog 恢复；
- Cursor 参数如 `glob_pattern`、`working_directory` 等需要映射到目标 schema 的 `pattern`、`path` 等字段；
- 映射完成后必须按调用方 JSON Schema 校验，不能仅做字段重命名后盲目输出。

Cursor MCP 是另一条路径。官方文档说明 MCP server 通过 `stdio`、SSE 或 Streamable HTTP 提供 tools/resources/prompts；工具由 server 暴露，Cursor 作为 MCP host 调用。[Cursor MCP 文档](https://cursor.com/docs/context/mcp)

MCP 规范要求：

- 客户端通过 `tools/list` 获取每个工具的 `name`、`description`、`inputSchema`、可选 `outputSchema`；
- 通过 `tools/call` 原样传递 `name` 和 `arguments`；
- 工具名按大小写敏感处理；
- 执行错误在结果中用 `isError:true` 表示；
- `structuredContent` 是 MCP server 输出，不等同于 LLM structured output。

来源：[MCP Tools 规范](https://modelcontextprotocol.io/specification/2025-11-25/server/tools)

结论：Cursor native tool、Claude Code built-in tool 和 MCP tool 必须保留不同 namespace/identity。MCP 名称尤其不能经过 lowercase、去前缀或“canonical Claude 工具名”归一化。

## 5. Anthropic client tool 的标准流和续轮

标准 client-tool SSE：

```text
message_start

content_block_start(index=N)
  content_block={type:"tool_use", id:"toolu_...", name:"Glob", input:{}}

content_block_delta(index=N)
  delta={type:"input_json_delta", partial_json:"{\"pat"}

content_block_delta(index=N)
  delta={type:"input_json_delta", partial_json:"tern\":\"**/*.go\"}"}

content_block_stop(index=N)
message_delta(stop_reason="tool_use", usage=累计值)
message_stop
```

官方 fixture 展示了 `input_json_delta.partial_json` 可以为空、可以在任意字符处拆分。客户端/网关必须按 block index 逐字拼接，并在 `content_block_stop` 后才解析 JSON。[官方 tool-use SSE fixture](https://github.com/anthropics/anthropic-sdk-python/blob/ad53cac8eeeb1608c162081f883755427ac3a26f/tests/lib/streaming/fixtures/tool_use_response.txt#L1-L44)；[TypeScript SDK accumulator](https://github.com/anthropics/anthropic-sdk-typescript/blob/64a1e8e285bbcc4cef2b15ebcadccd8e5f6987ff/src/lib/MessageStream.ts#L633-L674)

工具执行后，下一轮必须：

1. 保留上一轮 assistant content，包括原始 `tool_use` id/name/input；
2. 新增 user content；
3. 把对应 `tool_result` 放在该 user content 数组开头；
4. `tool_use_id` 与调用 ID 精确匹配；
5. 并行工具调用返回所有对应结果；
6. 工具失败使用 `is_error:true`。

```json
{
  "role": "user",
  "content": [
    {
      "type": "tool_result",
      "tool_use_id": "toolu_123",
      "content": "...",
      "is_error": false
    }
  ]
}
```

来源：[ToolResultBlockParam](https://github.com/anthropics/anthropic-sdk-python/blob/ad53cac8eeeb1608c162081f883755427ac3a26f/src/anthropic/types/tool_result_block_param.py#L22-L32) 和 [官方续轮示例](https://github.com/anthropics/anthropic-sdk-typescript/blob/64a1e8e285bbcc4cef2b15ebcadccd8e5f6987ff/src/resources/messages/messages.ts#L3284-L3353)。

## 6. Anthropic 原生 WebSearch 标准流

### 6.1 请求版本

当前官方 SDK 定义三种 WebSearch 工具版本，名称都必须为小写 `web_search`：

```text
type=web_search_20250305, name=web_search
type=web_search_20260209, name=web_search
type=web_search_20260318, name=web_search
```

来源：[Anthropic SDK 当前生成类型](https://github.com/anthropics/anthropic-sdk-typescript/blob/64a1e8e285bbcc4cef2b15ebcadccd8e5f6987ff/src/resources/messages/messages.ts#L2831-L2975)。

如果 parser 只识别早期的一两个版本，新的 scorer/client 会被误判成普通 client tool，表现就是 WebSearch 失败。兼容层应识别所有官方已知版本；长期实现可按 `type` 的 `web_search_` 前缀加 `name==web_search` 分类，同时对未知版本保留严格校验和可观测告警。

### 6.2 完整流序列

```text
message_start

content_block_start(index=0)
  server_tool_use(id=srvtoolu_..., name=web_search, input={})
content_block_delta(index=0)
  input_json_delta(partial_json=查询参数片段)
content_block_stop(index=0)

content_block_start(index=1)
  web_search_tool_result(
    tool_use_id=同一个 srvtoolu_...,
    content=[web_search_result...]
  )
content_block_stop(index=1)

content_block_start(index=2, type=text)
  text_delta / citations_delta ...
content_block_stop(index=2)

message_delta(
  stop_reason=end_turn,
  usage.server_tool_use.web_search_requests=1
)
message_stop
```

官方 wire fixture：[server_tool_use_response.txt](https://github.com/anthropics/anthropic-sdk-python/blob/ad53cac8eeeb1608c162081f883755427ac3a26f/tests/lib/streaming/fixtures/server_tool_use_response.txt#L1-L56)。请求 schema：[WebSearchTool20250305](https://github.com/anthropics/anthropic-sdk-python/blob/ad53cac8eeeb1608c162081f883755427ac3a26f/src/anthropic/types/web_search_tool_20250305_param.py#L15-L59)。结果 schema：[WebSearchToolResultBlock](https://github.com/anthropics/anthropic-sdk-python/blob/ad53cac8eeeb1608c162081f883755427ac3a26f/src/anthropic/types/web_search_tool_result_block.py#L20-L28)。

关键区别：

- `server_tool_use` 的完整 input 从后续 `input_json_delta` 拼出；start frame 常为 `input:{}`。
- `web_search_tool_result` 的完整结果通常直接位于 `content_block_start.content_block`，不是用 `input_json_delta` 拆分。
- `tool_use_id` 必须匹配 server tool id。
- 客户端不执行、不发送下一轮 user `tool_result`。
- usage 必须带实际 `web_search_requests`。

## 7. Anthropic 标准 Messages SSE 状态机

成功流必须有一个且只有一个消息信封：

```text
message_start                         # exactly once
  content_block_start(index=0)
  content_block_delta(index=0) × N
  content_block_stop(index=0)
  content_block_start(index=1)
  ...
message_delta                         # final stop reason + cumulative usage
message_stop                          # exactly once
```

Anthropic TypeScript SDK 明确拒绝：

- `message_stop` 前第二次 `message_start`；
- `message_start` 前出现语义事件；
- block index/生命周期不合法的 delta/stop。

来源：[MessageStream 状态校验](https://github.com/anthropics/anthropic-sdk-typescript/blob/64a1e8e285bbcc4cef2b15ebcadccd8e5f6987ff/src/lib/MessageStream.ts#L558-L574) 和 [Messages Streaming 文档](https://platform.claude.com/docs/en/build-with-claude/streaming)。

### 7.1 Thinking 和 signature

标准 thinking block：

```text
content_block_start(type=thinking)
thinking_delta × N
signature_delta
content_block_stop
```

`signature_delta` 在对应 thinking block 的 stop 前到达。签名是 opaque value：不得生成、截断、解码、格式化或跨 block 移动。使用 thinking + tools 多轮续接时，thinking blocks、原始顺序和 signature 必须原样回传；修改会得到 `400 invalid_request_error`。

来源：[SignatureDelta](https://github.com/anthropics/anthropic-sdk-python/blob/ad53cac8eeeb1608c162081f883755427ac3a26f/src/anthropic/types/signature_delta.py#L10-L18)、[ThinkingBlockParam](https://github.com/anthropics/anthropic-sdk-python/blob/ad53cac8eeeb1608c162081f883755427ac3a26f/src/anthropic/types/thinking_block_param.py#L10-L23) 和 [Extended Thinking](https://platform.claude.com/docs/en/build-with-claude/extended-thinking)。

签名“有时全合格、有时部分合格”应按路径分组排查：stream/non-stream、普通文本/client tool/server tool、首次 turn/tool_result continuation、不同模型路由。最可能的协议问题是某条分支丢失、重排或合成了 `signature_delta`，而不是签名内容本身随机失效。

### 7.2 ping 和 stream error

`ping` 是传输保活：官方 SDK直接忽略；它不创建 block、不增加 usage、不改变 stop reason，也不算首个语义输出。

HTTP 200 之后仍可能出现正式 SSE `event:error`，官方 SDK会抛错。不能把它转换成“成功空消息”。[TypeScript SDK ping/error 处理](https://github.com/anthropics/anthropic-sdk-typescript/blob/64a1e8e285bbcc4cef2b15ebcadccd8e5f6987ff/src/core/streaming.ts#L135-L143)；[Python SDK 同等处理](https://github.com/anthropics/anthropic-sdk-python/blob/ad53cac8eeeb1608c162081f883755427ac3a26f/src/anthropic/_streaming.py#L151-L167)

`stop_reason:"error"` 不是合法 Anthropic stop reason。合法值包括：

```text
end_turn
max_tokens
stop_sequence
tool_use
pause_turn
refusal
model_context_window_exceeded
```

来源：[Message stop reason](https://github.com/anthropics/anthropic-sdk-python/blob/ad53cac8eeeb1608c162081f883755427ac3a26f/src/anthropic/types/message.py#L81-L97)。

已经提交 HTTP/SSE envelope 后遇到真实上游失败，不应伪造成 `end_turn` 或非法 `error` 的成功消息；应发送标准 Anthropic `event:error`（若仍可写）并关闭。尚未提交下游响应时，才可以干净地 failover/retry。

### 7.3 `pause_turn`

`pause_turn` 是长运行 server turn 的协议状态。调用方应把当前 assistant 响应原样放回下一次请求继续，而不是把它当成空返回、tool_use 或终局错误。[Message stop reason 文档](https://github.com/anthropics/anthropic-sdk-python/blob/ad53cac8eeeb1608c162081f883755427ac3a26f/src/anthropic/types/message.py#L81-L97)

## 8. 长上下文和首输出超时

Anthropic Python SDK 默认请求超时是 10 分钟；预计超过 10 分钟的操作要求使用 streaming。[默认超时](https://github.com/anthropics/anthropic-sdk-python/blob/ad53cac8eeeb1608c162081f883755427ac3a26f/src/anthropic/_constants.py#L8-L10)；[长操作需 streaming](https://github.com/anthropics/anthropic-sdk-python/blob/ad53cac8eeeb1608c162081f883755427ac3a26f/src/anthropic/_base_client.py#L769-L782)

Anthropic 没有承诺：

- 长 prompt/prefill 在 60 秒内一定产生 text/thinking/tool block；
- 固定频率一定会收到 ping；
- ping 等价于模型开始输出。

Claude Code 官方 changelog 也记录过“长模型 thinking pause 被误判 Stream idle timeout”以及后台/远程 session 的同类修复，说明宿主必须容忍长时间无语义 block。[Claude Code changelog](https://github.com/anthropics/claude-code/blob/ae58f7a0a23f0fddedd668de74622ff46646526b/CHANGELOG.md)

正确的 timeout 分层至少包括：

| 阶段 | 应监控内容 | 建议语义 |
|---|---|---|
| connect timeout | TCP/TLS/HTTP headers | 较短，可 failover |
| upstream first-byte timeout | 任意上游字节 | 独立于模型输出 |
| envelope timeout | Anthropic/Cursor 消息开始 | 失败前不得提交半个下游流 |
| first semantic block timeout | thinking/text/tool/server-tool | 必须远长于当前短 deadline，并随 prompt size/route 调整 |
| inter-event idle timeout | 上游任何活动，包括有效 heartbeat | sliding timeout |
| total request deadline | 整体 wall clock | 分钟级，明确可配置 |

`upstream produced no content before first-output timeout` 应视为网关可观测错误，不应包装成 Claude 的空 `end_turn`。真实 context 超限应保留 `model_context_window_exceeded`；真正 server-side 长任务暂停应保留 `pause_turn`。

对 account failover：

```text
未向客户端提交 message_start：可切账号，重新创建完整单一流
已向客户端提交 message_start：不可把第二账号的 message_start 拼接进来
```

## 9. usage/token 语义

Anthropic `message_delta.usage` 是累计快照，不是本事件增量。官方 SDK采用覆盖而不是相加。[MessageDeltaUsage](https://github.com/anthropics/anthropic-sdk-python/blob/ad53cac8eeeb1608c162081f883755427ac3a26f/src/anthropic/types/message_delta_usage.py#L12-L35)；[TypeScript accumulator](https://github.com/anthropics/anthropic-sdk-typescript/blob/64a1e8e285bbcc4cef2b15ebcadccd8e5f6987ff/src/lib/MessageStream.ts#L575-L605)

字段语义：

- `input_tokens`：普通输入 token；
- `cache_creation_input_tokens`：用于创建缓存的输入 token；
- `cache_read_input_tokens`：从缓存读取的输入 token；
- `output_tokens`：权威的累计输出 token。

按照公开协议，不能做全局：

```text
output_tokens -= cache_creation_input_tokens + cache_read_input_tokens
```

缓存字段属于输入，output 是独立计数。如果 Cursor 私有上游某个字段确实偶尔混合了这些计数，只能在 Cursor adapter 边界依据已验证的上游字段语义修正，并记录 route/version/provenance；不能根据数值“看起来可相减”做跨协议启发式归一化。

## 10. 对当前故障的可执行诊断顺序

### P0：`glob` / 普通工具调用

对同一个 request-id 记录（敏感参数只记录 schema/hash，不记录内容）：

```text
inbound declared tools: exact name + normalized name + schema hash
Cursor request catalog: exposed tool kind/name + schema hash
Cursor response: native tool kind + raw arguments
adapter match: exact / alias / casefold / ambiguous / missing
outbound Anthropic: tool_use.id + exact name + argument keys
next inbound turn: tool_result.tool_use_id + result position
```

验收条件：入站声明 `Glob` 时，对外只能出现 `Glob`；入站只声明 `glob` 时，对外只能出现 `glob`。两者都声明时不得 casefold 猜测。

### P0：WebSearch

分别测试：

1. 普通 client tool `{name:"WebSearch", input_schema:...}`，期望 `tool_use` + 下一轮 `tool_result`；
2. server tool `{type:"web_search_20250305",name:"web_search"}`；
3. `web_search_20260209`；
4. `web_search_20260318`。

server-tool 每个版本都要断言：

- exactly one `message_start`/`message_stop`；
- `server_tool_use.name==web_search`；
- input 由 `input_json_delta` 拼出；
- result ID 与 server tool ID 相等；
- result 完整位于 start block；
- 有最终 text/citations（若模型产生）；
- usage 中 `web_search_requests>=1`。

### P0：长上下文

用同一模型、同一工具表、同一问题，逐级增加 system/history 长度，并记录：

```text
t_headers
t_upstream_first_byte
t_upstream_message_start/transport-start
t_first_ping
t_first_thinking
t_first_tool
t_first_text
t_message_delta
t_message_stop
```

每个计时点必须区分“没有事件”和“事件被 converter 暂存未下发”。测试需覆盖首个语义 block 超过当前 timeout 后仍最终成功的情况。

### P1：签名

建立以下矩阵并保存 block-level trace：

```text
stream / non-stream
text / client tool / server WebSearch
first turn / tool_result continuation
thinking adaptive / enabled
各实际 Cursor model route
```

每个 thinking block 断言：`thinking_start → thinking_delta* → signature_delta → block_stop`，signature 非空、未改写，续轮 assistant 历史原样包含该 block。

### P1：Codex 与 MCP

- Codex 请求只声明 `exec_command` 时，Cursor shell 事件必须恢复为 `exec_command`，不能输出 `Bash`。
- Claude Code 请求只声明 `Bash` 时，同一 Cursor shell 事件必须恢复为 `Bash`。
- MCP 工具 exact name/casing 和 server identity 必须全程不变。
- deferred tool 未加载时不可假装已可用；ToolSearch/工具列表变更后更新本轮/下一轮 catalog。

## 11. 推荐的兼容层设计

```text
IncomingToolCatalog
  ├─ ClientTool{name, exactName, schema, source=caller}
  ├─ AnthropicServerTool{typeVersion, name=web_search, source=anthropic}
  ├─ MCPTool{serverIdentity, exactName, schema, source=mcp}
  └─ DeferredTool{identity, loaded=false}

CursorNativeEvent
  └─ capabilityKind + rawArgs

RequestScopedResolver
  1. exact identity/name match
  2. source-aware native capability mapping
  3. unique schema-compatible alias match
  4. validate transformed args against target schema
  5. emit target exactName unchanged
  6. ambiguous/missing => observable error, never guessed lowercase name
```

这一设计可以同时支持：

- Claude Code 的 `Glob`/`Bash`；
- Codex 的 `exec_command`/`apply_patch`；
- 任意第三方 client tools；
- MCP 动态工具；
- Claude ToolSearch/Codex tool_search 延迟加载；
- Anthropic 原生 WebSearch 的专用 server-tool 流。

## 12. 最重要的五条修复判据

1. 对外 `tool_use.name` 必须来自当前请求声明，保留原始大小写；Cursor native 名称不能直接泄漏为协议工具名。
2. WebSearch 必须先区分 uppercase client tool 与 versioned lowercase Anthropic server tool，并覆盖当前三个官方版本。
3. 长上下文不能由单一短 first-content wall timer 判死；连接、活动、首语义 block、idle 和总 deadline 要拆开。
4. thinking/signature 必须按 block 原样转发并在工具续轮中原样回传；不同路径不得合成或省略。
5. Anthropic `output_tokens` 是独立权威输出计数；缓存输入字段不得被启发式全局相减。


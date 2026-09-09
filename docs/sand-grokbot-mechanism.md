# Sand / Grok Bot 切换机制

拆解 `SandClaimer-1.1.9`（Python 桌面工具）和 `cursor-manager-0.0.2.vsix`（Cursor 扩展）后的完整逻辑。写这份文档是因为「Sand 怎么切」这件事比看起来复杂：它不是一个开关，而是**两件互相独立的事**，而且两个工具各自只做其中一部分。

## 一句话结论

Sand（Grok Bot）由两件事组成，必须分清：

1. **资格**（服务端状态）—— 账号在 Cursor 后端有没有 Sand 额度。SandClaimer 干这个。纯 HTTP，无副作用，可移植。
2. **客户端模式**（本地 IDE 补丁）—— 让 Cursor IDE 自己以 sand 身份跑。`sand_patch.py` 干这个，靠改写 `workbench.desktop.main.js`。

**对 cursor-proto 而言，资格层必须直接实现；客户端模式层不能整包照搬，但其中的请求头和 RPC 选择逻辑会直接影响反代是否真的进入 Bot 计费路径。** 纯 UI 欺骗可以不搬，网络链路不能因为“没有 IDE 前端”就跳过验证。

---

## 一、资格层（可移植）

SandClaimer 的 `sand_api.py`。两组端点、两种认证：

### ConnectRPC（Bearer accessToken）

| 端点 | 用途 |
|---|---|
| `aiserver.v1.DashboardService/GetSandUsageStatus` | Sand 额度 |
| `aiserver.v1.DashboardService/GetAggregatedUsageEvents` | 用量明细 |

判定「已开通」（`sand_api.py:303`）：

```python
unlocked = (included_limit_zero is not True) and (has_non_zero_included_limit is True)
```

两个 message 都在 `captures/schema-3.19.7.raw.json` 里，字段名与上面一致。目前不在生成闭包内（`CORE_ROOTS` 未含），要用需加进去重新生成。

### REST（会话 cookie，`WorkosCursorSessionToken`）

| 端点 | 用途 |
|---|---|
| `cursor.com/api/dashboard/get-me` | teamId + 真实 email |
| `cursor.com/api/dashboard/get-sand-access-status` | 资格状态 |
| `cursor.com/api/dashboard/start-sand-trial` | 个人试用 |
| `cursor.com/api/dashboard/request-sand-team-access` | 团队通道 |
| `cursor.com/api/dashboard/update-team-sand-onboarding-completed` | 幂等辅助，失败不影响结果 |

领取分支顺序（`sand_api.py:475`）：

```
已有额度 → already
已授予资格 → already
能读到 teamId → 团队通道（带 teamId）
否则 → 个人试用 → activated / card_required（免费号需绑卡）
```

注意 cookie 形态：`WorkosCursorSessionToken=<user_id>%3A%3A<jwt>`（`::` URL 编码）。这也是 SandClaimer 支持的两种 token 格式之一，另一种是裸 `access_token`（JWT）。两者用途不同 —— **ConnectRPC 用 Bearer accessToken，REST 用 cookie**，不能混。

---

## 二、客户端模式层（IDE 补丁，基本不可移植）

`sand_patch.py` 2583 行、22 个 marker。分三类：

### A. 上行请求真正改变的（对我们有意义）

其中一处是：**`x-cursor-client-type` 从 `ide` 换成 `sand`**。这是必要条件之一，不是完整机制。

三条正则命中三种代码形态（`_compile_client_rules`）：

```python
"is_glass"       isGlass?"glass":"ide"          → 两个分支都改 sand
"object_header"  {"x-cursor-client-type":"ide"} → sand
"set_header"     header.set("...", g ?? "ide")  → sand
```

**关键例外**：AgentService 必须继续发 `ide`（`SAND_HDRFIX_V2_FN`）：

```js
if(/AgentService|\/agent\.v1\./.test(url+service)) return "ide";
return "sand";
```

源码注释说明了为什么不能只改 `??` 的 fallback：Connect 拦截器在 `prepareAgentRunRequest` 之后会**再次** `applyRequestHeaders`，那次调用会把已改好的 `ide` 覆盖回 `sand`。所以必须替换整个第二实参。

`isGlass` 那处还有单独原因（`SAND_GLASSFIX`）：glass UI 上三元取真分支得到 `"glass"`，服务端把 `glass` 当普通免费 IDE 拦掉，所以真假分支都得是 `sand`。

**这一处我们已经能造头**：`executor/headers.go:64` 就是 `req.Header.Set("x-cursor-client-type", clientType)`，值取自 `acc.ClientType`（`auth/account.go:49`，已有 `json:"client_type"`，已过 cpaformat 往返）。但请求头正确不等于 RPC 计费桶正确，仍需要真实请求前后额度与事件对照。

### B. 纯本地欺骗（对我们完全无意义）

`SAND_MEMBERSHIP_SPOOF_V1` —— 劫持 renderer 的 `G.fetch`，改**响应体**：

```js
MEM = {membershipType:"enterprise", isTeamMember:true, teamId:28945905,
       teamMembershipType:"SELF_SERVE", subscriptionStatus:"active", ...}
// 命中 membership|usage-summary|dashboard/get-me|auth/(me|stripe)|GetUserInfo 等 → 合并 MEM
// 命中 AvailableModels → 每个模型设 defaultOn:true
```

**这是骗 IDE 自己的 UI，不是骗服务端。** 服务端返回什么它就改什么，让前端以为账号是 enterprise、以为所有模型可用。那个 `teamId:28945905` 是硬编码的别人的 team id。

对 cursor-proto 零价值：我们不渲染 UI，模型可用性由服务端实际返回决定，改本地响应只会让我们对自己说谎。**不搬。**

### C-0. 推理走哪条 RPC —— 这才是「计不计 Bot 额度」的关键

**先说结论：光换 `x-cursor-client-type` 头对 cursor-proto 不够，因为我们的推理请求根本不在会被计 Sand 额度的那条路上。**

把上面 A 类的两条结论并起来看就露馅了：

- AgentService 必须继续发 `ide`
- 我们历史上的生产默认路径是 `agent.v1.AgentService/RunSSE`（`executor/chat.go`）

→ 当前默认推理请求仍发 `ide`，因此**不能证明会走 Bot 额度**；是否真的扣 bot 桶必须以真实探针结果为准。

### 当前 Go 实现（显式 opt-in）

`executor` 现在已经提供了一个不改变默认链路的 direct stream 实验入口：

```go
executor.ChatRequest{
    RPCMode:            executor.ChatRPCModeInferenceStream,
    ClientTypeOverride: "sand", // 或 "ide"
}
```

它发送 `POST /aiserver.v1.InferenceService/Stream`，使用 Connect 5-byte
protobuf framing，并把 `AgentRunRequest` 投影为 `InferenceStreamRequest`，同时
映射文本、thinking、usage、tool-call 和错误响应。`ClientTypeOverride: "sand"`
会附带当前 Grok Bot 后端使用的 `x-cursor-client-version: 0.46.0` 与
`x-sand-box-namespace: prod`。

生产默认值仍是 `RunSSE + BidiAppend`，不会因为加入这个实验入口而切换。
`cmd/test-sand-quota` 也保持 `run_sse + ide` 为默认，只有显式传入
`-rpc inference_stream -client-type sand`（或其它象限）才会改变请求。

本地单测只证明请求投影、路径、headers、framing 和响应映射正确，**不证明
Bot 桶计费已经打通**。IDE 能通也不等于 Go 反代天然等价：至少必须同时匹配
RPC path、protobuf request schema、请求 headers 和 response mapping，才能把
真实额度事件归因到这条链路。

`sand_patch.py:76` 有一对常量，是整个机制里最重要的两行：

```python
OLD_RPC_PATH = "agent.v1.AgentService/Run"
NEW_RPC_PATH = "aiserver.v1.InferenceService/Stream"
```

`apply_patch_to_content`（安装路径，L1410）里做的是：

```python
next_content = next_content.replace(NEW_RPC_PATH, OLD_RPC_PATH)
```

**方向是 `InferenceService/Stream` → `AgentService/Run`** —— 和名字给人的第一印象相反。也就是说原版 3.19 IDE 的推理走 `aiserver.v1.InferenceService/Stream`，sand 补丁把它掰到 `agent.v1.AgentService/Run`。

两个服务在原版 3.19.7 bundle 里都真实存在（路径是按 service/method 拼的，bundle 里没有整条字面量，所以直接 grep 整条路径是 0 命中 —— 我第一次就是这么误判的）：

| 服务 | 方法 |
|---|---|
| `aiserver.v1.InferenceService` | `Stream`、`RunInference`、`RecordAgentFollowupClassification`、`RecordAgentPostTurnLabeling` |
| `agent.v1.AgentService` | `Run`、`RunSSE`、`RunPoll`、`NameAgent`、`GetUsableModels` … |

`aiserver.v1.InferenceStreamRequest` / `InferenceStreamResponse` 及整个 `Inference*` 消息族（55 个）**都已在我们的 `captures/schema-3.19.7.raw.json` 里**，只是不在生成闭包内。

#### 真实验证结论（2026-09-09）

最终可工作的链路不是旧 SandClaimer 的简单 RPC 字符串替换，而是：Cursor
`InferenceService/Stream` → Box gateway 的同名 relay → Box 内 Grok Bot token
provider → Sand backend。关键请求身份为：

- `x-cursor-client-type: sand`
- `x-cursor-client-version: 0.46.0`
- `x-sand-box-namespace: prod`
- `x-ghost-mode: true`
- checksum 使用 Box service machine identity
- 删除旧的 `x-cursor-client-source`、`x-cursor-config-version` 和
  `x-cursor-client-commit`

旧 relay 使用 `0.44.0 + x-cursor-client-source=sand-desktop` 时，Claude 稳定返回
`ERROR_OUTDATED_CLIENT` / `actionRequired=config`。v4 对齐 Box 自身成功请求后，
Claude 返回 `SAND_OK` / `SAND_FINAL_OK`，Grok 返回 `GROK_OK`；两种模型都有
正常 Connect end frame 和 included-usage event，延迟额度采样只增加 Bot，
Auto / Other 不增加。

因此 Go 反代应显式使用 direct `InferenceService/Stream` + Box relay；普通
`RunSSE` 仍保持原行为。IDE 补丁只把 Stream 的认证和目标 URL 改到 relay，不再
把“路由 HTTP 200”当成已打通。

### C. IDE 内部运行时改造（只用于本地补丁）

`SAND_MANAGED_LOCAL_ROUTE` / `SAND_DIRECT_INFERENCE_STREAM` / `SAND_AGENT_HOST_ENABLEMENT` / `SAND_LOCAL_RUNTIME_LOAD` / `SAND_MOVE_EXEC` / `SAND_RPC_REWRITE` / `SAND_STREAM_WRAP` / `SAND_MAXMODE` / `SAND_MEM_PRO` / `SAND_MODEL_UNLOCK` …

这些改的是 IDE 的 agent host 运行时：强制 `checkFeatureGate` 返回 `{runtime:"managed-local"}`、加载 `agent_host_local_loop`、改 `createAgentHost` 身份、搬 exec 位置。它们是本地 IDE 完整工作流需要的配套，但 Go 反代不需要照搬进程控制，只需构造同一 Stream protobuf 并走已验证的 Box relay。

`AgentRunRequest` 的字段（`harness`、`selected_subagent_models` 等）是既有协议面，不是 sand 新增。

**结论：本地 agent-host 的 UI/进程控制留在 IDE 补丁；Go 反代通过显式 direct Stream 模式复现网络链路，默认 `RunSSE` 不受影响。**

### 补丁本身的脆弱性（说明为什么不该走这条路）

源码注释里明说：

> minified 变量名随每次构建变化 —— **同一版本号的不同 commit（About 里的 ...130 与官方下载的 ...137）变量名并不相同**，写死字面量会导致「Cursor 版本对，却一个锚点都命中不了 → 切不过去」。

这正是我们 kernel 升级里记的那个 `releaseHash` 末位差异（`...130` vs `...137`）。补丁方案要为每个 Cursor 构建维护结构锚点正则；我们造头的方案不受构建变化影响。

---

## 三、DoH 的作用与我们是否需要

SandClaimer README 写「绕过本机 DNS 劫持」：内置 DoH（1.1.1.1）解析 `cursor.com` / `api2.cursor.sh` 真实 IP，用于本机跑着会劫持这些域名的网关（如 cgw）的场景。

**判断：默认不需要，但不是"没有环境会有问题"。**

会出问题的具体场景：本机或部署环境跑着把 `cursor.com` / `api2.cursor.sh` 指向自己的代理/网关（常见于同时在用某些 Cursor 中转工具）。那种情况下我们的请求会被劫到本地网关而不是真实 Cursor，表现为莫名的连接错误或认证失败，且**不易归因** —— 会看起来像 token 问题。

处理方式：不内置 DoH（增加复杂度且绕过用户自己的 DNS 策略是有争议的），而是**在 sand 命令里做一次归因检查** —— 解析这两个域名，如果解到私有网段（10./172.16-31./192.168./127.）就明确报「你的 DNS 把 cursor.com 解析到了内网 IP，可能有本地网关在劫持」，而不是让用户对着一个含糊的网络错误猜。真需要绕过时再加 `--resolve` 之类的显式开关。

---

## 四、对 cursor-proto 的取舍

| SandClaimer / VSIX 的能力 | 我们要不要 | 原因 |
|---|---|---|
| 资格查询（GetSandUsageStatus / get-sand-access-status） | **要** | 纯 HTTP，可移植 |
| 资格领取（trial / team access） | **要** | 同上 |
| `x-cursor-client-type: sand` | **要，但不够** | 我们本来就造这个头；单独它不解决额度归属 |
| **推理走 Sand 额度的那条 RPC** | **要，且已验证** | direct `InferenceService/Stream` 通过 Box relay 只增加 Bot |
| AgentService 例外发 `ide` | **保留** | 非 Stream 的 AgentService 不应被 relay 劫持 |
| membership 伪装（改响应体） | **不要** | 骗自己的 UI，我们没有 UI |
| AvailableModels 强制 defaultOn | **不要** | 同上，会让我们对自己说谎 |
| managed-local / agent host / stream 改造 | **IDE 实现，Go 不照搬** | IDE 需要本地运行时配套；Go 直接构造 Stream protobuf |
| 打补丁改 workbench.desktop.main.js | **仅 IDE 需要** | Go 自己造头；IDE 用版本锁定、可回滚补丁 |
| DoH 绕过 | **不内置**，改成归因检查 | 见上 |

---

## 五、回归探针

每次 Cursor / Grok Bot 后端升级后都应重新执行真实探针，不能只看 marker 或
relay HTTP 状态：

```bash
go run ./cmd/test-sand-quota \
  -account /path/account.json \
  -rpc inference_stream \
  -relay-config auto \
  -model claude-sonnet-4-6 \
  -msg 'Reply exactly: SAND_OK'
```

必须同时满足：返回非空文本、收到 Connect end frame、无
`ERROR_OUTDATED_CLIENT` / 464，延迟采样只增加 Bot，并能在 `ListEvents` 找到
本次 conversation。Claude 与 Grok 至少各跑一次。

## 六、我先前判断错在哪

两处，都值得记下来：

**第一次**：我说「机制就是把 `x-cursor-client-type` 从 ide 换成 sand」。那句话对上行协议的一处改动是准确的，但当成整个机制就是错的 —— 22 个 marker 我只看了 1 个。

**第二次**（你指出的）：我把 C 类判成「IDE 内部架构、我们不需要」，理由是「没有新端点」。那个理由本身就是错的 —— 我当时 grep 整条路径字面量得到 0 命中就下了结论，而 bundle 里路径是按 service/method 拼的，字面量根本不会出现。`aiserver.v1.InferenceService/Stream` 一直在那儿，是我没找到。

你的问题「反代需要替换走 bot 额度」直接命中了这个漏洞：按我原来的方案（只换头 + 资格 API），AgentService 发 `ide`、推理走 `RunSSE`，Bot 额度一分钱都不会走。

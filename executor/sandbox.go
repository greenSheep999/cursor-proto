package executor

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/cursor-proto/auth"
)

const (
	sandRelayStreamPath    = "/sand-stream-relay/aiserver.v1.InferenceService/Stream"
	sandRelayStatusPath    = "/sand-stream-relay/status"
	ensureSandBoxPath      = "/aiserver.v1.GrokBotService/EnsureSandBox"
	boxFreshFor            = 48 * time.Minute
	sandBoxRunStateRunning = 3
)

type sandBox struct {
	RelayURL     string
	Token        string
	NetworkToken string
	MintedAtMs   int64
	RunState     int
}

func boxFresh(mintedAtMs int64, now time.Time) bool {
	if mintedAtMs <= 0 {
		return true
	}
	return now.Sub(time.UnixMilli(mintedAtMs)) < boxFreshFor
}

func applyStoredBox(req *ChatRequest, acc *auth.Account) {
	if req == nil || acc == nil {
		return
	}
	if acc.RelayURL == "" || acc.BoxToken == "" {
		return
	}
	if !boxFresh(acc.BoxMintedAtMs, time.Now()) {
		return
	}
	req.BoxRelayURL = acc.RelayURL
	req.BoxToken = acc.BoxToken
	req.BoxNetworkToken = acc.NetworkToken
}

func (c *Client) applyBox(req *ChatRequest, box *sandBox) {
	if req == nil || box == nil {
		return
	}
	req.BoxRelayURL = box.RelayURL
	req.BoxToken = box.Token
	req.BoxNetworkToken = box.NetworkToken
	if acc := c.CurrentAccount(); acc != nil {
		acc.RelayURL = box.RelayURL
		acc.BoxToken = box.Token
		acc.NetworkToken = box.NetworkToken
		acc.BoxMintedAtMs = box.MintedAtMs
	}
}

func (c *Client) ensureBoxRelay(ctx context.Context, req *ChatRequest) error {
	if req == nil {
		return fmt.Errorf("chat request is required")
	}
	if req.BoxRelayURL == "" || req.BoxToken == "" {
		applyStoredBox(req, c.CurrentAccount())
	}
	if req.BoxRelayURL == "" || req.BoxToken == "" {
		box, err := c.waitSandBoxRunning(ctx)
		if err != nil {
			return err
		}
		c.applyBox(req, box)
	}
	status, err := c.probeBoxRelay(ctx, req)
	if err == nil && status == http.StatusOK {
		return nil
	}
	if status == http.StatusNotFound || status == 0 {
		if provErr := c.provisionBoxRelay(ctx, req); provErr != nil {
			return fmt.Errorf("box relay missing and provision failed: %w", provErr)
		}
	}
	return nil
}

func (c *Client) MintSandBox(ctx context.Context) (*sandBox, error) {
	acc := c.CurrentAccount()
	if acc == nil || strings.TrimSpace(acc.AccessToken) == "" {
		return nil, fmt.Errorf("EnsureSandBox needs an access token")
	}
	url := strings.TrimRight(c.API2, "/") + ensureSandBoxPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte{16, 1}))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("content-type", "application/proto")
	ApplyCommonHeadersWithClientType(httpReq, acc, auth.GenerateRequestID(), "sand")
	c.applySidecarToken(httpReq)
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("EnsureSandBox dial: %w", err)
	}
	defer resp.Body.Close()
	body, err := readBody(resp)
	if err != nil {
		return nil, fmt.Errorf("EnsureSandBox read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("EnsureSandBox http %d: %s", resp.StatusCode, string(body))
	}
	relayURL, token, network, runState, err := parseSandBoxBody(body)
	if err != nil {
		return nil, err
	}
	return &sandBox{
		RelayURL:     relayURL,
		Token:        token,
		NetworkToken: network,
		MintedAtMs:   time.Now().UnixMilli(),
		RunState:     runState,
	}, nil
}

func (c *Client) waitSandBoxRunning(ctx context.Context) (*sandBox, error) {
	deadline := time.Now().Add(150 * time.Second)
	var last *sandBox
	for {
		box, err := c.MintSandBox(ctx)
		if err != nil {
			return nil, err
		}
		last = box
		if box.RunState == sandBoxRunStateRunning || box.RunState == 0 {
			return box, nil
		}
		if time.Now().After(deadline) {
			if last.RelayURL != "" {
				return last, nil
			}
			return nil, fmt.Errorf("EnsureSandBox still state %d, want RUNNING(3)", box.RunState)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(4 * time.Second):
		}
	}
}

func parseSandBoxBody(body []byte) (relayURL, token, network string, runState int, err error) {
	payload := body
	if len(payload) >= 5 && payload[0] == 0 {
		size := int(binary.BigEndian.Uint32(payload[1:5]))
		if 5+size <= len(payload) {
			payload = payload[5 : 5+size]
		}
	}
	offset := 0
	for offset < len(payload) {
		key, next, ok := readProtoVarint(payload, offset)
		if !ok {
			break
		}
		offset = next
		field := int(key >> 3)
		wire := int(key & 7)
		switch wire {
		case 0:
			value, next, ok := readProtoVarint(payload, offset)
			if !ok {
				return "", "", "", 0, fmt.Errorf("EnsureSandBox proto truncated")
			}
			offset = next
			if field == 13 {
				runState = int(value)
			}
		case 2:
			length, next, ok := readProtoVarint(payload, offset)
			if !ok || next+int(length) > len(payload) {
				return "", "", "", 0, fmt.Errorf("EnsureSandBox proto truncated")
			}
			value := string(payload[next : next+int(length)])
			offset = next + int(length)
			switch field {
			case 10:
				relayURL = value
			case 11:
				token = value
			case 4:
				network = value
			}
		case 1:
			offset += 8
		case 5:
			offset += 4
		default:
			return "", "", "", 0, fmt.Errorf("EnsureSandBox proto wire %d", wire)
		}
	}
	if relayURL == "" || token == "" {
		return "", "", "", 0, fmt.Errorf("EnsureSandBox missing box url or token")
	}
	return relayURL, token, network, runState, nil
}

func readProtoVarint(buf []byte, offset int) (uint64, int, bool) {
	var value uint64
	var shift uint
	for offset < len(buf) {
		b := buf[offset]
		offset++
		value |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			return value, offset, true
		}
		shift += 7
		if shift > 63 {
			return 0, offset, false
		}
	}
	return 0, offset, false
}

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
	sandRelayStreamPath = "/sand-stream-relay/aiserver.v1.InferenceService/Stream"
	ensureSandBoxPath   = "/aiserver.v1.GrokBotService/EnsureSandBox"
	boxFreshFor         = 48 * time.Minute
)

type sandBox struct {
	RelayURL     string
	Token        string
	NetworkToken string
	MintedAtMs   int64
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

func (c *Client) ensureBoxRelay(ctx context.Context, req *ChatRequest) error {
	if req == nil {
		return fmt.Errorf("chat request is required")
	}
	if req.BoxRelayURL != "" && req.BoxToken != "" {
		return nil
	}
	applyStoredBox(req, c.CurrentAccount())
	if req.BoxRelayURL != "" && req.BoxToken != "" {
		return nil
	}
	box, err := c.MintSandBox(ctx)
	if err != nil {
		return err
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
	relayURL, token, network, err := parseSandBoxBody(body)
	if err != nil {
		return nil, err
	}
	return &sandBox{
		RelayURL:     relayURL,
		Token:        token,
		NetworkToken: network,
		MintedAtMs:   time.Now().UnixMilli(),
	}, nil
}

func parseSandBoxBody(body []byte) (relayURL, token, network string, err error) {
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
			_, offset, ok = readProtoVarint(payload, offset)
			if !ok {
				return "", "", "", fmt.Errorf("EnsureSandBox proto truncated")
			}
		case 2:
			length, next, ok := readProtoVarint(payload, offset)
			if !ok || next+int(length) > len(payload) {
				return "", "", "", fmt.Errorf("EnsureSandBox proto truncated")
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
			return "", "", "", fmt.Errorf("EnsureSandBox proto wire %d", wire)
		}
	}
	if relayURL == "" || token == "" {
		return "", "", "", fmt.Errorf("EnsureSandBox missing box url or token")
	}
	return relayURL, token, network, nil
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


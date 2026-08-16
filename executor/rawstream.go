package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"

	"google.golang.org/protobuf/proto"

	cursorpb "github.com/router-for-me/cursor-proto/gen/cursor"
)

// RawChatStream is a diagnostic helper that mirrors Client.RunChat but returns
// the raw HTTP response body instead of decoded ChatEvents. Callers get every
// byte the server sent (still Connect-framed) so wire-level tools can inspect
// how different upstream models are packaging their responses.
//
// This is not used by the proxy runtime. It exists so cmd/test-rawstream can
// dump captures without duplicating the RunSSE / BidiAppend orchestration.
func RawChatStream(ctx context.Context, c *Client, req *ChatRequest) (io.ReadCloser, error) {
	acc := c.CurrentAccount()
	requestID, runID, agentClientMsg, err := c.prepareChatRequest(req, acc)
	if err != nil {
		return nil, err
	}

	bidiRequestID := &cursorpb.AiserverV1_BidiRequestId{RequestId: requestID}
	bidiRequestIDBytes, err := proto.Marshal(bidiRequestID)
	if err != nil {
		return nil, fmt.Errorf("marshal BidiRequestId: %w", err)
	}
	sseBody := addConnectEnvelope(bidiRequestIDBytes, false)

	sseURL := fmt.Sprintf("%s/agent.v1.AgentService/RunSSE", c.API3)
	sseReq, err := http.NewRequestWithContext(ctx, "POST", sseURL, bytes.NewReader(sseBody))
	if err != nil {
		return nil, err
	}
	sseReq.Header.Set("content-type", "application/grpc-web+proto")
	ApplyCommonHeaders(sseReq, acc, requestID)
	sseReq.Header.Set("x-original-request-id", runID)

	sseClient := c.NewStreamClient()
	sseResp, err := sseClient.Do(sseReq)
	if err != nil {
		return nil, fmt.Errorf("RunSSE dial: %w", err)
	}
	if sseResp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(sseResp.Body, 64<<10))
		sseResp.Body.Close()
		return nil, fmt.Errorf("RunSSE http %d: %s", sseResp.StatusCode, string(body))
	}

	if err := c.bidiAppendForAccount(ctx, acc, requestID, runID, 0, agentClientMsg); err != nil {
		sseResp.Body.Close()
		return nil, fmt.Errorf("BidiAppend seed: %w", err)
	}

	return sseResp.Body, nil
}

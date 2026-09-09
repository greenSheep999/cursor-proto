package executor

import (
	"crypto/rand"
	"encoding/hex"
	"strings"

	"github.com/router-for-me/cursor-proto/auth"
	cursorpb "github.com/router-for-me/cursor-proto/gen/cursor"
)

// buildAgentRunRequest constructs the top-level AgentRunRequest for a chat.
// See docs/schema-3.10.md for the field layout.
func (c *Client) buildAgentRunRequest(req *ChatRequest, messageID string, accounts ...*auth.Account) (*cursorpb.AgentV1_AgentRunRequest, error) {
	acc := c.Account
	if len(accounts) > 0 {
		acc = accounts[0]
	}
	platform := resolveClientPlatform(acc)
	var env *cursorpb.AgentV1_RequestContextEnv
	if !req.PureMode {
		workspace := req.WorkspacePath
		if workspace == "" {
			workspace = platform.workspacePath
		}
		env = &cursorpb.AgentV1_RequestContextEnv{
			OsVersion:      platform.osVersion,
			WorkspacePaths: []string{workspace},
			Shell:          platform.shell,
			TimeZone:       timezone(),
			ProjectFolder:  workspace,
		}
	} else {
		env = &cursorpb.AgentV1_RequestContextEnv{
			OsVersion: platform.osVersion,
			TimeZone:  timezone(),
		}
	}
	reqCtx := &cursorpb.AgentV1_RequestContext{Env: env}
	if req.WebSearch {
		reqCtx.WebSearchEnabled = ptr(true)
	}
	if req.WebFetch {
		reqCtx.WebFetchEnabled = ptr(true)
	}

	// If the caller advertised MCP tools, attach them to the model-visible
	// catalog (field 7 in RequestContext) and add a matching McpInstructions
	// entry (field 14). Cursor's server also needs AgentRunRequest.mcp_tools
	// populated below, so tool calls route back via ExecServerMessage
	// field 11 (McpArgs). Without the pair, either the model doesn't see the
	// tool or the server drops the tool call before it reaches us.
	toolDefs, err := buildMcpToolDefinitions(req.Tools)
	if err != nil {
		return nil, err
	}
	if len(toolDefs) > 0 {
		reqCtx.Tools = toolDefs
		if instr := buildMcpInstructions(req.Tools); instr != nil {
			reqCtx.McpInstructions = []*cursorpb.AgentV1_McpInstructions{instr}
		}
	}

	// Cursor's server treats Mode=UNSPECIFIED (proto zero) as PLAN mode —
	// it injects a "Plan mode is active" system_reminder that blocks every
	// non-readonly tool call. Callers that leave Mode==0 want the normal
	// agentic flow (write_file / bash / etc.), so promote to AGENT here.
	// Verified 2026-07-19 by dumping the KV blob for `Mode: 0` vs `Mode: 1`.
	agentMode := cursorpb.AgentV1_AgentMode(req.Mode)
	if agentMode == cursorpb.AgentV1_AGENT_MODE_UNSPECIFIED {
		agentMode = cursorpb.AgentV1_AGENT_MODE_AGENT
	}

	userMsg := &cursorpb.AgentV1_UserMessage{
		Text:      req.UserMessage,
		MessageId: messageID,
		Mode:      agentMode,
	}
	if selectedContext := buildSelectedContext(req.Attachments); selectedContext != nil {
		userMsg.SelectedContext = selectedContext
	}
	umAction := &cursorpb.AgentV1_UserMessageAction{
		UserMessage:    userMsg,
		RequestContext: reqCtx,
	}
	if !req.OmitConversationHistoryWire {
		if hist := buildConversationHistory(req.History); hist != nil {
			umAction.ConversationHistory = hist
		}
	}
	if len(req.PrependUserMessages) > 0 {
		umAction.PrependUserMessages = buildPrependUserMessages(req.PrependUserMessages)
	}
	if req.SendToInteractionListener != nil {
		umAction.SendToInteractionListener = req.SendToInteractionListener
	}
	action := &cursorpb.AgentV1_ConversationAction{
		Action: &cursorpb.AgentV1_ConversationAction_UserMessageAction{
			UserMessageAction: umAction,
		},
	}
	trueVal := true
	// Reuse agentMode (already defaulted to AGENT for the UNSPECIFIED case
	// above) so ConversationStateStructure stays in sync with UserMessage.
	convState := &cursorpb.AgentV1_ConversationStateStructure{Mode: &agentMode}

	arr := &cursorpb.AgentV1_AgentRunRequest{
		ConversationState:                   convState,
		Action:                              action,
		ConversationId:                      ptr(req.ConversationID),
		ClientSupportsInlineImages:          &trueVal,
		ClientSupportsSendToUser:            &trueVal,
		ClientSupportsPromptContextUsageRpc: &trueVal,
		ClientSupportsRoutedModelUpdate:     &trueVal,
	}
	if req.runID != "" {
		arr.RunId = &req.runID
	}
	if req.resolvedModel != nil {
		arr.RequestedModel = req.resolvedModel
	} else {
		arr.ModelDetails = &cursorpb.AgentV1_ModelDetails{ModelId: req.Model}
	}
	if req.Harness != "" {
		arr.Harness = &req.Harness
	}

	// AgentRunRequest.mcp_tools wraps the same McpToolDefinition list — this
	// is what makes Cursor's server route tool calls back via
	// ExecServerMessage field 11. Populating only RequestContext.tools would
	// tell the model about the tools but the server would silently drop the
	// resulting McpArgs frames.
	if len(toolDefs) > 0 {
		arr.McpTools = &cursorpb.AgentV1_McpTools{McpTools: toolDefs}
	}
	// NOTE: We *do not* forward the request's SystemPrompt into
	// CustomSystemPrompt — Cursor's backend rejects that field with
	// `unknown option '--system-prompt'` regardless of harness. Instead,
	// callers should splice the system prompt into UserMessage themselves
	// (RunChat does this automatically).
	return arr, nil
}

func buildSelectedContext(attachments []Attachment) *cursorpb.AgentV1_SelectedContext {
	if len(attachments) == 0 {
		return nil
	}
	selected := &cursorpb.AgentV1_SelectedContext{}
	for _, attachment := range attachments {
		if len(attachment.Data) == 0 {
			continue
		}
		switch attachment.Kind {
		case "image":
			selected.SelectedImages = append(selected.SelectedImages, &cursorpb.AgentV1_SelectedImage{
				Uuid:     auth.GenerateSessionID(),
				MimeType: attachment.MimeType,
				DataOrBlobId: &cursorpb.AgentV1_SelectedImage_Data{
					Data: append([]byte(nil), attachment.Data...),
				},
			})
		case "document":
			selected.SelectedDocuments = append(selected.SelectedDocuments, &cursorpb.AgentV1_SelectedDocument{
				Uuid:     auth.GenerateSessionID(),
				Filename: attachment.Filename,
				MimeType: attachment.MimeType,
				DataOrBlobId: &cursorpb.AgentV1_SelectedDocument_Data{
					Data: append([]byte(nil), attachment.Data...),
				},
			})
		}
	}
	if len(selected.SelectedImages) == 0 && len(selected.SelectedDocuments) == 0 {
		return nil
	}
	return selected
}

// buildConversationHistory turns the caller-supplied prior turns into a
// Cursor ConversationHistory message. Returns nil when there is nothing to
// send so single-turn callers stay wire-compatible.
//
// Tool_use / tool_result JSON that the Anthropic parser flattened into a
// turn is restored onto ConversationHistoryToolCall / ToolMessage so a
// later Cursor run can see the tool chain instead of opaque text.
func buildConversationHistory(turns []HistoryTurn) *cursorpb.AgentV1_ConversationHistory {
	if len(turns) == 0 {
		return nil
	}
	msgs := make([]*cursorpb.AgentV1_ConversationHistoryMessage, 0, len(turns))
	for _, t := range turns {
		if t.Content == "" {
			continue
		}
		switch t.Role {
		case "user":
			msgs = append(msgs, conversationHistoryFromUser(t.Content)...)
		case "assistant":
			if msg := conversationHistoryFromAssistant(t.Content); msg != nil {
				msgs = append(msgs, msg)
			}
		}
	}
	if len(msgs) == 0 {
		return nil
	}
	fillEmptyHistoryToolIDs(msgs)
	replace := true
	return &cursorpb.AgentV1_ConversationHistory{
		Messages: msgs,
		// ReplaceUserInfo=true tells the server to trust the messages we ship
		// instead of falling back to its own stored transcript. Without this
		// flag Cursor's backend acknowledges the request but does not fold
		// the history into the prompt fed to the model.
		ReplaceUserInfo: &replace,
	}
}

// buildPrependUserMessages projects HistoryTurn entries onto
// AgentV1_UserMessage protos so we can probe whether Cursor treats
// UserMessageAction.prepend_user_messages (field 4) as a history channel.
// Because the field carries UserMessage protos (no assistant variant), any
// non-user turn is serialized as an inline "[ASSISTANT]: ..." user turn.
func buildPrependUserMessages(turns []HistoryTurn) []*cursorpb.AgentV1_UserMessage {
	if len(turns) == 0 {
		return nil
	}
	out := make([]*cursorpb.AgentV1_UserMessage, 0, len(turns))
	for _, t := range turns {
		if t.Content == "" {
			continue
		}
		text := t.Content
		if t.Role == "assistant" {
			text = "[ASSISTANT]: " + t.Content
		}
		out = append(out, &cursorpb.AgentV1_UserMessage{Text: text})
	}
	return out
}

func conversationHistoryFromUser(content string) []*cursorpb.AgentV1_ConversationHistoryMessage {
	fragments := ParseContentFragments(content)
	if len(fragments) == 0 {
		return []*cursorpb.AgentV1_ConversationHistoryMessage{userHistoryText(content)}
	}
	var msgs []*cursorpb.AgentV1_ConversationHistoryMessage
	var text strings.Builder
	flushText := func() {
		if text.Len() == 0 {
			return
		}
		msgs = append(msgs, userHistoryText(text.String()))
		text.Reset()
	}
	for _, fragment := range fragments {
		switch fragment.Kind {
		case ContentToolResult:
			flushText()
			result := fragment.Result
			if result == "" {
				result = fragment.Text
			}
			msgs = append(msgs, &cursorpb.AgentV1_ConversationHistoryMessage{
				Message: &cursorpb.AgentV1_ConversationHistoryMessage_Tool{
					Tool: &cursorpb.AgentV1_ConversationHistoryToolMessage{
						ToolCallId: fragment.ToolID,
						ToolName:   fragment.ToolName,
						IsError:    ptr(fragment.IsError),
						Content: []*cursorpb.AgentV1_ConversationHistoryToolResultContent{{
							Content: &cursorpb.AgentV1_ConversationHistoryToolResultContent_Text{
								Text: &cursorpb.AgentV1_ConversationHistoryTextContent{Text: result},
							},
						}},
					},
				},
			})
		default:
			if text.Len() > 0 {
				text.WriteByte('\n')
			}
			text.WriteString(fragment.Transcript())
		}
	}
	flushText()
	return msgs
}

// fillEmptyHistoryToolIDs pairs assistant tool_use blocks that arrived
// without an id (TokenSheep/NewAPI omitempty-strips them on the way in)
// with the following tool_result tool_use_id. Vertex rejects a tool_use
// whose id field is missing.
func fillEmptyHistoryToolIDs(msgs []*cursorpb.AgentV1_ConversationHistoryMessage) {
	var pending []*cursorpb.AgentV1_ConversationHistoryToolCall
	for _, msg := range msgs {
		if asst := msg.GetAssistant(); asst != nil {
			for _, part := range asst.Content {
				if tc := part.GetToolCall(); tc != nil && strings.TrimSpace(tc.GetToolCallId()) == "" {
					pending = append(pending, tc)
				}
			}
		}
		if tool := msg.GetTool(); tool != nil && len(pending) > 0 {
			tc := pending[0]
			pending = pending[1:]
			id := strings.TrimSpace(tool.GetToolCallId())
			if id == "" {
				id = strings.TrimSpace(tc.GetToolCallId())
			}
			if id == "" {
				id = newHistoryToolID()
			}
			if strings.TrimSpace(tc.GetToolCallId()) == "" {
				tc.ToolCallId = id
			}
			if strings.TrimSpace(tool.GetToolCallId()) == "" {
				tool.ToolCallId = id
			}
		}
	}
	for _, tc := range pending {
		if strings.TrimSpace(tc.GetToolCallId()) == "" {
			tc.ToolCallId = newHistoryToolID()
		}
	}
}

func newHistoryToolID() string {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "toolu_historyfallback"
	}
	return "toolu_" + hex.EncodeToString(raw[:])
}

func conversationHistoryFromAssistant(content string) *cursorpb.AgentV1_ConversationHistoryMessage {
	fragments := ParseContentFragments(content)
	if len(fragments) == 0 {
		return assistantHistoryText(content)
	}
	var parts []*cursorpb.AgentV1_ConversationHistoryAssistantContent
	for _, fragment := range fragments {
		switch fragment.Kind {
		case ContentToolUse:
			parts = append(parts, &cursorpb.AgentV1_ConversationHistoryAssistantContent{
				Content: &cursorpb.AgentV1_ConversationHistoryAssistantContent_ToolCall{
					ToolCall: &cursorpb.AgentV1_ConversationHistoryToolCall{
						ToolCallId: fragment.ToolID,
						ToolName:   fragment.ToolName,
						ArgsJson:   fragment.ArgsJSON,
					},
				},
			})
		default:
			if fragment.Text == "" {
				continue
			}
			parts = append(parts, &cursorpb.AgentV1_ConversationHistoryAssistantContent{
				Content: &cursorpb.AgentV1_ConversationHistoryAssistantContent_Text{
					Text: &cursorpb.AgentV1_ConversationHistoryTextContent{Text: fragment.Text},
				},
			})
		}
	}
	if len(parts) == 0 {
		return assistantHistoryText(content)
	}
	return &cursorpb.AgentV1_ConversationHistoryMessage{
		Message: &cursorpb.AgentV1_ConversationHistoryMessage_Assistant{
			Assistant: &cursorpb.AgentV1_ConversationHistoryAssistantMessage{Content: parts},
		},
	}
}

func userHistoryText(text string) *cursorpb.AgentV1_ConversationHistoryMessage {
	return &cursorpb.AgentV1_ConversationHistoryMessage{
		Message: &cursorpb.AgentV1_ConversationHistoryMessage_User{
			User: &cursorpb.AgentV1_ConversationHistoryUserMessage{
				Content: []*cursorpb.AgentV1_ConversationHistoryUserContent{{
					Content: &cursorpb.AgentV1_ConversationHistoryUserContent_Text{
						Text: &cursorpb.AgentV1_ConversationHistoryTextContent{Text: text},
					},
				}},
			},
		},
	}
}

func assistantHistoryText(text string) *cursorpb.AgentV1_ConversationHistoryMessage {
	return &cursorpb.AgentV1_ConversationHistoryMessage{
		Message: &cursorpb.AgentV1_ConversationHistoryMessage_Assistant{
			Assistant: &cursorpb.AgentV1_ConversationHistoryAssistantMessage{
				Content: []*cursorpb.AgentV1_ConversationHistoryAssistantContent{{
					Content: &cursorpb.AgentV1_ConversationHistoryAssistantContent_Text{
						Text: &cursorpb.AgentV1_ConversationHistoryTextContent{Text: text},
					},
				}},
			},
		},
	}
}

func ptr[T any](v T) *T { return &v }

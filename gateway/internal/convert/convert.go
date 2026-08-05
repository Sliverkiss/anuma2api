// Package convert translates between OpenAI Chat Completions requests and the
// upstream Responses API format (SPEC section 5.2), and decomposes the single
// upstream streamed response object into OpenAI chat.completion.chunk SSE
// events.
package convert

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// anumaSystemPrompt is the full Anuma-style system prompt embedded from
// anuma_system_prompt.txt (SPEC system-inject §2.1). It is kept as the single
// source of truth from which head9 (the minimal unlock header) is derived.
//
//go:embed anuma_system_prompt.txt
var anumaSystemPrompt string

// head9 is the minimal Anuma-style unlock header: the first 9 lines of the
// full prompt (SPEC system-inject §2.1b, 429 chars). Hermes measured that
// this prefix alone passes the portal's script-based risk control — the full
// 16KB also works but wastes tokens and can 502 some basic-tier models
// (qwen-3.6-plus / minimax-m2.5). The gateway prepends it for every model
// except noInjectModels (which 500 on any system message); any user-supplied
// system content is appended after it, never overwritten.
var head9 = unlockHead9()

// unlockHead9 derives head9 from the embedded full prompt. The unlock header
// is "You are an AI assistant developed by ZetaChain." + the Memory
// Capabilities block + the "MCP Tools:" line, i.e. lines 1-9 of the file.
func unlockHead9() string {
	const n = 9
	lines := strings.SplitN(anumaSystemPrompt, "\n", n+1)
	if len(lines) <= n {
		return anumaSystemPrompt // fewer than 9 lines: use the whole file
	}
	return strings.Join(lines[:n], "\n")
}

// noInjectModels are upstream models whose /responses endpoint returns a 500
// "internal_error" whenever the request input carries a system message — with
// or without content, head9 or full prompt (verified 2026-08-03 across 3
// accounts). They are already unlockable without any system prompt, so the
// gateway must NOT inject head9 for them or they break.
var noInjectModels = map[string]bool{
	"qwen/qwen-3.6-plus":   true,
	"minimax/minimax-m2.5": true,
}

// modelOutputCap is the known hard ceiling on output tokens the upstream
// portal.anuma.ai enforces per model, regardless of the max_output_tokens the
// gateway sends. It is the only reliable length-truncation signal: the probe
// (2026-08-04, claude-sonnet-5) showed the upstream never fills
// incomplete_details.reason or response.status even when it truncates at the
// cap, so the gateway infers length truncation from completion_tokens reaching
// the cap. Entries are keyed by the short model name the upstream
// response.model actually returns (e.g. "claude-sonnet-5", not the
// vendor-prefixed long name); an unknown model gets no cap (length is never
// inferred). Add entries here as new caps are measured.
//
// Measured 2026-08-04 (ticket 11): max_tokens=16000, 2+ non-stream requests
// per model. Nine models truncate at a hard cap; grok-4.5 has no cap (natural
// end at 1230/1442/1070 — non-round, finish=stop). qwen-3.6-plus and
// minimax-m2.5 (the noInjectModels basic tier) returned 503 "all accounts
// unavailable" throughout the test window so their caps are unknown and they
// are intentionally omitted (length is not inferred for them). The two gemini
// models stop at exactly 1020 (not 1024): 3 consecutive runs held at 1020,
// so the cap is recorded as 1020 to keep completion>=cap accurate — recording
// 1024 would mask their truncation as "stop".
var modelOutputCap = map[string]int{
	"ling-2.6-flash":         1024,
	"kimi-k3":                1024,
	"gpt-5.6-luna":           1024,
	"qwen-3.7-plus":          1024,
	"glm-5.2":                1024,
	"minimax-m3":             1024,
	"claude-sonnet-5":        1024,
	"gemini-3.1-pro-preview": 1020,
	"gemini-3-flash-preview": 1020,
}

// defaultMaxOutputTokens is the max_output_tokens the gateway sends to the
// upstream when the client did not supply one (P1-A2: raised from 4096 so
// non-cap-limited models can return longer outputs; the upstream's own per-
// model cap still applies the real ceiling, e.g. claude-sonnet-5 stays 1024).
const defaultMaxOutputTokens = 16384

// shouldInjectSystem reports whether the head9 unlock header must be prepended
// for the given resolved upstream model. All allowlisted advanced models need
// it (else the portal 403s on script risk control); the two basic-tier models
// above must be skipped (else the portal 500s).
func shouldInjectSystem(resolvedModel string) bool {
	return !noInjectModels[resolvedModel]
}

// Message is one OpenAI chat message.
type Message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"` // string or []block
	ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

// ToolCall is the OpenAI tool_calls entry in an assistant message.
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction is the name + arguments of a tool call.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Tool is an OpenAI function tool.
type Tool struct {
	Type     string         `json:"type"`
	Function ToolDefinition `json:"function"`
}

// ToolDefinition describes a function tool.
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// UpstreamInputItem is one entry in the upstream "input" array.
type UpstreamInputItem struct {
	Role       string             `json:"role"`
	Content    []ContentBlock     `json:"content"`
	ToolCalls  []UpstreamToolCall `json:"tool_calls,omitempty"`
	ToolCallID string             `json:"tool_call_id,omitempty"`
}

// ContentBlock is a text content block.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// UpstreamToolCall is the Chat Completions style tool_call that the upstream
// accepts in assistant messages (SPEC 3.5).
type UpstreamToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function UpstreamToolFunc `json:"function"`
}

// UpstreamToolFunc carries name + arguments.
type UpstreamToolFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// UpstreamTool is the OpenAI-format tool passed through to the upstream.
type UpstreamTool struct {
	Type     string                 `json:"type"`
	Function UpstreamToolDefinition `json:"function"`
}

// UpstreamToolDefinition mirrors ToolDefinition for JSON marshaling.
type UpstreamToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// StreamOptions carries the OpenAI stream_options request object. The gateway
// supports include_usage: when true, the final streamed chunk carries a usage
// object (SPEC: stream usage).
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// Request is the parsed OpenAI Chat Completions request.
type Request struct {
	Model           string          `json:"model"`
	Messages        []Message       `json:"messages"`
	Stream          bool            `json:"stream"`
	StreamOptions   *StreamOptions  `json:"stream_options,omitempty"`
	MaxTokens       *int            `json:"max_tokens"`
	MaxOutputTokens *int            `json:"max_output_tokens"`
	Temperature     *float64        `json:"temperature"`
	Tools           []Tool          `json:"tools"`
	ToolChoice      json.RawMessage `json:"tool_choice"`
	// ConversationID is the client's conversation handle (OpenAI Responses style
	// extension, ticket 12). When set, the gateway uses it verbatim as the
	// upstream conversation_id and for account stickiness instead of generating a
	// fresh UUID, so a continuation request lands on the same account and the
	// same upstream conversation.
	ConversationID string `json:"conversation_id,omitempty"`
	// PreviousResponseID references the upstream response this request continues
	// from (the client echoes the gateway-returned previous_response_id after a
	// finish_reason=length truncation). Forwarded to the upstream when set.
	PreviousResponseID string `json:"previous_response_id,omitempty"`
}

// WantUsage reports whether the client asked for token usage on the final
// streamed chunk via stream_options.include_usage (SPEC: stream usage).
func (r *Request) WantUsage() bool {
	return r.StreamOptions != nil && r.StreamOptions.IncludeUsage
}

// ToUpstreamPayload builds the upstream responses request body (SPEC 3.2).
func (r *Request) ToUpstreamPayload(conversationID string) ([]byte, error) {
	// Resolve the inbound model (short or long name) to the upstream long name
	// used for forwarding (SPEC model-alias §3.3).
	resolved := ResolveUpstream(r.Model)
	input := make([]UpstreamInputItem, 0, len(r.Messages)+1)
	var userSystems []string
	inject := shouldInjectSystem(resolved)
	for _, m := range r.Messages {
		if m.Role == "system" {
			if !inject {
				// Exempted model (noInjectModels): keep the user's system
				// message in place and unchanged — no head9, no merge.
				item, err := convertMessage(m)
				if err != nil {
					return nil, err
				}
				input = append(input, item)
				continue
			}
			// Collect user-supplied system content so it can be appended after
			// the head9 unlock header below instead of being overwritten.
			txt, err := systemText(m)
			if err != nil {
				return nil, err
			}
			if strings.TrimSpace(txt) != "" {
				userSystems = append(userSystems, txt)
			}
			continue
		}
		item, err := convertMessage(m)
		if err != nil {
			return nil, err
		}
		input = append(input, item)
	}
	// Prepend the head9 unlock header: the upstream portal 403s requests whose
	// "input" lacks an Anuma-style system prefix (SPEC system-inject §2.1b). A
	// user-supplied system is kept by merging it after the header; with no user
	// system, the header stands alone. The two basic models that 500 on any
	// system message are exempted (see noInjectModels).
	if inject {
		input = append([]UpstreamInputItem{{
			Role:    "system",
			Content: []ContentBlock{{Type: "text", Text: systemInjectionText(userSystems)}},
		}}, input...)
	}
	payload := map[string]interface{}{
		"input":           input,
		"model":           resolved,
		"stream":          r.Stream,
		"conversation_id": conversationID,
	}
	// Continue-from reference (ticket 12): forward the client's
	// previous_response_id so the upstream appends this request to the prior
	// response instead of starting fresh.
	if r.PreviousResponseID != "" {
		payload["previous_response_id"] = r.PreviousResponseID
	}
	if r.MaxTokens != nil {
		payload["max_output_tokens"] = *r.MaxTokens
	} else if r.MaxOutputTokens != nil {
		payload["max_output_tokens"] = *r.MaxOutputTokens
	} else {
		payload["max_output_tokens"] = defaultMaxOutputTokens
	}
	if r.Temperature != nil {
		payload["temperature"] = *r.Temperature
	}
	if len(r.Tools) > 0 {
		tools := make([]UpstreamTool, 0, len(r.Tools))
		for _, t := range r.Tools {
			tools = append(tools, UpstreamTool{
				Type: "function",
				Function: UpstreamToolDefinition{
					Name:        t.Function.Name,
					Description: t.Function.Description,
					Parameters:  t.Function.Parameters,
				},
			})
		}
		payload["tools"] = tools
	}
	if len(r.ToolChoice) > 0 && string(r.ToolChoice) != "null" {
		payload["tool_choice"] = json.RawMessage(r.ToolChoice)
	}
	return json.Marshal(payload)
}

// systemInjectionText builds the text of the single injected system message:
// always the head9 unlock header, with any user-supplied system content
// appended after it (SPEC system-inject §2.1b). No user system → head9 only.
func systemInjectionText(userSystems []string) string {
	if len(userSystems) == 0 {
		return head9
	}
	return head9 + "\n\n" + strings.Join(userSystems, "\n\n")
}

// systemText extracts the plain-text content of a system message, handling
// both the string form and the array-of-blocks form. Non-text blocks are
// skipped, matching normalizeContent.
func systemText(m Message) (string, error) {
	blocks, err := normalizeContent(m.Content)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, b := range blocks {
		sb.WriteString(b.Text)
	}
	return sb.String(), nil
}

func convertMessage(m Message) (UpstreamInputItem, error) {
	item := UpstreamInputItem{Role: m.Role}
	content, err := normalizeContent(m.Content)
	if err != nil {
		return item, err
	}
	item.Content = content

	if m.Role == "assistant" && len(m.ToolCalls) > 0 {
		tcs := make([]UpstreamToolCall, 0, len(m.ToolCalls))
		for _, tc := range m.ToolCalls {
			tcs = append(tcs, UpstreamToolCall{
				ID:   tc.ID,
				Type: "function",
				Function: UpstreamToolFunc{
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				},
			})
		}
		item.ToolCalls = tcs
	}
	if m.Role == "tool" {
		item.ToolCallID = m.ToolCallID
	}
	return item, nil
}

func normalizeContent(raw json.RawMessage) ([]ContentBlock, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return []ContentBlock{}, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return []ContentBlock{}, nil
		}
		return []ContentBlock{{Type: "text", Text: s}}, nil
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, fmt.Errorf("unsupported content format: %v", err)
	}
	out := make([]ContentBlock, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case "text", "input_text":
			if b.Text != "" {
				out = append(out, ContentBlock{Type: "text", Text: b.Text})
			}
		default:
			// Skip image/audio/etc blocks: the gateway is text-focused (SPEC 5.2).
			continue
		}
	}
	return out, nil
}

// Session carries the conversation-continuation identifiers (ticket 12). The
// upstream does not echo a conversation_id (probe 2026-08-04), so the gateway
// echoes the client's conversation_id when one was used and always surfaces the
// upstream response.id as previous_response_id — the value a client needs to
// issue the next "continue" request after finish_reason=length.
type Session struct {
	ConversationID      string
	PreviousResponseID  string
	HasConversationID   bool
	HasPreviousResponse bool
}

// ChatCompletion is the OpenAI Chat Completion response shape.
type ChatCompletion struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
	Usage   Usage        `json:"usage"`
	// ConversationID echoes the client's conversation_id when this request used
	// one (ticket 12). Omitted otherwise so the standard OpenAI shape stays
	// unchanged for ordinary requests.
	ConversationID string `json:"conversation_id,omitempty"`
	// PreviousResponseID is the upstream response.id of this reply — the handle
	// the client passes back to continue the conversation after truncation.
	// Omitted when the upstream response carries no usable id.
	PreviousResponseID string `json:"previous_response_id,omitempty"`
}

// ChatChoice is one choice.
type ChatChoice struct {
	Index        int             `json:"index"`
	Message      *ChatMessage    `json:"message,omitempty"`
	Delta        json.RawMessage `json:"delta,omitempty"`
	FinishReason *string         `json:"finish_reason"`
}

// ChatMessage is the assistant message in a completion.
type ChatMessage struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// PromptTokensDetails is the OpenAI-standard usage detail object attached to
// usage.prompt_tokens_details. cached_tokens is the number of prompt tokens
// served from cache (0 when the upstream does not provide cache stats).
type PromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

// Usage mirrors OpenAI usage.
type Usage struct {
	PromptTokens        int                  `json:"prompt_tokens"`
	CompletionTokens    int                  `json:"completion_tokens"`
	TotalTokens         int                  `json:"total_tokens"`
	PromptTokensDetails *PromptTokensDetails `json:"prompt_tokens_details,omitempty"`
}

// UpstreamOutput is a minimal output item for conversion.
type UpstreamOutput struct {
	Type      string         `json:"type"`
	Content   []ContentBlock `json:"content"`
	Name      string         `json:"name"`
	CallID    string         `json:"call_id"`
	Arguments string         `json:"arguments"`
}

// FinishHint is the completion signal the convert package needs to map onto an
// OpenAI finish_reason. It is the convert-side view of upstream.FinishSignals
// (kept as a local struct so the convert package does not import upstream,
// preserving the one-way dependency convert <- api -> upstream). The upstream
// never sends a usable stop_reason (probe 2026-08-04), so the only reliable
// length signal is CompletionTokens reaching the model's hard cap.
type FinishHint struct {
	Model             string // upstream long model name, e.g. "anthropic/claude-sonnet-5"
	CompletionTokens  int    // output token count (stream: output_tokens, non-stream: completion_tokens)
	IncompleteReason  string // raw incomplete_details.reason; upstream leaves "" even on truncation
	OutputHasToolCall bool   // any output item is a function_call
}

// resolveFinishReason maps a FinishHint onto an OpenAI Chat finish_reason
// (P0-A1). Priority:
//  1. tool_calls when the output carries a function_call (tool call ends turn).
//  2. "length" when completion_tokens reached the model's known hard cap
//     (modelOutputCap), OR when the upstream explicitly signaled an incomplete
//     reason containing "max_output_tokens"/"length" (future-proofing — measured
//     empty today).
//  3. "stop" otherwise (normal end-of-turn).
//
// The cap-table inference is the only reliable length signal: the probe
// (2026-08-04) showed the upstream truncates at the per-model cap without
// filling incomplete_details.reason or response.status. Without the cap table,
// a length-truncated response would be mislabeled "stop" and clients would
// treat a cut-off answer as complete.
func resolveFinishReason(h FinishHint) string {
	if h.OutputHasToolCall {
		return "tool_calls"
	}
	if isLengthTruncated(h) {
		return "length"
	}
	return "stop"
}

// isLengthTruncated reports whether the completion was truncated at the model's
// output-token cap, or the upstream explicitly signaled length via
// incomplete_details.reason.
func isLengthTruncated(h FinishHint) bool {
	// Explicit upstream signal (future-proof): the probe found reason always "",
	// but if the upstream starts filling it we honor it.
	if r := strings.ToLower(h.IncompleteReason); r != "" {
		if strings.Contains(r, "max_output_tokens") || strings.Contains(r, "length") {
			return true
		}
	}
	// Cap-table inference: completion_tokens reaching the known per-model hard
	// cap means the upstream cut the output at the ceiling. The upstream
	// response.model may carry the short name ("claude-sonnet-5") while the cap
	// table is keyed by the vendor-prefixed long name
	// ("anthropic/claude-sonnet-5") — probe 2026-08-04 returned the short form —
	// so try both spellings before giving up.
	if h.CompletionTokens > 0 && h.Model != "" {
		candidates := []string{h.Model}
		if i := strings.LastIndex(h.Model, "/"); i >= 0 {
			candidates = append(candidates, h.Model[i+1:])
		}
		for _, name := range candidates {
			if cap, ok := modelOutputCap[name]; ok && h.CompletionTokens >= cap {
				return true
			}
		}
	}
	return false
}

// ResolveFinishReason is the exported form of resolveFinishReason for tests
// and external callers that build a FinishHint directly.
func ResolveFinishReason(h FinishHint) string { return resolveFinishReason(h) }

// IsLengthTruncated reports whether a FinishHint signals a length-truncated
// completion (completion tokens at the model cap, or an explicit upstream
// max_output_tokens/length reason). The api layer uses it to decide whether to
// auto-continue (ticket 13).
func IsLengthTruncated(h FinishHint) bool { return isLengthTruncated(h) }

// UpstreamResponseToChat converts a non-streaming upstream Response into a
// ChatCompletion (SPEC 5.2). The finish_reason is resolved from the FinishHint
// (P0-A1): a length-truncated completion is now surfaced as "length" instead
// of being masked as "stop".
func UpstreamResponseToChat(respID, model string, output []UpstreamOutput, usage Usage, hint FinishHint, session Session) *ChatCompletion {
	var content strings.Builder
	var toolCalls []ToolCall
	for _, item := range output {
		switch item.Type {
		case "message":
			for _, b := range item.Content {
				if b.Type == "output_text" || b.Type == "text" {
					content.WriteString(b.Text)
				}
			}
		case "function_call":
			toolCalls = append(toolCalls, ToolCall{
				ID:   item.CallID,
				Type: "function",
				Function: ToolCallFunction{
					Name:      item.Name,
					Arguments: item.Arguments,
				},
			})
		}
	}
	// hint.OutputHasToolCall is authoritative when the caller built it from the
	// response; keep len(toolCalls) as a fallback for callers that pass a zero
	// hint (preserves the old behavior for direct converters).
	hint.OutputHasToolCall = hint.OutputHasToolCall || len(toolCalls) > 0
	msg := ChatMessage{Role: "assistant", Content: content.String()}
	if len(toolCalls) > 0 {
		msg.ToolCalls = toolCalls
	}
	finish := resolveFinishReason(hint)
	cc := &ChatCompletion{
		ID:      respID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []ChatChoice{{
			Index:        0,
			Message:      &msg,
			FinishReason: &finish,
		}},
		Usage: usage,
	}
	// Attach the continuation identifiers (ticket 12). The conversation_id is
	// echoed only when this request actually used one (client-supplied or
	// gateway-generated and forwarded); previous_response_id is the upstream
	// response id when available.
	if session.HasConversationID && session.ConversationID != "" {
		cc.ConversationID = session.ConversationID
	}
	if session.HasPreviousResponse && session.PreviousResponseID != "" {
		cc.PreviousResponseID = session.PreviousResponseID
	}
	return cc
}

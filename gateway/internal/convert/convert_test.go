package convert

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func mustRaw(t *testing.T, s string) json.RawMessage {
	t.Helper()
	return json.RawMessage(s)
}

// TestDisplayName verifies long -> short: the provider prefix up to and
// including the first "/" is stripped; an id without "/" is unchanged (SPEC
// model-alias §3.1).
func TestDisplayName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"anthropic/claude-sonnet-5", "claude-sonnet-5"},
		{"openai/gpt-5.6-luna", "gpt-5.6-luna"},
		{"inclusionai/ling-2.6-flash", "ling-2.6-flash"},
		{"no-slash", "no-slash"},
		{"", ""},
	}
	for _, c := range cases {
		if got := DisplayName(c.in); got != c.want {
			t.Errorf("DisplayName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestResolveUpstreamBidirectional verifies the full 12-entry mapping: every
// short name resolves to its upstream long name, and every long name resolves
// to itself (SPEC model-alias §3.5).
func TestResolveUpstreamBidirectional(t *testing.T) {
	cases := []struct{ short, long string }{
		{"ling-2.6-flash", "inclusionai/ling-2.6-flash"},
		{"kimi-k3", "kimi/kimi-k3"},
		{"gpt-5.6-luna", "openai/gpt-5.6-luna"},
		{"qwen-3.7-plus", "qwen/qwen-3.7-plus"},
		{"qwen-3.6-plus", "qwen/qwen-3.6-plus"},
		{"glm-5.2", "glm/glm-5.2"},
		{"minimax-m3", "minimax/minimax-m3"},
		{"minimax-m2.5", "minimax/minimax-m2.5"},
		{"claude-sonnet-5", "anthropic/claude-sonnet-5"},
		{"gemini-3.1-pro-preview", "gemini/gemini-3.1-pro-preview"},
		{"gemini-3-flash-preview", "gemini/gemini-3-flash-preview"},
		{"grok-4.5", "grok/grok-4.5"},
	}
	for _, c := range cases {
		if got := ResolveUpstream(c.short); got != c.long {
			t.Errorf("ResolveUpstream(%q) = %q, want %q", c.short, got, c.long)
		}
		if got := ResolveUpstream(c.long); got != c.long {
			t.Errorf("ResolveUpstream(%q) = %q, want %q (long name idempotent)", c.long, got, c.long)
		}
		// Idempotence with DisplayName: ResolveUpstream(DisplayName(x)) == x.
		if got := ResolveUpstream(DisplayName(c.long)); got != c.long {
			t.Errorf("ResolveUpstream(DisplayName(%q)) = %q, want %q", c.long, got, c.long)
		}
	}
}

// TestResolveUpstreamUnknownPassThrough verifies unknown models — short or long
// form — are returned unchanged so the allowlist check that runs afterwards
// rejects them (SPEC model-alias §3.1 / §3.3).
func TestResolveUpstreamUnknownPassThrough(t *testing.T) {
	cases := []string{
		"anthropic/claude-opus-5",
		"openai/gpt-5.6-sol",
		"some-random-model",
		"no-slash",
		"",
	}
	for _, in := range cases {
		if got := ResolveUpstream(in); got != in {
			t.Errorf("ResolveUpstream(%q) = %q, want unchanged %q", in, got, in)
		}
	}
}

func TestToUpstreamPayloadBasic(t *testing.T) {
	r := &Request{
		Model: "glm-5.2",
		Messages: []Message{
			{Role: "system", Content: mustRaw(t, `"You are helpful."`)},
			{Role: "user", Content: mustRaw(t, `"hello"`)},
		},
	}
	payload, err := r.ToUpstreamPayload("conv-1")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "glm/glm-5.2" {
		t.Errorf("model = %v, want glm/glm-5.2", body["model"])
	}
	if body["stream"] != false {
		t.Errorf("stream should be false")
	}
	if body["max_output_tokens"] != float64(16384) {
		t.Errorf("default max_output_tokens = %v", body["max_output_tokens"])
	}
	if body["conversation_id"] != "conv-1" {
		t.Errorf("conversation_id mismatch")
	}
	input, ok := body["input"].([]interface{})
	if !ok || len(input) != 2 {
		t.Fatalf("input = %v", body["input"])
	}
	first := input[0].(map[string]interface{})
	if first["role"] != "system" {
		t.Errorf("first role = %v", first["role"])
	}
	content := first["content"].([]interface{})
	block := content[0].(map[string]interface{})
	if block["type"] != "text" {
		t.Errorf("content block = %v", block)
	}
	// User system is preserved by merging it after the head9 unlock header
	// (SPEC system-inject §2.1b: head9 + user system, never overwritten).
	want := head9 + "\n\n" + "You are helpful."
	if got := block["text"]; got != want {
		t.Errorf("system text = %q, want %q", got, want)
	}
}

// TestToUpstreamPayloadInjectsDefaultSystem verifies that when the request has
// no role=system message, the head9 unlock header is prepended to the upstream
// input — the minimal 9-line prefix, not the full embedded prompt (SPEC
// system-inject §2.1b).
func TestToUpstreamPayloadInjectsDefaultSystem(t *testing.T) {
	r := &Request{
		Model: "anthropic/claude-sonnet-5",
		Messages: []Message{
			{Role: "user", Content: mustRaw(t, `"hello"`)},
		},
	}
	payload, err := r.ToUpstreamPayload("conv-sys")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatal(err)
	}
	input := body["input"].([]interface{})
	if len(input) != 2 {
		t.Fatalf("input len = %d, want 2 (injected system + user)", len(input))
	}
	first := input[0].(map[string]interface{})
	if first["role"] != "system" {
		t.Errorf("first role = %v, want system", first["role"])
	}
	blocks := first["content"].([]interface{})
	if len(blocks) != 1 {
		t.Fatalf("system content blocks = %d, want 1", len(blocks))
	}
	block := blocks[0].(map[string]interface{})
	if block["type"] != "text" {
		t.Errorf("system block type = %v, want text", block["type"])
	}
	if got := block["text"]; got != head9 {
		t.Errorf("injected system text = %q, want head9 (%d chars)", got, len(head9))
	}
	if role := input[1].(map[string]interface{})["role"]; role != "user" {
		t.Errorf("second role = %v, want user", role)
	}
}

// TestNoInjectModelsSkipsSystem verifies that the two basic models which the
// upstream 500s on any system message are exempted from head9 injection, and a
// user-supplied system message for them is preserved in place (SPEC 2.1b
// deviation, see noInjectModels).
func TestNoInjectModelsSkipsSystem(t *testing.T) {
	for _, m := range []string{"qwen/qwen-3.6-plus", "minimax/minimax-m2.5"} {
		r := &Request{
			Model: m,
			Messages: []Message{
				{Role: "user", Content: mustRaw(t, `"hello"`)},
			},
		}
		payload, err := r.ToUpstreamPayload("conv-ni")
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]interface{}
		if err := json.Unmarshal(payload, &body); err != nil {
			t.Fatal(err)
		}
		input := body["input"].([]interface{})
		if len(input) != 1 {
			t.Fatalf("[%s] input len = %d, want 1 (no injected system)", m, len(input))
		}
		if role := input[0].(map[string]interface{})["role"]; role != "user" {
			t.Errorf("[%s] first role = %v, want user", m, role)
		}

		// User-supplied system for an exempted model is kept in place.
		r2 := &Request{
			Model: m,
			Messages: []Message{
				{Role: "system", Content: mustRaw(t, `"Keep me."`)},
				{Role: "user", Content: mustRaw(t, `"hello"`)},
			},
		}
		payload2, err := r2.ToUpstreamPayload("conv-ni2")
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(payload2, &body); err != nil {
			t.Fatal(err)
		}
		input2 := body["input"].([]interface{})
		if len(input2) != 2 {
			t.Fatalf("[%s] sys-kept input len = %d, want 2", m, len(input2))
		}
		first := input2[0].(map[string]interface{})
		if first["role"] != "system" {
			t.Errorf("[%s] first role = %v, want system", m, first["role"])
		}
		if text := first["content"].([]interface{})[0].(map[string]interface{})["text"]; text != "Keep me." {
			t.Errorf("[%s] system text = %v, want 'Keep me.'", m, text)
		}
	}
}

// TestHead9 verifies the unlock header specification: exactly the first 9
// lines (429 chars) of the embedded full prompt, ending on the "MCP Tools:"
// line (SPEC system-inject §2.1b).
func TestHead9(t *testing.T) {
	full, err := os.ReadFile("anuma_system_prompt.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got := len(head9); got != 429 {
		t.Errorf("head9 length = %d, want 429", got)
	}
	if got := len(strings.Split(head9, "\n")); got != 9 {
		t.Errorf("head9 lines = %d, want 9", got)
	}
	if !strings.HasPrefix(string(full), head9) {
		t.Errorf("head9 is not a prefix of the embedded full prompt")
	}
	if !strings.HasSuffix(head9, "MCP Tools:") {
		t.Errorf("head9 should end on the 'MCP Tools:' line, got %q", head9[len(head9)-12:])
	}
}

// TestToUpstreamPayloadKeepsUserSystem verifies that a user-supplied system
// message is preserved and merged after the head9 unlock header, never
// overwritten (SPEC system-inject §2.1b: "用户 system 不被覆盖").
func TestToUpstreamPayloadKeepsUserSystem(t *testing.T) {
	r := &Request{
		Model: "anthropic/claude-sonnet-5",
		Messages: []Message{
			{Role: "system", Content: mustRaw(t, `"My custom system."`)},
			{Role: "user", Content: mustRaw(t, `"hello"`)},
		},
	}
	payload, err := r.ToUpstreamPayload("conv-sys2")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatal(err)
	}
	input := body["input"].([]interface{})
	if len(input) != 2 {
		t.Fatalf("input len = %d, want 2 (merged system + user)", len(input))
	}
	first := input[0].(map[string]interface{})
	if first["role"] != "system" {
		t.Fatalf("first role = %v, want system", first["role"])
	}
	blocks := first["content"].([]interface{})
	if len(blocks) != 1 {
		t.Fatalf("system content blocks = %d, want 1", len(blocks))
	}
	want := head9 + "\n\n" + "My custom system."
	if text := blocks[0].(map[string]interface{})["text"]; text != want {
		t.Errorf("system text = %q, want %q", text, want)
	}
	if role := input[1].(map[string]interface{})["role"]; role != "user" {
		t.Errorf("second role = %v, want user", role)
	}
}

func TestToUpstreamPayloadArrayContent(t *testing.T) {
	r := &Request{
		Model: "inclusionai/ling-2.6-flash",
		Messages: []Message{
			{Role: "user", Content: mustRaw(t, `[{"type":"text","text":"hi"}]`)},
		},
	}
	payload, err := r.ToUpstreamPayload("c2")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]interface{}
	_ = json.Unmarshal(payload, &body)
	input := body["input"].([]interface{})
	// Default system prompt is injected first (no system in request).
	if len(input) != 2 {
		t.Fatalf("input len = %d, want 2", len(input))
	}
	item := input[1].(map[string]interface{})
	blocks := item["content"].([]interface{})
	if len(blocks) != 1 {
		t.Fatalf("blocks = %d", len(blocks))
	}
}

func TestToUpstreamPayloadToolRoundTrip(t *testing.T) {
	r := &Request{
		Model: "inclusionai/ling-2.6-flash",
		Messages: []Message{
			{Role: "user", Content: mustRaw(t, `"weather in Beijing?"`)},
			{
				Role:    "assistant",
				Content: mustRaw(t, `""`),
				ToolCalls: []ToolCall{{
					ID:       "fc-1",
					Type:     "function",
					Function: ToolCallFunction{Name: "get_weather", Arguments: `{"city":"Beijing"}`},
				}},
			},
			{Role: "tool", Content: mustRaw(t, `"25C sunny"`), ToolCallID: "fc-1"},
		},
		Tools: []Tool{{
			Type: "function",
			Function: ToolDefinition{
				Name:        "get_weather",
				Description: "Get weather",
				Parameters:  mustRaw(t, `{"type":"object","properties":{"city":{"type":"string"}}}`),
			},
		}},
	}
	payload, err := r.ToUpstreamPayload("c3")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]interface{}
	_ = json.Unmarshal(payload, &body)
	input := body["input"].([]interface{})
	// Default system prompt is injected first (no system in request).
	if len(input) != 4 {
		t.Fatalf("input len = %d, want 4", len(input))
	}
	asst := input[2].(map[string]interface{})
	tcs, ok := asst["tool_calls"].([]interface{})
	if !ok || len(tcs) != 1 {
		t.Fatalf("assistant tool_calls = %v", asst["tool_calls"])
	}
	tc := tcs[0].(map[string]interface{})
	if tc["id"] != "fc-1" || tc["type"] != "function" {
		t.Errorf("tool call = %v", tc)
	}
	toolMsg := input[3].(map[string]interface{})
	if toolMsg["tool_call_id"] != "fc-1" {
		t.Errorf("tool_call_id = %v", toolMsg["tool_call_id"])
	}
	tools := body["tools"].([]interface{})
	if len(tools) != 1 {
		t.Fatalf("tools = %v", body["tools"])
	}
}

func TestUpstreamResponseToChat(t *testing.T) {
	output := []UpstreamOutput{
		{
			Type: "message",
			Content: []ContentBlock{
				{Type: "output_text", Text: "Hello"},
				{Type: "output_text", Text: " world"},
			},
		},
		{
			Type:      "function_call",
			Name:      "get_weather",
			CallID:    "fc-9",
			Arguments: `{"city":"Beijing"}`,
		},
	}
	cc := UpstreamResponseToChat("gen-1", "inclusionai/ling-2.6-flash", output, Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}, FinishHint{}, Session{})
	if cc.Object != "chat.completion" {
		t.Errorf("object = %v", cc.Object)
	}
	msg := cc.Choices[0].Message
	if msg.Content != "Hello world" {
		t.Errorf("content = %q", msg.Content)
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d", len(msg.ToolCalls))
	}
	if msg.ToolCalls[0].Function.Name != "get_weather" {
		t.Errorf("tool name = %v", msg.ToolCalls[0].Function.Name)
	}
	if cc.Choices[0].FinishReason == nil || *cc.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %v", cc.Choices[0].FinishReason)
	}
	if cc.Usage.TotalTokens != 15 {
		t.Errorf("usage = %+v", cc.Usage)
	}
}

func TestUpstreamResponseToChatPlain(t *testing.T) {
	output := []UpstreamOutput{{
		Type:    "message",
		Content: []ContentBlock{{Type: "output_text", Text: "Hello!"}},
	}}
	cc := UpstreamResponseToChat("gen-2", "m", output, Usage{}, FinishHint{}, Session{})
	if cc.Choices[0].Message.Content != "Hello!" {
		t.Errorf("content = %q", cc.Choices[0].Message.Content)
	}
	if cc.Choices[0].Message.ToolCalls != nil {
		t.Errorf("unexpected tool calls")
	}
	if cc.Choices[0].FinishReason == nil || *cc.Choices[0].FinishReason != "stop" {
		t.Errorf("finish = %v", cc.Choices[0].FinishReason)
	}
}

func TestStreamEvents(t *testing.T) {
	output := []UpstreamOutput{{
		Type:    "message",
		Content: []ContentBlock{{Type: "output_text", Text: "one two"}},
	}}
	events := StreamEvents("chatcmpl-1", "inclusionai/ling-2.6-flash", output, Usage{}, false, FinishHint{}, Session{})
	if len(events) < 4 {
		t.Fatalf("expected at least 4 events, got %d", len(events))
	}
	var first struct {
		Choices []struct {
			Delta struct {
				Role string `json:"role"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(events[0], &first); err != nil {
		t.Fatal(err)
	}
	if first.Choices[0].Delta.Role != "assistant" {
		t.Errorf("first delta role = %q", first.Choices[0].Delta.Role)
	}
	var content struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(events[1], &content); err != nil {
		t.Fatal(err)
	}
	if content.Choices[0].Delta.Content != "one two" {
		t.Errorf("content = %q", content.Choices[0].Delta.Content)
	}
	var fin struct {
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(events[2], &fin); err != nil {
		t.Fatal(err)
	}
	if fin.Choices[0].FinishReason == nil || *fin.Choices[0].FinishReason != "stop" {
		t.Errorf("finish = %v", fin.Choices[0].FinishReason)
	}
	if string(events[len(events)-1]) != "[DONE]" {
		t.Errorf("last = %q", events[len(events)-1])
	}
}

func TestStreamEventsToolCall(t *testing.T) {
	output := []UpstreamOutput{{
		Type:      "function_call",
		Name:      "get_weather",
		CallID:    "fc-7",
		Arguments: `{"city":"Paris"}`,
	}}
	events := StreamEvents("chatcmpl-2", "m", output, Usage{}, false, FinishHint{}, Session{})
	foundTool := false
	var finish string
	for _, ev := range events {
		var probe struct {
			Choices []struct {
				Delta struct {
					ToolCalls []ToolCall `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(ev, &probe); err != nil {
			continue
		}
		if len(probe.Choices) > 0 {
			if len(probe.Choices[0].Delta.ToolCalls) > 0 {
				foundTool = true
				tc := probe.Choices[0].Delta.ToolCalls[0]
				if tc.Function.Name != "get_weather" || tc.ID != "fc-7" {
					t.Errorf("tool call = %+v", tc)
				}
			}
			if probe.Choices[0].FinishReason != nil {
				finish = *probe.Choices[0].FinishReason
			}
		}
	}
	if !foundTool {
		t.Errorf("no tool call chunk emitted")
	}
	if finish != "tool_calls" {
		t.Errorf("finish = %q, want tool_calls", finish)
	}
}

func TestFormatSSE(t *testing.T) {
	out := FormatSSE([]byte(`{"a":1}`))
	if !strings.HasPrefix(string(out), "data: ") {
		t.Errorf("missing data prefix: %q", out)
	}
	if !strings.HasSuffix(string(out), "\n\n") {
		t.Errorf("missing blank line: %q", out)
	}
}

// TestWantUsage verifies stream_options.include_usage parsing (SPEC §2.1).
func TestWantUsage(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{`{"stream":true,"stream_options":{"include_usage":true}}`, true},
		{`{"stream":true,"stream_options":{"include_usage":false}}`, false},
		{`{"stream":true,"stream_options":{}}`, false},
		{`{"stream":true}`, false},
	}
	for _, c := range cases {
		var r Request
		if err := json.Unmarshal([]byte(c.body), &r); err != nil {
			t.Fatalf("unmarshal %q: %v", c.body, err)
		}
		if got := r.WantUsage(); got != c.want {
			t.Errorf("WantUsage() for %q = %v, want %v", c.body, got, c.want)
		}
	}
}

// TestStreamEventsIncludeUsage verifies that with include_usage=true the final
// chunk (the one with finish_reason) carries the upstream usage with the
// correct token counts, and no earlier chunk does (SPEC §2.2).
func TestStreamEventsIncludeUsage(t *testing.T) {
	output := []UpstreamOutput{{
		Type:    "message",
		Content: []ContentBlock{{Type: "output_text", Text: "one two"}},
	}}
	usage := Usage{PromptTokens: 12, CompletionTokens: 7, TotalTokens: 19}
	events := StreamEvents("chatcmpl-3", "inclusionai/ling-2.6-flash", output, usage, true, FinishHint{}, Session{})
	if len(events) < 4 {
		t.Fatalf("expected at least 4 events, got %d", len(events))
	}
	if string(events[len(events)-1]) != "[DONE]" {
		t.Errorf("last event = %q, want [DONE]", events[len(events)-1])
	}

	// The final data event (before [DONE]) is the finish chunk with usage.
	var fin struct {
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *Usage `json:"usage"`
	}
	if err := json.Unmarshal(events[len(events)-2], &fin); err != nil {
		t.Fatal(err)
	}
	if fin.Choices[0].FinishReason == nil || *fin.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %v, want stop", fin.Choices[0].FinishReason)
	}
	if fin.Usage == nil {
		t.Fatal("expected usage on final chunk, got nil")
	}
	if fin.Usage.PromptTokens != 12 || fin.Usage.CompletionTokens != 7 || fin.Usage.TotalTokens != 19 {
		t.Errorf("usage = %+v, want prompt=12 completion=7 total=19", fin.Usage)
	}

	// No earlier chunk may carry usage.
	for i, ev := range events[:len(events)-2] {
		var probe struct {
			Usage *Usage `json:"usage"`
		}
		if err := json.Unmarshal(ev, &probe); err != nil {
			t.Fatalf("event %d not JSON: %s", i, ev)
		}
		if probe.Usage != nil {
			t.Errorf("event %d unexpectedly carries usage: %s", i, ev)
		}
	}
}

// TestStreamEventsNoUsage verifies the stream stays usage-free when the client
// did not request include_usage (SPEC §2.2, protocol default compatibility).
func TestStreamEventsNoUsage(t *testing.T) {
	output := []UpstreamOutput{{
		Type:    "message",
		Content: []ContentBlock{{Type: "output_text", Text: "hello"}},
	}}
	events := StreamEvents("chatcmpl-4", "m", output, Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}, false, FinishHint{}, Session{})
	for i, ev := range events {
		if string(ev) == "[DONE]" {
			continue
		}
		var probe struct {
			Usage *Usage `json:"usage"`
		}
		if err := json.Unmarshal(ev, &probe); err != nil {
			t.Fatalf("event %d not JSON: %s", i, ev)
		}
		if probe.Usage != nil {
			t.Errorf("event %d has usage when not requested: %s", i, ev)
		}
	}
}

// TestNonStreamUnaffected verifies the non-streaming response still carries
// usage exactly as before (SPEC §2.3).
func TestNonStreamUnaffected(t *testing.T) {
	output := []UpstreamOutput{{
		Type:    "message",
		Content: []ContentBlock{{Type: "output_text", Text: "hi"}},
	}}
	cc := UpstreamResponseToChat("gen-3", "m", output, Usage{PromptTokens: 3, CompletionTokens: 4, TotalTokens: 7}, FinishHint{}, Session{})
	if cc.Usage.TotalTokens != 7 {
		t.Errorf("non-stream usage = %+v, want total=7", cc.Usage)
	}
	if cc.Choices[0].FinishReason == nil || *cc.Choices[0].FinishReason != "stop" {
		t.Errorf("finish = %v", cc.Choices[0].FinishReason)
	}
}

// TestPromptTokensDetailsSerialization verifies the OpenAI-standard
// prompt_tokens_details object serializes as {"cached_tokens":N} when set and
// is omitted (omitempty) when nil (SPEC-usage-fix P0-2 §2.2).
func TestPromptTokensDetailsSerialization(t *testing.T) {
	// Set: the field must appear with the exact cached_tokens value.
	withDetails := Usage{
		PromptTokens:        10,
		CompletionTokens:    5,
		TotalTokens:         15,
		PromptTokensDetails: &PromptTokensDetails{CachedTokens: 120},
	}
	b, err := json.Marshal(withDetails)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	details, ok := m["prompt_tokens_details"].(map[string]interface{})
	if !ok {
		t.Fatalf("prompt_tokens_details missing or wrong type in %s", b)
	}
	if details["cached_tokens"] != float64(120) {
		t.Errorf("cached_tokens = %v, want 120", details["cached_tokens"])
	}

	// Unset (nil): omitempty drops the key entirely.
	without := Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}
	b2, err := json.Marshal(without)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b2), "prompt_tokens_details") {
		t.Errorf("nil details must be omitted by omitempty, got %s", b2)
	}
}

// ---- P0-A1: finish_reason 透传三态映射 ----

// TestResolveFinishReasonThreeStates verifies P0-A1: the finish_reason is
// resolved from the FinishHint across the three terminal states — length
// truncation, normal stop, and tool_calls — covering both data shapes the
// upstream can present (with an explicit incomplete_details.reason, and without
// where the cap-table inference is the only signal).
func TestResolveFinishReasonThreeStates(t *testing.T) {
	cases := []struct {
		name string
		hint FinishHint
		want string
	}{
		{
			name: "length via cap-table (no stop_reason, tokens at cap)",
			// claude-sonnet-5 cap is 1024; completion_tokens==1024 -> length.
			hint: FinishHint{Model: "anthropic/claude-sonnet-5", CompletionTokens: 1024},
			want: "length",
		},
		{
			name: "length via explicit incomplete_details.reason (future upstream)",
			hint: FinishHint{IncompleteReason: "max_output_tokens"},
			want: "length",
		},
		{
			name: "length via explicit 'length' reason",
			hint: FinishHint{IncompleteReason: "length"},
			want: "length",
		},
		{
			name: "stop: tokens below cap, no incomplete reason",
			hint: FinishHint{Model: "anthropic/claude-sonnet-5", CompletionTokens: 800},
			want: "stop",
		},
		{
			name: "stop: unknown model never inferred as length",
			hint: FinishHint{Model: "some/unknown-model", CompletionTokens: 99999},
			want: "stop",
		},
		{
			name: "tool_calls wins over length",
			hint: FinishHint{Model: "anthropic/claude-sonnet-5", CompletionTokens: 1024, OutputHasToolCall: true},
			want: "tool_calls",
		},
		{
			name: "tool_calls via output flag alone",
			hint: FinishHint{OutputHasToolCall: true},
			want: "tool_calls",
		},
		{
			name: "zero hint -> stop",
			hint: FinishHint{},
			want: "stop",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveFinishReason(c.hint); got != c.want {
				t.Errorf("resolveFinishReason(%+v) = %q, want %q", c.hint, got, c.want)
			}
		})
	}
}

// TestUpstreamResponseToChatLength verifies P0-A1 non-streaming path: a
// length-truncated upstream response (completion_tokens at the model cap) is
// surfaced as finish_reason="length", not masked as "stop".
func TestUpstreamResponseToChatLength(t *testing.T) {
	output := []UpstreamOutput{{
		Type:    "message",
		Content: []ContentBlock{{Type: "output_text", Text: "被截断的输出..."}},
	}}
	hint := FinishHint{Model: "anthropic/claude-sonnet-5", CompletionTokens: 1024}
	cc := UpstreamResponseToChat("gen-len", "anthropic/claude-sonnet-5", output, Usage{CompletionTokens: 1024}, hint, Session{})
	if cc.Choices[0].FinishReason == nil || *cc.Choices[0].FinishReason != "length" {
		t.Errorf("finish_reason = %v, want length (cap-truncated)", cc.Choices[0].FinishReason)
	}
}

// TestStreamEventsLength verifies P0-A1 streaming path: a length-truncated
// completion's final chunk carries finish_reason="length".
func TestStreamEventsLength(t *testing.T) {
	output := []UpstreamOutput{{
		Type:    "message",
		Content: []ContentBlock{{Type: "output_text", Text: "被截断"}},
	}}
	hint := FinishHint{Model: "anthropic/claude-sonnet-5", CompletionTokens: 1024}
	events := StreamEvents("chatcmpl-len", "anthropic/claude-sonnet-5", output, Usage{}, false, hint, Session{})
	var fin struct {
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(events[len(events)-2], &fin); err != nil {
		t.Fatal(err)
	}
	if fin.Choices[0].FinishReason == nil || *fin.Choices[0].FinishReason != "length" {
		t.Errorf("finish_reason = %v, want length", fin.Choices[0].FinishReason)
	}
}

// TestStreamEventsLengthShortModel verifies the cap-table lookup also accepts
// the short model name the upstream actually returns ("claude-sonnet-5"),
// without the vendor prefix the table is keyed by — regression for the live
// probe finding (upstream response.model is the short form).
func TestStreamEventsLengthShortModel(t *testing.T) {
	output := []UpstreamOutput{{
		Type:    "message",
		Content: []ContentBlock{{Type: "output_text", Text: "被截断"}},
	}}
	hint := FinishHint{Model: "claude-sonnet-5", CompletionTokens: 1024}
	events := StreamEvents("chatcmpl-len2", "claude-sonnet-5", output, Usage{}, false, hint, Session{})
	var fin struct {
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(events[len(events)-2], &fin); err != nil {
		t.Fatal(err)
	}
	if fin.Choices[0].FinishReason == nil || *fin.Choices[0].FinishReason != "length" {
		t.Errorf("finish_reason = %v, want length (short model name)", fin.Choices[0].FinishReason)
	}
}

// TestModelOutputCapTable verifies every entry measured in ticket 11
// (2026-08-04) is present in modelOutputCap with the measured value, and that
// resolveFinishReason surfaces "length" when completion_tokens reaches the cap.
// This is the unit-test guard for the cap table: adding a cap without a test
// here will fail the suite. The gemini pair stops at exactly 1020 (not 1024),
// so its cap is 1020 — completion==1020 must map to length, and completion==1024
// must NOT be required (regression against recording 1024 by mistake).
func TestModelOutputCapTable(t *testing.T) {
	cases := []struct {
		model string // short name the upstream response.model returns
		cap   int    // measured hard ceiling
	}{
		{"ling-2.6-flash", 1024},
		{"kimi-k3", 1024},
		{"gpt-5.6-luna", 1024},
		{"qwen-3.7-plus", 1024},
		{"glm-5.2", 1024},
		{"minimax-m3", 1024},
		{"claude-sonnet-5", 1024},
		{"gemini-3.1-pro-preview", 1020},
		{"gemini-3-flash-preview", 1020},
	}
	for _, c := range cases {
		t.Run(c.model, func(t *testing.T) {
			got, ok := modelOutputCap[c.model]
			if !ok {
				t.Fatalf("modelOutputCap missing entry for %q (add it after measuring the cap)", c.model)
			}
			if got != c.cap {
				t.Errorf("modelOutputCap[%q] = %d, want %d", c.model, got, c.cap)
			}
			// completion at the cap -> length (the truncation signal).
			if r := resolveFinishReason(FinishHint{Model: c.model, CompletionTokens: c.cap}); r != "length" {
				t.Errorf("completion==cap: resolveFinishReason(%q, %d) = %q, want length", c.model, c.cap, r)
			}
			// one token below the cap -> stop (not yet truncated).
			if r := resolveFinishReason(FinishHint{Model: c.model, CompletionTokens: c.cap - 1}); r != "stop" {
				t.Errorf("completion==cap-1: resolveFinishReason(%q, %d) = %q, want stop", c.model, c.cap-1, r)
			}
		})
	}
}

// TestModelOutputCapGemini1020Not1024 guards the gemini finding: the upstream
// stops at exactly 1020, so the cap MUST be 1020. If someone "rounds" it to
// 1024, a 1020-token truncation would be masked as stop — this test fails.
func TestModelOutputCapGemini1020Not1024(t *testing.T) {
	for _, m := range []string{"gemini-3.1-pro-preview", "gemini-3-flash-preview"} {
		// Live probe: completion_tokens==1020 (3 consecutive runs). Must be length.
		if r := resolveFinishReason(FinishHint{Model: m, CompletionTokens: 1020}); r != "length" {
			t.Errorf("%q completion=1020 -> %q, want length (cap must be 1020, not 1024)", m, r)
		}
	}
}

// TestResolveFinishReasonGrokNoCap verifies grok-4.5, which has no measured hard
// cap (natural end at non-round 1230/1442/1070), stays "stop" even at high
// completion counts — no false length inference for an uncapped model.
func TestResolveFinishReasonGrokNoCap(t *testing.T) {
	for _, comp := range []int{1230, 1442, 1070, 99999} {
		if r := resolveFinishReason(FinishHint{Model: "grok-4.5", CompletionTokens: comp}); r != "stop" {
			t.Errorf("grok-4.5 completion=%d -> %q, want stop (no cap)", comp, r)
		}
	}
	// Also confirm the long name resolves the same (cap table keyed by short name,
	// isLengthTruncated strips the vendor prefix).
	if r := resolveFinishReason(FinishHint{Model: "grok/grok-4.5", CompletionTokens: 1442}); r != "stop" {
		t.Errorf("grok/grok-4.5 completion=1442 -> %q, want stop", r)
	}
}

// TestStreamEventsStopNoCap verifies a normal completion on an uncapped model
// stays "stop" (no false length inference).
func TestStreamEventsStopNoCap(t *testing.T) {
	output := []UpstreamOutput{{
		Type:    "message",
		Content: []ContentBlock{{Type: "output_text", Text: "完整输出"}},
	}}
	hint := FinishHint{Model: "inclusionai/ling-2.6-flash", CompletionTokens: 42}
	events := StreamEvents("chatcmpl-stop", "inclusionai/ling-2.6-flash", output, Usage{}, false, hint, Session{})
	var fin struct {
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(events[len(events)-2], &fin); err != nil {
		t.Fatal(err)
	}
	if fin.Choices[0].FinishReason == nil || *fin.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %v, want stop", fin.Choices[0].FinishReason)
	}
}

// TestStreamEventsToolCallWithHint verifies the tool_calls state is preserved
// even when a FinishHint is supplied (output flag authoritative).
func TestStreamEventsToolCallWithHint(t *testing.T) {
	output := []UpstreamOutput{{
		Type:      "function_call",
		Name:      "get_weather",
		CallID:    "fc-7",
		Arguments: `{"city":"Paris"}`,
	}}
	hint := FinishHint{Model: "anthropic/claude-sonnet-5", CompletionTokens: 1024}
	events := StreamEvents("chatcmpl-tc", "anthropic/claude-sonnet-5", output, Usage{}, false, hint, Session{})
	var finish string
	for _, ev := range events {
		var probe struct {
			Choices []struct {
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(ev, &probe); err != nil {
			continue
		}
		if len(probe.Choices) > 0 && probe.Choices[0].FinishReason != nil {
			finish = *probe.Choices[0].FinishReason
		}
	}
	if finish != "tool_calls" {
		t.Errorf("finish = %q, want tool_calls (tool call wins over length)", finish)
	}
}

// ---- ticket 12: 续写协议 conversation_id / previous_response_id ----

// TestRequestJSONFields verifies the Chat Completions request body accepts the
// OpenAI-extension continuation fields conversation_id and previous_response_id
// at the top level.
func TestRequestJSONFields(t *testing.T) {
	body := `{"model":"claude-sonnet-5","conversation_id":"conv-abc","previous_response_id":"msg_prev_123","messages":[{"role":"user","content":"hi"}]}`
	var r Request
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatal(err)
	}
	if r.ConversationID != "conv-abc" {
		t.Errorf("ConversationID = %q, want conv-abc", r.ConversationID)
	}
	if r.PreviousResponseID != "msg_prev_123" {
		t.Errorf("PreviousResponseID = %q, want msg_prev_123", r.PreviousResponseID)
	}
	// A request without them leaves the fields empty.
	var r2 Request
	if err := json.Unmarshal([]byte(`{"model":"m","messages":[]}`), &r2); err != nil {
		t.Fatal(err)
	}
	if r2.ConversationID != "" || r2.PreviousResponseID != "" {
		t.Errorf("empty request fields = %q/%q, want empty", r2.ConversationID, r2.PreviousResponseID)
	}
}

// TestToUpstreamPayloadContinuation verifies the request side of ticket 12: a
// client-supplied conversation_id is forwarded verbatim and previous_response_id
// is added to the upstream payload; without them the payload keeps only the
// caller-provided conversation_id and no previous_response_id.
func TestToUpstreamPayloadContinuation(t *testing.T) {
	r := &Request{
		Model:              "anthropic/claude-sonnet-5",
		ConversationID:     "conv-client-1",
		PreviousResponseID: "msg_prev_1",
		Messages:           []Message{{Role: "user", Content: mustRaw(t, `"continue"`)}},
	}
	payload, err := r.ToUpstreamPayload(r.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatal(err)
	}
	if body["conversation_id"] != "conv-client-1" {
		t.Errorf("conversation_id = %v, want conv-client-1", body["conversation_id"])
	}
	if body["previous_response_id"] != "msg_prev_1" {
		t.Errorf("previous_response_id = %v, want msg_prev_1", body["previous_response_id"])
	}

	// Without client fields: conversation_id is whatever the caller passed, and
	// no previous_response_id key is present.
	r2 := &Request{
		Model:    "anthropic/claude-sonnet-5",
		Messages: []Message{{Role: "user", Content: mustRaw(t, `"hi"`)}},
	}
	payload2, err := r2.ToUpstreamPayload("conv-server-1")
	if err != nil {
		t.Fatal(err)
	}
	var body2 map[string]interface{}
	if err := json.Unmarshal(payload2, &body2); err != nil {
		t.Fatal(err)
	}
	if body2["conversation_id"] != "conv-server-1" {
		t.Errorf("conversation_id = %v, want conv-server-1", body2["conversation_id"])
	}
	if _, ok := body2["previous_response_id"]; ok {
		t.Errorf("previous_response_id present without client value: %v", body2["previous_response_id"])
	}
}

// TestUpstreamResponseToChatContinuation verifies the non-streaming response
// side: the chat completion carries conversation_id (echoed from the request)
// and previous_response_id (= the upstream response id).
func TestUpstreamResponseToChatContinuation(t *testing.T) {
	output := []UpstreamOutput{{
		Type:    "message",
		Content: []ContentBlock{{Type: "output_text", Text: "continued"}},
	}}
	session := Session{ConversationID: "conv-client-1", PreviousResponseID: "msg_resp_9", HasConversationID: true, HasPreviousResponse: true}
	cc := UpstreamResponseToChat("msg_resp_9", "anthropic/claude-sonnet-5", output, Usage{}, FinishHint{}, session)
	if cc.ConversationID != "conv-client-1" {
		t.Errorf("ConversationID = %q, want conv-client-1", cc.ConversationID)
	}
	if cc.PreviousResponseID != "msg_resp_9" {
		t.Errorf("PreviousResponseID = %q, want msg_resp_9", cc.PreviousResponseID)
	}
}

// TestUpstreamResponseToChatNoSession verifies the continuation fields are
// omitted entirely (omitempty) when the session is empty — the standard OpenAI
// shape stays unchanged for ordinary requests.
func TestUpstreamResponseToChatNoSession(t *testing.T) {
	output := []UpstreamOutput{{
		Type:    "message",
		Content: []ContentBlock{{Type: "output_text", Text: "plain"}},
	}}
	cc := UpstreamResponseToChat("msg_resp_1", "m", output, Usage{}, FinishHint{}, Session{})
	if cc.ConversationID != "" || cc.PreviousResponseID != "" {
		t.Errorf("empty session leaked fields: %q/%q", cc.ConversationID, cc.PreviousResponseID)
	}
	raw, _ := json.Marshal(cc)
	if strings.Contains(string(raw), "conversation_id") || strings.Contains(string(raw), "previous_response_id") {
		t.Errorf("omitempty not honored for empty session: %s", raw)
	}
}

// TestStreamEventsContinuation verifies the streaming response side: the final
// finish chunk carries conversation_id and previous_response_id, and no earlier
// chunk does.
func TestStreamEventsContinuation(t *testing.T) {
	output := []UpstreamOutput{{
		Type:    "message",
		Content: []ContentBlock{{Type: "output_text", Text: "one two"}},
	}}
	session := Session{ConversationID: "conv-client-1", PreviousResponseID: "msg_resp_9", HasConversationID: true, HasPreviousResponse: true}
	events := StreamEvents("chatcmpl-cont", "anthropic/claude-sonnet-5", output, Usage{}, false, FinishHint{}, session)
	if string(events[len(events)-1]) != "[DONE]" {
		t.Fatalf("last event = %q, want [DONE]", events[len(events)-1])
	}
	var fin struct {
		ConversationID     string `json:"conversation_id"`
		PreviousResponseID string `json:"previous_response_id"`
	}
	if err := json.Unmarshal(events[len(events)-2], &fin); err != nil {
		t.Fatal(err)
	}
	if fin.ConversationID != "conv-client-1" {
		t.Errorf("final chunk conversation_id = %q, want conv-client-1", fin.ConversationID)
	}
	if fin.PreviousResponseID != "msg_resp_9" {
		t.Errorf("final chunk previous_response_id = %q, want msg_resp_9", fin.PreviousResponseID)
	}
	// No earlier chunk carries the ids (all but the last data event and [DONE]).
	for i, ev := range events[:len(events)-2] {
		var probe struct {
			ConversationID     string `json:"conversation_id"`
			PreviousResponseID string `json:"previous_response_id"`
		}
		if err := json.Unmarshal(ev, &probe); err != nil {
			t.Fatalf("event %d not JSON: %s", i, ev)
		}
		if probe.ConversationID != "" || probe.PreviousResponseID != "" {
			t.Errorf("event %d unexpectedly carries continuation ids: %s", i, ev)
		}
	}
}

// TestStreamEventsContinuationEmptySession verifies an empty session leaves the
// stream free of the extension fields.
func TestStreamEventsContinuationEmptySession(t *testing.T) {
	output := []UpstreamOutput{{
		Type:    "message",
		Content: []ContentBlock{{Type: "output_text", Text: "hi"}},
	}}
	events := StreamEvents("chatcmpl-plain", "m", output, Usage{}, false, FinishHint{}, Session{})
	for i, ev := range events {
		if string(ev) == "[DONE]" {
			continue
		}
		if strings.Contains(string(ev), "conversation_id") || strings.Contains(string(ev), "previous_response_id") {
			t.Errorf("event %d leaked continuation fields: %s", i, ev)
		}
	}
}

// TestUpstreamResponseToChatLengthWithSession verifies the length-truncation
// signal and the continuation identifiers coexist on the non-streaming path
// (the exact shape a client needs after finish_reason=length).
func TestUpstreamResponseToChatLengthWithSession(t *testing.T) {
	output := []UpstreamOutput{{
		Type:    "message",
		Content: []ContentBlock{{Type: "output_text", Text: "截断..."}},
	}}
	hint := FinishHint{Model: "anthropic/claude-sonnet-5", CompletionTokens: 1024}
	session := Session{ConversationID: "conv-len", PreviousResponseID: "msg_len_1", HasConversationID: true, HasPreviousResponse: true}
	cc := UpstreamResponseToChat("msg_len_1", "anthropic/claude-sonnet-5", output, Usage{CompletionTokens: 1024}, hint, session)
	if cc.Choices[0].FinishReason == nil || *cc.Choices[0].FinishReason != "length" {
		t.Errorf("finish_reason = %v, want length", cc.Choices[0].FinishReason)
	}
	if cc.PreviousResponseID != "msg_len_1" || cc.ConversationID != "conv-len" {
		t.Errorf("continuation ids = %q/%q, want conv-len/msg_len_1", cc.ConversationID, cc.PreviousResponseID)
	}
}

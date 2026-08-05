package convert

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Chunk is an SSE chunk object emitted for streaming completions (SPEC 5.2).
type Chunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	// Usage is attached to the final chunk only when the client asked for it
	// via stream_options.include_usage (SPEC: stream usage).
	Usage *Usage `json:"usage,omitempty"`
	// ConversationID and PreviousResponseID are the continuation identifiers
	// (ticket 12). They are attached to the final finish chunk, next to usage,
	// so a client that reads the stream to completion can issue the next
	// "continue" request. Both are omitted when empty.
	ConversationID     string `json:"conversation_id,omitempty"`
	PreviousResponseID string `json:"previous_response_id,omitempty"`
}

// ChunkChoice carries the delta and finish reason.
type ChunkChoice struct {
	Index        int             `json:"index"`
	Delta        json.RawMessage `json:"delta"`
	FinishReason *string         `json:"finish_reason"`
}

// ChunkDelta is the mutable part of a chunk.
type ChunkDelta struct {
	Role      string     `json:"role,omitempty"`
	Content   string     `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// SSEChunk returns a single OpenAI chat.completion.chunk event body. When
// usage is non-nil it is attached to the chunk (stream_options.include_usage).
// session carries the continuation identifiers (ticket 12): they are attached
// to the final finish chunk, next to usage, so a client that reads the stream
// to completion can issue the next "continue" request.
func SSEChunk(id, model string, delta ChunkDelta, finish *string, usage *Usage, session Session) []byte {
	rawDelta, _ := json.Marshal(delta)
	c := Chunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []ChunkChoice{{
			Index:        0,
			Delta:        rawDelta,
			FinishReason: finish,
		}},
		Usage: usage,
	}
	// The continuation ids ride the final finish chunk. Keep them omitted on
	// every other chunk (empty string => omitempty) so the stream shape stays
	// unchanged for ordinary requests.
	if session.HasConversationID && session.ConversationID != "" {
		c.ConversationID = session.ConversationID
	}
	if session.HasPreviousResponse && session.PreviousResponseID != "" {
		c.PreviousResponseID = session.PreviousResponseID
	}
	out, _ := json.Marshal(c)
	return out
}

// StreamEvents converts the single upstream streamed response object (SPEC
// 3.4) into a sequence of OpenAI SSE data payloads, ending with "[DONE]".
// Because the upstream only sends one full object, the gateway emits a role
// prefix chunk, one content chunk per text block, one chunk per tool call, a
// final finish chunk, and [DONE]. When includeUsage is true the finish chunk
// also carries the usage object (SPEC: stream usage); otherwise the stream
// stays usage-free as before. The finish_reason is resolved from hint (P0-A1):
// a length-truncated completion is surfaced as "length" instead of "stop".
func StreamEvents(id, model string, output []UpstreamOutput, usage Usage, includeUsage bool, hint FinishHint, session Session) [][]byte {
	var events [][]byte

	// 1. role prefix
	events = append(events, SSEChunk(id, model, ChunkDelta{Role: "assistant", Content: ""}, nil, nil, Session{}))

	// 2. content + tool calls
	for _, item := range output {
		switch item.Type {
		case "message":
			for _, b := range item.Content {
				if b.Type == "output_text" || b.Type == "text" {
					if b.Text != "" {
						events = append(events, SSEChunk(id, model, ChunkDelta{Content: b.Text}, nil, nil, Session{}))
					}
				}
			}
		case "function_call":
			events = append(events, SSEChunk(id, model, ChunkDelta{
				ToolCalls: []ToolCall{{
					ID:   item.CallID,
					Type: "function",
					Function: ToolCallFunction{
						Name:      item.Name,
						Arguments: item.Arguments,
					},
				}},
			}, nil, nil, Session{}))
		}
	}

	// 3. finish (optionally carrying usage when the client requested it).
	// Resolve the real finish_reason from the upstream signal instead of
	// hardcoding "stop" (P0-A1): a length-truncated completion is now
	// "length", a tool call is "tool_calls", otherwise "stop". The continuation
	// identifiers (ticket 12) ride this final chunk.
	hint.OutputHasToolCall = hint.OutputHasToolCall || hasFunctionCall(output)
	finish := resolveFinishReason(hint)
	var usagePtr *Usage
	if includeUsage {
		usagePtr = &usage
	}
	events = append(events, SSEChunk(id, model, ChunkDelta{}, &finish, usagePtr, session))

	// 4. [DONE]
	events = append(events, []byte("[DONE]"))
	return events
}

// hasFunctionCall reports whether any output item is a function_call.
func hasFunctionCall(output []UpstreamOutput) bool {
	for _, item := range output {
		if item.Type == "function_call" {
			return true
		}
	}
	return false
}

// SSEFromResponse converts a full upstream Response into a list of SSE data
// lines, one per event. includeUsage mirrors StreamEvents (SPEC: stream usage).
// The finish_reason is resolved from hint (P0-A1). session carries the
// continuation identifiers (ticket 12) attached to the final chunk.
func SSEFromResponse(id, model string, output []UpstreamOutput, usage Usage, includeUsage bool, hint FinishHint, session Session) []string {
	events := StreamEvents(id, model, output, usage, includeUsage, hint, session)
	lines := make([]string, 0, len(events))
	for _, ev := range events {
		lines = append(lines, "data: "+string(ev))
	}
	return lines
}

// FormatSSE writes a single SSE frame with trailing blank line.
func FormatSSE(payload []byte) []byte {
	return append(append([]byte("data: "), payload...), '\n', '\n')
}

// CollectStreamText joins the text content of a streamed upstream response.
func CollectStreamText(output []UpstreamOutput) string {
	var sb strings.Builder
	for _, item := range output {
		if item.Type == "message" {
			for _, b := range item.Content {
				if b.Type == "output_text" || b.Type == "text" {
					sb.WriteString(b.Text)
				}
			}
		}
	}
	return sb.String()
}

// DescribeUpstreamOutput returns a compact debug string without any content.
func DescribeUpstreamOutput(output []UpstreamOutput) string {
	var kinds []string
	for _, item := range output {
		kinds = append(kinds, item.Type)
	}
	return fmt.Sprintf("[%s]", strings.Join(kinds, ", "))
}

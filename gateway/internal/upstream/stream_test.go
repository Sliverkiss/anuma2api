package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// streamSSE builds an SSE body from raw data payloads, each prefixed with
// "data: " and terminated by "\n\n" (matching the upstream's framing).
func streamSSE(payloads ...string) string {
	var sb strings.Builder
	for _, p := range payloads {
		sb.WriteString("data: " + p + "\n\n")
	}
	return sb.String()
}

func sseEvent(typ string, resp *Response, err *RespError) string {
	evt := map[string]interface{}{"type": typ}
	if resp != nil {
		evt["response"] = resp
	}
	if err != nil {
		evt["error"] = err
	}
	b, _ := json.Marshal(evt)
	return string(b)
}

// TestStreamYieldsCompletedOnly verifies the streaming parser absorbs the
// OpenAI Responses API intermediate events and yields exactly one complete
// Response from the final response.completed event (the 503 root cause: the
// response.created skeleton event was being treated as a real response and its
// empty error:{} as a failure).
func TestStreamYieldsCompletedOnly(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		// Real upstream sequence measured 2026-08-03: intermediate events carry
		// skeleton response objects with an EMPTY (non-nil) error object, then
		// response.completed carries the full output.
		created := &Response{ID: "resp-1", Model: ""}
		created.Error = &RespError{}
		completed := &Response{
			ID:    "resp-1",
			Model: "anthropic/claude-sonnet-5",
			Output: []OutputItem{{
				Type:    "message",
				Content: []ContentBlock{{Type: "output_text", Text: "hi there"}},
			}},
			Usage: Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		}
		fmt.Fprint(w, streamSSE(
			sseEvent("response.created", created, nil),
			sseEvent("response.in_progress", &Response{ID: "resp-1"}, nil),
			sseEvent("response.output_text.delta", nil, nil),
			sseEvent("response.completed", completed, nil),
			"[DONE]",
		))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "https://auth.privy.io/api/v1", time.Minute)
	ch, closer, err := c.Stream(context.Background(), "tok", []byte(`{"stream":true}`))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer closer.Close()

	select {
	case resp := <-ch:
		if resp == nil {
			t.Fatal("nil response")
		}
		if resp.Error != nil {
			t.Fatalf("unexpected error on completed event: %+v", resp.Error)
		}
		if resp.ID != "resp-1" || resp.Model != "anthropic/claude-sonnet-5" {
			t.Errorf("completed response = %s / %s", resp.ID, resp.Model)
		}
		if len(resp.Output) != 1 || resp.Output[0].Content[0].Text != "hi there" {
			t.Errorf("output = %+v", resp.Output)
		}
		if resp.Usage.TotalTokens != 15 {
			t.Errorf("usage = %+v", resp.Usage)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the completed response")
	}

	// Channel must be closed after the single completed event.
	if _, ok := <-ch; ok {
		t.Errorf("channel still open after completed event; want closed")
	}
	if gotBody == nil {
		t.Error("upstream never received the request body")
	}
}

// TestStreamFailedEventSurfacesError verifies a response.failed event (or a
// non-stream legacy event with a top-level error) is surfaced as an error.
func TestStreamFailedEventSurfacesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, streamSSE(
			sseEvent("response.failed", nil, &RespError{Message: "boom", Code: "internal_error"}),
			"[DONE]",
		))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "https://auth.privy.io/api/v1", time.Minute)
	ch, closer, err := c.Stream(context.Background(), "tok", []byte(`{"stream":true}`))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer closer.Close()

	select {
	case resp := <-ch:
		if resp == nil || resp.Error == nil {
			t.Fatalf("expected error response, got %+v", resp)
		}
		if resp.Error.Code != "internal_error" || resp.Error.Message != "boom" {
			t.Errorf("error = %+v", resp.Error)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for failed event")
	}
}

// TestStreamHTTPError verifies a non-200 upstream response is classified.
func TestStreamHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":{"message":"forbidden","code":"forbidden","type":"authorization_error"}}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "https://auth.privy.io/api/v1", time.Minute)
	_, _, err := c.Stream(context.Background(), "tok", []byte(`{"stream":true}`))
	if err == nil {
		t.Fatal("expected error for HTTP 403")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("err type = %T, want *APIError", err)
	}
	if apiErr.Kind != ErrForbidden {
		t.Errorf("kind = %v, want forbidden", apiErr.Kind)
	}
}

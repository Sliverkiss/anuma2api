package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"anuma2api/gateway/internal/convert"
	"anuma2api/gateway/internal/upstream"
)

// streamWithRetry selects an account, refreshes its token if needed, opens the
// upstream stream, and converts the single streamed response object into
// OpenAI chat.completion.chunk SSE frames. When includeUsage is true (client
// sent stream_options.include_usage), the final chunk carries the usage object.
//
// Failures before any bytes are written to the client (connect errors, 401/403
// /429/5xx, or an embedded error as the first event) record the failure,
// queue the account for an async health check, and retry with the next
// account, up to the retry budget (SPEC-hardening 3.4). Once a valid response
// object is received and output begins, the account is not switched.
func (s *Server) streamWithRetry(w http.ResponseWriter, flusher http.Flusher, ctx context.Context, conversationID string, payload []byte, displayModel string, includeUsage bool) {
	// Auto-continuation (ticket 13): fetch the first complete Response and, when
	// it was truncated at the model cap, keep replaying the accumulated text
	// until the model stops or the segment cap. The gateway buffers the whole
	// upstream stream before emitting SSE anyway (the upstream only sends the
	// full object at response.completed), so the merged answer is converted once.
	segments, _, err := s.fetchSegments(ctx, conversationID, payload, s.streamFetcher(conversationID))
	if err != nil {
		if ctx.Err() != nil {
			return // client is gone; nothing left to write
		}
		s.writeStreamError(w, err)
		return
	}
	merged := mergeSegments(segments)
	last := segments[len(segments)-1]
	usage := mergedUsage(segments)
	lines := convert.SSEFromResponse(merged.ID, displayModel, toUpstreamOutput(merged.Output), usage, includeUsage, finishHintFrom(last), sessionFromResp(conversationID, merged))
	for _, line := range lines {
		select {
		case <-ctx.Done():
			return
		default:
		}
		_, _ = w.Write([]byte(line + "\n\n"))
		flusher.Flush()
	}
}

// streamFetcher binds a streaming fetch (fetchStreamOnce) to a conversation so
// the continuation loop reuses the same conversationID stickiness for every
// segment (same account preferred; the retry logic rotates on failure).
func (s *Server) streamFetcher(conversationID string) segmentFetcher {
	return func(ctx context.Context, payload []byte) (*upstream.Response, error) {
		return s.fetchStreamOnce(ctx, conversationID, payload)
	}
}

// fetchStreamOnce performs one streaming upstream round-trip with the account
// retry (ticket 13 splits the fetch from the SSE write so the auto-continuation
// loop can re-fetch). Failures before any response object is received (connect
// errors, 401/403/429/5xx, or an embedded error as the first event) record the
// failure, queue the account for an async health check, and retry with the next
// account up to the retry budget (SPEC-hardening 3.4). The returned error
// preserves the original streamWithRetry classification: an upstream 5xx stays
// 502, balance exhaustion 402, anything else 503.
func (s *Server) fetchStreamOnce(ctx context.Context, conversationID string, payload []byte) (*upstream.Response, error) {
	maxRetries := s.pool.MaxRetries()
	var lastErr error
	allAccountLevel := true
	for attempt := 0; attempt < maxRetries; attempt++ {
		pickID := conversationID
		if attempt > 0 {
			pickID = "" // after the first failure, rotate freely
		}
		s.backoffBeforeRetry(ctx, attempt)
		acc := s.pool.Pick(pickID)
		if acc == nil {
			if lastErr == nil {
				lastErr = &upstream.APIError{Kind: upstream.ErrUnknown, StatusCode: http.StatusServiceUnavailable, Message: "no available accounts"}
			}
			break
		}
		if acc.NeedsRefresh() {
			if err := s.pool.RefreshAccountToken(ctx, acc.Email); err != nil {
				acc.Disable("token refresh failed")
				s.pool.EnqueueHealthCheck(acc)
				lastErr = err
				continue
			}
		}

		ch, closer, err := s.client.Stream(ctx, acc.IdentityTokenValue(), payload)
		if err != nil {
			apiErr, _ := err.(*upstream.APIError)
			kind := upstream.ErrUnknown
			if apiErr != nil {
				kind = apiErr.Kind
			}
			acc.RecordFailure(kind, err.Error())
			s.pool.EnqueueHealthCheck(acc)
			lastErr = err
			if kind == upstream.ErrUpstream {
				allAccountLevel = false
			}
			continue
		}

		// Wait for the first event before returning so we can still switch
		// accounts if the stream opens into an error.
		select {
		case resp, ok := <-ch:
			closer.Close()
			if !ok {
				// Stream closed without data: treat as a failure and retry.
				lastErr = fmt.Errorf("stream closed without data")
				continue
			}
			if resp.Error != nil {
				apiErr := classifyEmbeddedError(resp.Error)
				if apiErr == nil {
					// An empty error object (e.g. a skeleton event's error:{}) is
					// not a real failure; treat the response as valid.
					lastErr = nil
				} else {
					acc.RecordFailure(apiErr.Kind, resp.Error.Message)
					s.pool.EnqueueHealthCheck(acc)
					lastErr = apiErr
					continue
				}
			}
			acc.RecordSuccess(resp.Usage.CreditsUsed)
			return resp, nil
		case <-ctx.Done():
			closer.Close()
			return nil, ctx.Err()
		}
	}

	// Retry budget exhausted. An upstream 5xx surfaces as 502 with trace_id;
	// otherwise the failure is account-level. Among account-level failures a
	// balance-exhaustion error is preserved as 402 so the client can tell "all
	// accounts out of balance" from "all accounts unavailable" (P1-3).
	if apiErr, ok := lastErr.(*upstream.APIError); ok && !allAccountLevel {
		return nil, apiErr
	}
	if apiErr, ok := lastErr.(*upstream.APIError); ok && apiErr.Kind == upstream.ErrInsufficient {
		return nil, &upstream.APIError{Kind: upstream.ErrInsufficient, StatusCode: http.StatusPaymentRequired, Message: "all accounts have insufficient balance"}
	}
	return nil, lastErr
}

// writeStreamError emits the retry-exhaustion error on the streaming path. An
// upstream 5xx surfaces as 502 with trace_id; otherwise the failure is
// account-level, with balance exhaustion preserved as 402 (P1-3). The handler
// already set Content-Type: text/event-stream, so account-level errors are
// written as SSE frames rather than a JSON body (2-5).
func (s *Server) writeStreamError(w http.ResponseWriter, err error) {
	if apiErr, ok := err.(*upstream.APIError); ok && apiErr.Kind == upstream.ErrUpstream {
		writeUpstreamError(w, apiErr)
		return
	}
	if apiErr, ok := err.(*upstream.APIError); ok && apiErr.Kind == upstream.ErrInsufficient {
		writeSSEError(w, http.StatusPaymentRequired, "all accounts have insufficient balance", apiErr)
		return
	}
	writeSSEError(w, http.StatusServiceUnavailable, "no available accounts", err)
}

// writeSSEError emits an error as an SSE data frame with the real HTTP status.
// It is used on the streaming error path: the response header is already
// text/event-stream, so a bare JSON body would be unparseable by the SSE client
// (2-5). No SSE bytes have been flushed yet at this point, so the actual status
// (402 / 503) can still be written — the body stays an SSE frame for parsers.
func writeSSEError(w http.ResponseWriter, status int, msg string, err error) {
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	log.Printf("stream error: %s: %s", msg, detail)
	type body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Detail  string `json:"detail,omitempty"`
		} `json:"error"`
	}
	var b body
	b.Error.Message = msg
	b.Error.Type = "api_error"
	b.Error.Detail = truncate(detail, 300)
	payload, _ := json.Marshal(b)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(status)
	_, _ = w.Write([]byte("data: "))
	_, _ = w.Write(payload)
	_, _ = w.Write([]byte("\n\n"))
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// toUpstreamOutput converts upstream output items into the convert package
// representation used for building OpenAI responses.
func toUpstreamOutput(items []upstream.OutputItem) []convert.UpstreamOutput {
	out := make([]convert.UpstreamOutput, 0, len(items))
	for _, item := range items {
		blocks := make([]convert.ContentBlock, 0, len(item.Content))
		for _, b := range item.Content {
			blocks = append(blocks, convert.ContentBlock{Type: b.Type, Text: b.Text})
		}
		out = append(out, convert.UpstreamOutput{
			Type:      item.Type,
			Content:   blocks,
			Name:      item.Name,
			CallID:    item.CallID,
			Arguments: item.Arguments,
		})
	}
	return out
}

// sessionFromResp builds the continuation-identifier session (ticket 12) for a
// completed upstream response. The upstream never echoes conversation_id (probe
// 2026-08-04), so the gateway echoes the conversation_id it actually used for
// the request and always surfaces the upstream response.id as
// previous_response_id — the handle a client passes back to continue after a
// length truncation. conversationID is the value the gateway forwarded upstream
// (client-supplied or gateway-generated).
func sessionFromResp(conversationID string, resp *upstream.Response) convert.Session {
	s := convert.Session{ConversationID: conversationID}
	if conversationID != "" {
		s.HasConversationID = true
	}
	if resp != nil && resp.ID != "" {
		s.PreviousResponseID = resp.ID
		s.HasPreviousResponse = true
	}
	return s
}

// finishHintFrom builds the convert.FinishHint from a parsed upstream Response
// so the converter can resolve the real OpenAI finish_reason (P0-A1). The
// upstream never sends a usable stop_reason (probe 2026-08-04), so the hint
// carries the model + completion token count for cap-table inference plus the
// raw incomplete_details.reason for future-proofing.
func finishHintFrom(resp *upstream.Response) convert.FinishHint {
	if resp == nil {
		return convert.FinishHint{}
	}
	fs := resp.FinishSignals()
	return convert.FinishHint{
		Model:             fs.Model,
		CompletionTokens:  fs.OutputTokens,
		IncompleteReason: fs.IncompleteReason,
		OutputHasToolCall: fs.OutputHasToolCall,
	}
}

// classifyEmbeddedError maps an embedded error object to an APIError with a
// best-effort ErrorKind so the retry loop can decide whether to switch. A
// syntactically present but empty error object (all fields blank) is not an
// error and yields nil.
func classifyEmbeddedError(e *upstream.RespError) *upstream.APIError {
	if e == nil || (strings.TrimSpace(e.Message+e.Code+e.Type) == "" && e.TraceID == "") {
		return nil
	}
	msg := strings.ToLower(e.Message + " " + e.Code + " " + e.Type)
	kind := upstream.ErrUnknown
	switch {
	case e.Code == "unauthorized" || strings.Contains(msg, "unauthorized"):
		kind = upstream.ErrUnauthorized
	case e.Code == "forbidden" || strings.Contains(msg, "forbidden"):
		kind = upstream.ErrForbidden
	case e.Code == "rate_limited" || strings.Contains(msg, "rate_limit") || strings.Contains(msg, "too many requests"):
		kind = upstream.ErrRateLimited
	case strings.Contains(msg, "payment_required") || strings.Contains(msg, "insufficient balance"):
		kind = upstream.ErrInsufficient
	case strings.Contains(msg, "internal server error"):
		kind = upstream.ErrUpstream
	}
	return &upstream.APIError{
		Kind:    kind,
		Message: e.Message,
		Code:    e.Code,
		Type:    e.Type,
		TraceID: e.TraceID,
	}
}

// withRecovery catches panics in handlers and returns a 500.
func withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic recovered: %v", rec)
				writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
					"error": map[string]interface{}{"message": "internal server error", "type": "api_error"},
				})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// withRequestLog logs one line per request without any tokens or bodies.
func withRequestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		log.Printf("%s %s -> %d (%s) ua=%q",
			r.Method, r.URL.Path, sw.status, time.Since(start).Round(time.Millisecond), r.UserAgent())
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (sw *statusWriter) WriteHeader(code int) {
	sw.status = code
	sw.ResponseWriter.WriteHeader(code)
}

// Flush forwards flushes to the underlying writer so SSE streaming works
// through the request-log middleware.
func (sw *statusWriter) Flush() {
	if f, ok := sw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("conv-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

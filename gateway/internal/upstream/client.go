// Package upstream implements the client for portal.anuma.ai (OpenAI
// Responses API compatible) and the privy.io token refresh endpoint.
//
// Header requirements are captured in SPEC section 3.1: the privy headers and
// x-privacy-mode are mandatory (missing x-privacy-mode => 403).
package upstream

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Default request timeout for a single upstream round-trip.
const (
	DefaultTimeout        = 120 * time.Second
	streamReadTimeout     = 300 * time.Second
	maxFailuresBeforeSkip = 3
)

// Upstream config (SPEC section 3.1). PrivyAppID is configurable via the
// PRIVY_APP_ID env var (it is a public per-app identifier, but keeping it
// out of the source tree lets deployments point at their own Privy app).
var (
	PrivyAppID = envOr("PRIVY_APP_ID", "YOUR_PRIVY_APP_ID")

	PrivyClient = "react-auth:3.14.1"
	userAgent   = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36"
)

// ErrorKind classifies an upstream failure so the account pool can decide how
// to react (SPEC section 5.3).
type ErrorKind int

const (
	ErrUnknown      ErrorKind = iota
	ErrUnauthorized           // 401 -> refresh token then retry once
	ErrForbidden              // 403 forbidden -> switch account
	ErrRateLimited            // 429 -> switch account
	ErrInsufficient           // payment_required / balance exhausted -> disable account
	ErrUpstream               // 5xx -> surface as 502 with trace_id
)

// envOr returns the value of the named env var, or fallback when empty.
func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func (k ErrorKind) String() string {
	switch k {
	case ErrUnauthorized:
		return "unauthorized"
	case ErrForbidden:
		return "forbidden"
	case ErrRateLimited:
		return "rate_limited"
	case ErrInsufficient:
		return "insufficient_balance"
	case ErrUpstream:
		return "upstream_error"
	default:
		return "unknown"
	}
}

// APIError is a structured upstream error. Account is always masked.
type APIError struct {
	Kind       ErrorKind
	StatusCode int
	Message    string
	Code       string // upstream error code, e.g. "forbidden"
	Type       string // upstream error type, e.g. "authorization_error"
	TraceID    string
	RequestID  string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("upstream http=%d kind=%s code=%s trace_id=%s msg=%q",
		e.StatusCode, e.Kind, e.Code, e.TraceID, truncate(e.Message, 200))
}

// IncompleteDetails mirrors the upstream response.incomplete_details object.
// Measured 2026-08-04 (probe, claude-sonnet-5): the field is always present in
// response.completed but its reason is an empty string even when the upstream
// truncates at max_output_tokens, so it is NOT a reliable length signal on its
// own — FinishReason() combines it with the model-cap table.
type IncompleteDetails struct {
	Reason string `json:"reason"`
}

// StopReason is the normalized upstream finish signal. It mirrors the OpenAI
// Responses API stop_reason vocabulary (the gateway maps it onto OpenAI Chat
// finish_reason).
type StopReason string

const (
	StopNone          StopReason = ""                // upstream did not signal (most common)
	StopEndTurn       StopReason = "end_turn"        // model finished naturally
	StopLength        StopReason = "max_output_tokens" // hit the output-token cap
	StopToolCall      StopReason = "tool_call"       // emitted a tool call
	StopContentFilter StopReason = "content_filter"
)

// Response is a subset of the upstream Responses API payload (SPEC 3.3).
type Response struct {
	ID                 string             `json:"id"`
	Object             string             `json:"object"`
	Model              string             `json:"model"`
	CreatedAt          int64              `json:"created_at"`
	Output             []OutputItem       `json:"output"`
	Usage              Usage              `json:"usage"`
	Error              *RespError         `json:"error,omitempty"`
	Status             string             `json:"status"`                   // upstream response.status (measured always "" on completed)
	IncompleteDetails  *IncompleteDetails `json:"incomplete_details,omitempty"` // present but reason="" even on truncation
}

// FinishSignals captures the upstream's real completion reason in a form the
// convert package can map onto an OpenAI finish_reason. The upstream never
// sends a usable stop_reason (probe 2026-08-04): response.status and
// incomplete_details.reason are empty even on max_output_tokens truncation.
// The only reliable length signal is usage output tokens reaching the model's
// hard cap, which the convert package resolves via the model-cap table. The
// raw IncompleteReason is carried along so a future upstream that fills it
// takes effect automatically.
type FinishSignals struct {
	Model             string
	OutputTokens      int    // usage.output_tokens (stream) / completion_tokens (non-stream)
	IncompleteReason  string // raw incomplete_details.reason, usually ""
	OutputHasToolCall bool   // any output item is a function_call
}

// FinishSignals builds the normalized completion signal from a parsed Response.
// The caller hands the resulting FinishSignals to the convert package which
// maps it onto an OpenAI finish_reason (length / stop / tool_calls).
func (r *Response) FinishSignals() FinishSignals {
	if r == nil {
		return FinishSignals{}
	}
	_, completion := r.Usage.Normalized()
	hasTool := false
	for _, item := range r.Output {
		if item.Type == "function_call" {
			hasTool = true
			break
		}
	}
	reason := ""
	if r.IncompleteDetails != nil {
		reason = r.IncompleteDetails.Reason
	}
	return FinishSignals{
		Model:             r.Model,
		OutputTokens:      completion,
		IncompleteReason:  reason,
		OutputHasToolCall: hasTool,
	}
}

// OutputItem is one element of response.output: message or function_call.
type OutputItem struct {
	ID        string         `json:"id"`
	Type      string         `json:"type"` // "message" | "function_call"
	Role      string         `json:"role,omitempty"`
	Content   []ContentBlock `json:"content,omitempty"`
	Name      string         `json:"name,omitempty"`
	CallID    string         `json:"call_id,omitempty"`
	Arguments string         `json:"arguments,omitempty"`
}

// ContentBlock is a text block inside an output message.
type ContentBlock struct {
	Type string `json:"type"` // "output_text" | "text" | "input_text"
	Text string `json:"text"`
}

// TokensDetails mirrors the per-direction token breakdown the upstream embeds
// in usage. input_tokens_details carries cached_tokens (Responses style);
// output_tokens_details carries reasoning_tokens, which is not exposed to
// clients and so not modeled here.
type TokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

// Usage mirrors the upstream usage object. The upstream sends two namings
// depending on the transport (measured 2026-08-03, see docs/DISCREPANCIES §5):
//
//   - non-stream responses embed standard Chat Completions naming
//     (prompt_tokens / completion_tokens) plus total_tokens, cost fields and
//     credits_used — no *_details.
//   - the streamed response.completed event embeds Responses-API naming
//     (input_tokens / output_tokens) plus input_tokens_details.cached_tokens
//     and output_tokens_details.reasoning_tokens; prompt_tokens/completion_tokens
//     are absent, which is why the gateway previously read 0.
//
// Normalized() collapses both namings so callers never depend on which one the
// upstream picked.
type Usage struct {
	PromptTokens        int            `json:"prompt_tokens"`
	CompletionTokens    int            `json:"completion_tokens"`
	InputTokens         int            `json:"input_tokens"`
	OutputTokens        int            `json:"output_tokens"`
	TotalTokens         int            `json:"total_tokens"`
	CreditsUsed         int            `json:"credits_used"`
	PromptTokensDetails *TokensDetails `json:"prompt_tokens_details"`
	InputTokensDetails  *TokensDetails `json:"input_tokens_details"`
}

// Normalized returns the prompt/completion token counts regardless of which
// naming the upstream used: prompt_tokens wins, input_tokens is the fallback
// (the streamed response.completed event only carries input_tokens/output_tokens).
// Both missing / zero yields 0.
func (u *Usage) Normalized() (prompt, completion int) {
	if u == nil {
		return 0, 0
	}
	prompt, completion = u.PromptTokens, u.CompletionTokens
	if prompt == 0 {
		prompt = u.InputTokens
	}
	if completion == 0 {
		completion = u.OutputTokens
	}
	return prompt, completion
}

// CachedTokens returns the cached-token count regardless of which details
// object the upstream sent: prompt_tokens_details wins, input_tokens_details
// is the fallback (Responses style). Absent details yield 0.
func (u *Usage) CachedTokens() int {
	if u == nil {
		return 0
	}
	if u.PromptTokensDetails != nil {
		return u.PromptTokensDetails.CachedTokens
	}
	if u.InputTokensDetails != nil {
		return u.InputTokensDetails.CachedTokens
	}
	return 0
}

// RespError is an error object that may be embedded in a 200 response.
type RespError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
	TraceID string `json:"trace_id"`
}

// Balance mirrors GET /credits/balance.
type Balance struct {
	WalletAddress    string `json:"wallet_address"`
	AvailableCredits int    `json:"available_credits"`
	SubscriptionTier string `json:"subscription_tier"`
}

// Model is one entry in curated-models.
type Model struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Provider  string `json:"provider"`
	Category  string `json:"category"`
	Active    bool   `json:"active"`
	IsPrivate bool   `json:"is_private"`
}

// Client is a reusable HTTP client for the portal API.
type Client struct {
	http      *http.Client
	baseURL   string
	privyURL  string
	modelMu   sync.Mutex
	models    []Model
	modelsAt  time.Time
	modelsTTL time.Duration
}

// NewClient builds an upstream client.
func NewClient(baseURL, privyURL string, ttl time.Duration) *Client {
	return &Client{
		http: &http.Client{
			Timeout: DefaultTimeout,
			Transport: &http.Transport{
				MaxIdleConns:        50,
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		baseURL:   strings.TrimRight(baseURL, "/"),
		privyURL:  strings.TrimRight(privyURL, "/"),
		modelsTTL: ttl,
	}
}

// baseHeaders builds the mandatory auth headers (SPEC 3.1). privy-ca-id is
// freshly generated per request.
func (c *Client) baseHeaders() http.Header {
	h := http.Header{}
	h.Set("privy-app-id", PrivyAppID)
	h.Set("privy-client", PrivyClient)
	h.Set("privy-ca-id", newUUID())
	h.Set("x-privacy-mode", "standard")
	h.Set("origin", "https://chat.anuma.ai")
	h.Set("referer", "https://chat.anuma.ai/")
	h.Set("user-agent", userAgent)
	h.Set("accept", "application/json")
	h.Set("content-type", "application/json")
	return h
}

// Chat sends a non-streaming responses request and returns the parsed object.
func (c *Client) Chat(ctx context.Context, identityToken string, payload []byte) (*Response, error) {
	resp, err := c.do(ctx, "POST", c.baseURL+"/responses", identityToken, payload)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out Response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, &APIError{Kind: ErrUpstream, StatusCode: resp.StatusCode, Message: "decode upstream response: " + err.Error()}
	}
	if out.Error != nil {
		return nil, classifyError(resp.StatusCode, &APIError{Message: out.Error.Message, Code: out.Error.Code, Type: out.Error.Type, TraceID: out.Error.TraceID}, "")
	}
	return &out, nil
}

// Stream sends a streaming responses request and returns an iterator of parsed
// response objects. The upstream emits the OpenAI Responses API event sequence
// (response.created / in_progress / output_text.delta / ... / response.completed
// + [DONE], measured 2026-08-03): only the final response.completed event
// carries the full response object (output, usage, model). The intermediate
// events are absorbed so the iterator yields exactly one complete Response.
// A response.failed event is surfaced as a Response carrying the error. The
// returned closer must be called to release the connection.
func (c *Client) Stream(ctx context.Context, identityToken string, payload []byte) (<-chan *Response, io.Closer, error) {
	reqCtx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(reqCtx, "POST", c.baseURL+"/responses", bytes.NewReader(payload))
	if err != nil {
		cancel()
		return nil, nil, err
	}
	h := c.baseHeaders()
	h.Set("Authorization", "Bearer "+identityToken)
	req.Header = h

	httpClient := *c.http
	httpClient.Timeout = streamReadTimeout
	resp, err := httpClient.Do(req)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		cancel()
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return nil, nil, classifyError(resp.StatusCode, parseErrorBody(resp.StatusCode, body), "")
	}

	ch := make(chan *Response, 1)
	closer := &streamCloser{closer: resp.Body, cancel: cancel}
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" || data == "[DONE]" {
				continue
			}
			var evt struct {
				Type     string     `json:"type"`
				Response *Response  `json:"response"`
				Error    *RespError `json:"error"`
			}
			if err := json.Unmarshal([]byte(data), &evt); err != nil {
				continue
			}
			// The upstream sends one OpenAI Responses API event per line; only
			// response.completed (and response.failed) carry a usable payload.
			// The intermediate events (created / in_progress / output_text.delta
			// / ping / ...) are dropped — including their empty `error:{}` —
			// so a skeleton event is never mistaken for an error.
			switch evt.Type {
			case "response.completed":
				if evt.Response != nil {
					ch <- evt.Response
					return
				}
			case "response.failed":
				err := evt.Error
				if err == nil && evt.Response != nil {
					err = evt.Response.Error
				}
				if err == nil {
					err = &RespError{Message: "response failed", Type: "stream_error"}
				}
				ch <- &Response{Error: err}
				return
			}
			// response.created / in_progress / output_text.delta / ... : absorbed.
			if evt.Type == "" && evt.Error != nil {
				ch <- &Response{Error: evt.Error}
				return
			}
			if evt.Type == "" && evt.Response != nil {
				ch <- evt.Response
				return
			}
		}
	}()
	return ch, closer, nil
}

type streamCloser struct {
	closer io.Closer
	cancel context.CancelFunc
}

func (s *streamCloser) Close() error {
	s.cancel()
	return s.closer.Close()
}

// do performs a single authenticated JSON request.
func (c *Client) do(ctx context.Context, method, url, identityToken string, payload []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	h := c.baseHeaders()
	if identityToken != "" {
		h.Set("Authorization", "Bearer "+identityToken)
	}
	req.Header = h
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return nil, classifyError(resp.StatusCode, parseErrorBody(resp.StatusCode, body), "")
	}
	return resp, nil
}

// classifyError converts an upstream HTTP status into an APIError with the
// right Kind for the account pool retry logic.
func classifyError(status int, parsed *APIError, raw string) *APIError {
	if parsed == nil {
		parsed = &APIError{StatusCode: status, Message: truncate(raw, 300)}
	}
	if parsed.StatusCode == 0 {
		parsed.StatusCode = status
	}
	msg := strings.ToLower(parsed.Message + " " + parsed.Code + " " + parsed.Type)
	switch {
	case status == http.StatusUnauthorized:
		parsed.Kind = ErrUnauthorized
	case status == http.StatusForbidden:
		parsed.Kind = ErrForbidden
	case status == http.StatusTooManyRequests:
		parsed.Kind = ErrRateLimited
	case status == http.StatusPaymentRequired ||
		strings.Contains(msg, "payment_required") ||
		strings.Contains(msg, "insufficient balance"):
		parsed.Kind = ErrInsufficient
	case status >= 500:
		parsed.Kind = ErrUpstream
	default:
		parsed.Kind = ErrUnknown
	}
	return parsed
}

func parseErrorBody(status int, body []byte) *APIError {
	e := &APIError{StatusCode: status}
	var probe struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
		Message   string `json:"message"`
		Type      string `json:"type"`
		Code      string `json:"code"`
		TraceID   string `json:"trace_id"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(body, &probe); err == nil {
		if probe.Error.Message != "" {
			e.Message, e.Type, e.Code = probe.Error.Message, probe.Error.Type, probe.Error.Code
		} else {
			e.Message, e.Type, e.Code = probe.Message, probe.Type, probe.Code
		}
		e.TraceID, e.RequestID = probe.TraceID, probe.RequestID
	}
	if e.Message == "" {
		e.Message = truncate(string(body), 300)
	}
	return e
}

// newUUID returns a random RFC 4122 v4 UUID string using crypto/rand.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Extremely unlikely; fall back to a timestamp-derived id.
		return fmt.Sprintf("ca-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

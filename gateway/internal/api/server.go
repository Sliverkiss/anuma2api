// Package api exposes the HTTP routes: OpenAI-compatible endpoints and the
// management API (SPEC sections 5 & 6). The admin panel frontend was removed
// (SPEC-hardening 3.1): GET / returns 404 and only /healthz, /api/*, /v1/*
// remain.
package api

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"anuma2api/gateway/internal/convert"
	"anuma2api/gateway/internal/pool"
	"anuma2api/gateway/internal/upstream"
)

// upstreamClient is the subset of *upstream.Client the gateway handlers depend
// on, kept as an interface so the handlers are testable with a stub.
type upstreamClient interface {
	Models(ctx context.Context) ([]upstream.Model, error)
	Chat(ctx context.Context, identityToken string, payload []byte) (*upstream.Response, error)
	Stream(ctx context.Context, identityToken string, payload []byte) (<-chan *upstream.Response, io.Closer, error)
	GetBalance(ctx context.Context, identityToken string) (*upstream.Balance, error)
}

// Server holds the HTTP handlers' dependencies.
type Server struct {
	client upstreamClient
	pool   *pool.Pool
	// admin is the Bearer token required for /api/* and /v1/* (SPEC-auth §2).
	admin string
	// allowedModels is the allowlist for /v1/models and chat/completions
	// (SPEC-hardening 3.5). Empty means nothing is served.
	allowedModels map[string]bool
	// autoContinue enables gateway-side auto-continuation (ticket 13): a
	// length-truncated response is replayed with the accumulated text until the
	// model stops or the segment cap. Set via WithAutoContinue.
	autoContinue bool
	// autoContinueMaxSegments is the continuation segment cap including the
	// first. <=0 disables auto-continuation.
	autoContinueMaxSegments int
}

// Option configures optional Server behavior. Options are applied in order.
type Option func(*Server)

// WithAutoContinue enables gateway-side auto-continuation (ticket 13).
// maxSegments is the segment cap including the first (ANUMA_AUTO_CONTINUE_MAX_SEGMENTS,
// default 5); <=0 disables auto-continuation even when enabled is true.
func WithAutoContinue(enabled bool, maxSegments int) Option {
	return func(s *Server) {
		s.autoContinue = enabled && maxSegments > 0
		s.autoContinueMaxSegments = maxSegments
	}
}

// New creates the API server. adminPassword is the Bearer token protecting the
// OpenAI-compatible and management APIs. allowlist restricts which models the
// gateway serves (SPEC-hardening 3.5). *upstream.Client satisfies upstreamClient.
func New(client upstreamClient, pool *pool.Pool, adminPassword string, allowlist []string, opts ...Option) *Server {
	allowed := make(map[string]bool, len(allowlist))
	for _, m := range allowlist {
		allowed[m] = true
	}
	s := &Server{client: client, pool: pool, admin: adminPassword, allowedModels: allowed}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Handler returns the root http.Handler with all routes mounted.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	protected := http.NewServeMux()

	// No frontend is served; an unmatched GET / falls through to the mux's
	// 404 handler (SPEC-hardening 3.1).

	// Health probe: public (SPEC-auth §2).
	mux.HandleFunc("GET /healthz", s.handleHealth)

	// OpenAI-compatible (protected)
	protected.HandleFunc("GET /v1/models", s.handleModels)
	protected.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)

	// Management API (protected). POST /api/accounts uploads accounts (upsert,
	// SPEC-upload §2.1); the old CSV reload endpoint is gone (SPEC-upload §2.3).
	protected.HandleFunc("GET /api/stats", s.handleStats)
	protected.HandleFunc("GET /api/accounts", s.handleListAccounts)
	protected.HandleFunc("POST /api/accounts", s.handleUploadAccounts)
	protected.HandleFunc("DELETE /api/accounts/{email}", s.handleDeleteAccount)
	protected.HandleFunc("GET /api/accounts/{email}/balance", s.handleAccountBalance)
	protected.HandleFunc("POST /api/accounts/{email}/refresh-token", s.handleAccountRefresh)
	protected.HandleFunc("GET /api/metrics", s.handleMetrics)
	protected.HandleFunc("POST /api/accounts/refresh-all", s.handleRefreshAll)
	protected.HandleFunc("POST /api/accounts/cooldowns-check", s.handleCooldownCheck)

	// Register method-qualified subtree patterns ("/api/" vs "GET /" would
	// otherwise conflict in Go 1.22+ ServeMux).
	mux.Handle("GET /api/", s.auth(protected))
	mux.Handle("POST /api/", s.auth(protected))
	mux.Handle("DELETE /api/", s.auth(protected))
	mux.Handle("GET /v1/", s.auth(protected))
	mux.Handle("POST /v1/", s.auth(protected))

	return withRecovery(withRequestLog(mux))
}

// auth enforces Bearer-token authentication on the wrapped handler.
// The token is compared in constant time (SPEC-auth §2).
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := ""
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			token = strings.TrimPrefix(h, "Bearer ")
		}
		if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(s.admin)) != 1 {
			writeAuthError(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// writeAuthError writes the 401 response mandated by SPEC-auth §2.
func writeAuthError(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="anuma2api"`)
	writeJSON(w, http.StatusUnauthorized, map[string]interface{}{
		"error": map[string]interface{}{
			"message": "unauthorized",
			"type":    "api_error",
		},
	})
}

// isModelAllowed reports whether a resolved upstream model id is served.
func (s *Server) isModelAllowed(modelID string) bool {
	return s.allowedModels[modelID]
}

// ---- OpenAI-compatible endpoints ----

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	models, err := s.client.Models(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to fetch models", err)
		return
	}
	data := make([]map[string]interface{}, 0, len(models))
	for _, m := range models {
		// Only whitelisted models are exposed to basic-tier clients
		// (SPEC-hardening 3.5). The allowlist stores upstream long names; the
		// exposed id is the short name (provider prefix stripped, SPEC
		// model-alias §3.2). upstream_id is kept for debugging.
		if !s.isModelAllowed(m.ID) {
			continue
		}
		data = append(data, map[string]interface{}{
			"id":          convert.DisplayName(m.ID),
			"object":      "model",
			"owned_by":    "anuma",
			"created":     time.Now().Unix(),
			"provider":    m.Provider,
			"category":    m.Category,
			"upstream_id": m.ID,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"object": "list", "data": data})
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	var req convert.Request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", err)
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "messages is required", nil)
		return
	}
	// Resolve the inbound model (short or long name) to the upstream long name,
	// then allowlist-check the resolved id. Unknown models resolve to themselves
	// and are rejected here (SPEC model-alias §3.3).
	resolved := convert.ResolveUpstream(req.Model)
	if !s.isModelAllowed(resolved) {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error": map[string]interface{}{
				"message": fmt.Sprintf("model %q is not available", req.Model),
				"type":    "model_not_allowed",
			},
		})
		return
	}

	// Conversation continuation (ticket 12): when the client supplies a
	// conversation_id it is used verbatim (never rewritten) so a "continue"
	// request after finish_reason=length hits the same upstream conversation and
	// the same pool account (Pick is sticky on the same key). Otherwise a fresh
	// UUID keeps the old stateless behavior.
	conversationID := req.ConversationID
	if conversationID == "" {
		conversationID = newUUID()
	}
	payload, err := req.ToUpstreamPayload(conversationID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid conversion", err)
		return
	}

	// The response echoes the client-visible short name (glm-5.2) rather than the
	// upstream long/canonical id (glm/glm-5.2 or accounts/fireworks/models/glm-5p2),
	// so clients matching on the requested model see their own id back
	// (SPEC-usage-fix P1-1).
	displayModel := convert.DisplayName(convert.ResolveUpstream(req.Model))

	if req.Stream {
		s.handleStream(w, r, conversationID, payload, displayModel, req.WantUsage())
		return
	}
	s.handleNonStream(w, r, conversationID, payload, displayModel)
}

func (s *Server) handleNonStream(w http.ResponseWriter, r *http.Request, conversationID string, payload []byte, displayModel string) {
	// Auto-continuation (ticket 13): fetchSegments fetches the first complete
	// Response and, when auto-continuation is enabled and it was truncated at
	// the model cap, keeps replaying the accumulated text until the model stops
	// or the segment cap — the loop sits right after the first Response, before
	// anything is handed to the client. Segments are merged into one answer
	// below.
	segments, _, err := s.fetchSegments(r.Context(), conversationID, payload, s.nonStreamFetcher(conversationID))
	if err != nil {
		apiErr, ok := err.(*upstream.APIError)
		if ok {
			switch {
			case apiErr.StatusCode == http.StatusServiceUnavailable:
				// All accounts unavailable (SPEC 5.3).
				writeError(w, http.StatusServiceUnavailable, "all accounts unavailable", err)
				return
			case apiErr.Kind == upstream.ErrInsufficient:
				writeError(w, http.StatusPaymentRequired, "account balance exhausted", err)
				return
			case apiErr.Kind == upstream.ErrUpstream:
				// 502 with trace_id per SPEC 5.3.
				writeUpstreamError(w, apiErr)
				return
			}
		}
		writeError(w, http.StatusBadGateway, "upstream request failed", err)
		return
	}
	merged := mergeSegments(segments)
	last := segments[len(segments)-1]
	out := toUpstreamOutput(merged.Output)
	usage := mergedUsage(segments)
	cc := convert.UpstreamResponseToChat(merged.ID, displayModel, out, usage, finishHintFrom(last), sessionFromResp(conversationID, merged))
	writeJSON(w, http.StatusOK, cc)
}

// usageFromUpstream maps an upstream Usage into the OpenAI-format convert.Usage:
// token counts go through Normalized() (prompt_tokens wins, input_tokens falls
// back for the streamed response.completed naming), and the cached-token count
// is surfaced under prompt_tokens_details. When the upstream provides no cache
// stats the details object is still emitted as {"cached_tokens":0} so clients
// (and CPA's usage parser) always see the OpenAI-standard structure
// (SPEC-usage-fix P0-2 §2.2).
func usageFromUpstream(u upstream.Usage) convert.Usage {
	prompt, completion := u.Normalized()
	out := convert.Usage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      u.TotalTokens,
	}
	cached := u.CachedTokens()
	out.PromptTokensDetails = &convert.PromptTokensDetails{CachedTokens: cached}
	return out
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request, conversationID string, payload []byte, displayModel string, includeUsage bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported", nil)
		return
	}
	s.streamWithRetry(w, flusher, r.Context(), conversationID, payload, displayModel, includeUsage)
}

// doWithRetry attempts the request across accounts with retry (SPEC-hardening
// 3.4): upstream 5xx switch account immediately (an account-independent model
// defect); network errors retry the same account once then switch; 403/429
// switch account; 401 refreshes the token then retries once. Every failed
// account is queued for an async health check.
//
// Returned errors:
//   - upstream.APIError with Kind ErrUpstream  -> 502 with trace_id
//   - upstream.APIError with Kind ErrInsufficient -> 402 (account disabled)
//   - any other exhaustion (forbidden / rate limited / no accounts) -> 503
func (s *Server) doWithRetry(ctx context.Context, conversationID string, payload []byte) (*upstream.Response, string, error) {
	maxRetries := s.pool.MaxRetries()
	var lastErr error
	allAccountLevel := true // true if every failure was an account-level issue (not an upstream 5xx)
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
			return nil, "", lastErr
		}
		if acc.NeedsRefresh() {
			if err := s.pool.RefreshAccountToken(ctx, acc.Email); err != nil {
				acc.Disable("token refresh failed")
				s.pool.EnqueueHealthCheck(acc)
				lastErr = err
				continue
			}
		}
		resp, err := s.client.Chat(ctx, acc.IdentityTokenValue(), payload)
		if err == nil {
			acc.RecordSuccess(resp.Usage.CreditsUsed)
			return resp, resp.Model, nil
		}
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

		switch kind {
		case upstream.ErrUnauthorized:
			if rerr := s.pool.RefreshAccountToken(ctx, acc.Email); rerr != nil {
				acc.Disable("token refresh failed")
				continue
			}
			resp2, err2 := s.client.Chat(ctx, acc.IdentityTokenValue(), payload)
			if err2 == nil {
				acc.RecordSuccess(resp2.Usage.CreditsUsed)
				return resp2, resp2.Model, nil
			}
			acc.RecordFailure(apiKindOf(err2), err2.Error())
			s.pool.EnqueueHealthCheck(acc)
			lastErr = err2
			continue
		case upstream.ErrForbidden, upstream.ErrRateLimited:
			continue
		case upstream.ErrInsufficient:
			// Balance exhausted: park the account in cooldown (not disable) so the
			// monthly patrol restores it when the real balance is positive again
			// (P1-2, SPEC-cooldown §2.2).
			acc.EnterCooldown("insufficient balance")
			continue
		case upstream.ErrUpstream:
			// Upstream 5xx is a model-level defect, not an account-level one:
			// retrying the same account hits the same 5xx and wastes the retry
			// budget, so switch accounts immediately (P0-1). The failure was
			// already recorded and the account queued for an async health check
			// before the switch.
			continue
		case upstream.ErrUnknown:
			// Network error: retry the same account once (transient jitter), then
			// switch on a second failure.
			resp2, err2 := s.client.Chat(ctx, acc.IdentityTokenValue(), payload)
			if err2 == nil {
				acc.RecordSuccess(resp2.Usage.CreditsUsed)
				return resp2, resp2.Model, nil
			}
			acc.RecordFailure(apiKindOf(err2), err2.Error())
			s.pool.EnqueueHealthCheck(acc)
			lastErr = err2
			continue
		}
	}
	if allAccountLevel {
		// Every failure was account-level (not an upstream 5xx). Preserve a
		// balance-exhaustion signal so the client can tell "all accounts are
		// out of balance" (402) from "all accounts unavailable" (503) — P1-3.
		if apiErr, ok := lastErr.(*upstream.APIError); ok && apiErr.Kind == upstream.ErrInsufficient {
			return nil, "", &upstream.APIError{Kind: upstream.ErrInsufficient, StatusCode: http.StatusPaymentRequired, Message: "all accounts have insufficient balance"}
		}
		return nil, "", &upstream.APIError{Kind: upstream.ErrUnknown, StatusCode: http.StatusServiceUnavailable, Message: "all accounts unavailable"}
	}
	return nil, "", lastErr
}

// apiKindOf extracts the ErrorKind from an error, defaulting to ErrUnknown.
func apiKindOf(err error) upstream.ErrorKind {
	if apiErr, ok := err.(*upstream.APIError); ok {
		return apiErr.Kind
	}
	return upstream.ErrUnknown
}

// maxRetryBackoff caps the per-failure conversation backoff (SPEC-cooldown §2.4).
const maxRetryBackoff = 2 * time.Second

// backoffBeforeRetry sleeps before a retry attempt, ramping with the attempt
// index (200ms * attempt, capped at 2s) so repeated failures slow down instead
// of hammering the upstream (SPEC-cooldown §2.4). The first attempt (index 0)
// is never delayed.
func (s *Server) backoffBeforeRetry(ctx context.Context, attempt int) {
	if attempt <= 0 {
		return
	}
	d := s.pool.RetryBackoff() * time.Duration(attempt)
	if d > maxRetryBackoff {
		d = maxRetryBackoff
	}
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}

// ---- auto-continuation (ticket 13) ----

// continueInstruction is the generic prompt appended as the user message for
// each auto-continuation segment. It tells the model to resume from the break
// without repeating or restarting.
const continueInstruction = "请从上次中断的地方继续，完成剩余内容并给出自然的总结收尾，然后结束。不要重复已写内容，不要重新开头，不要无限扩展篇幅。"

// segmentFetcher performs one upstream round-trip (with the account-retry
// logic) and returns the single complete Response object.
type segmentFetcher func(ctx context.Context, payload []byte) (*upstream.Response, error)

// nonStreamFetcher binds a non-streaming fetch (doWithRetry) to a conversation
// so the continuation loop reuses the same conversationID stickiness for every
// segment (same account preferred; the retry logic rotates on failure).
func (s *Server) nonStreamFetcher(conversationID string) segmentFetcher {
	return func(ctx context.Context, payload []byte) (*upstream.Response, error) {
		resp, _, err := s.doWithRetry(ctx, conversationID, payload)
		return resp, err
	}
}

// fetchSegments drives the auto-continuation loop. It fetches the first
// segment; when auto-continuation is disabled or the response ended naturally
// (stop / tool_calls / a model without a measured cap), it returns that single
// segment unchanged. Otherwise it replays the accumulated text with the
// continue instruction until the model stops or the segment cap (inclusive of
// the first) is reached. A failed continuation segment is logged and stops the
// loop, returning whatever was accumulated so far — the partially-written
// answer is never discarded, and the caller surfaces the last segment's
// finish_reason (length). The second return value is the first segment's
// resolved upstream model.
func (s *Server) fetchSegments(ctx context.Context, conversationID string, payload []byte, fetch segmentFetcher) ([]*upstream.Response, string, error) {
	first, err := fetch(ctx, payload)
	if err != nil {
		return nil, "", err
	}
	if !s.autoContinue || s.autoContinueMaxSegments <= 1 {
		return []*upstream.Response{first}, first.Model, nil
	}
	segments := []*upstream.Response{first}
	accumulated := collectOutputText(first.Output)
	for seg := 2; seg <= s.autoContinueMaxSegments; seg++ {
		last := segments[len(segments)-1]
		// Continue only on a genuine length truncation. resolveFinishReason maps
		// tool_calls (priority) and stop to their own reasons, so neither a tool
		// call nor a natural stop triggers a replay (ticket 13 §3.1).
		if convert.ResolveFinishReason(finishHintFrom(last)) != "length" {
			break
		}
		if strings.TrimSpace(accumulated) == "" {
			break
		}
		nextPayload, err := appendContinuation(payload, accumulated)
		if err != nil {
			log.Printf("auto-continue: build segment %d payload: %v", seg, err)
			break
		}
		next, err := fetch(ctx, nextPayload)
		if err != nil {
			log.Printf("auto-continue: segment %d failed, returning %d accumulated segments: %v", seg, len(segments), err)
			break
		}
		segments = append(segments, next)
		accumulated += collectOutputText(next.Output)
	}
	return segments, first.Model, nil
}

// collectOutputText joins the text content of an upstream response's output
// items (the same join convert.CollectStreamText performs, adapted to the
// upstream response representation).
func collectOutputText(items []upstream.OutputItem) string {
	return convert.CollectStreamText(toUpstreamOutput(items))
}

// appendContinuation returns a copy of the upstream payload with the
// accumulated assistant text and the continue instruction appended to the
// "input" array (ticket 13). All other request parameters (model,
// max_output_tokens, temperature, tools, conversation_id) are kept from the
// original request. previous_response_id is dropped: the gateway continues by
// replaying the message history, and the upstream does not support the
// cross-request id hand-off (probe 2026-08-04).
func appendContinuation(payload []byte, accumulated string) ([]byte, error) {
	var body map[string]interface{}
	if err := json.Unmarshal(payload, &body); err != nil {
		return nil, err
	}
	rawInput, ok := body["input"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("payload has no input array")
	}
	input := make([]interface{}, 0, len(rawInput)+2)
	input = append(input, rawInput...)
	input = append(input,
		map[string]interface{}{
			"role":    "assistant",
			"content": []map[string]interface{}{{"type": "text", "text": accumulated}},
		},
		map[string]interface{}{
			"role":    "user",
			"content": []map[string]interface{}{{"type": "text", "text": continueInstruction}},
		},
	)
	body["input"] = input
	delete(body, "previous_response_id")
	return json.Marshal(body)
}

// mergeSegments concatenates the output items of all auto-continuation
// segments into one Response. The merged response carries the last segment's id
// (the handle a client would continue from), the first segment's model, and
// the last segment's incomplete_details so finish inference stays on the final
// segment. A single segment is returned unchanged.
func mergeSegments(segments []*upstream.Response) *upstream.Response {
	if len(segments) == 1 {
		return segments[0]
	}
	last := segments[len(segments)-1]
	merged := &upstream.Response{
		ID:                last.ID,
		Model:             segments[0].Model,
		IncompleteDetails: last.IncompleteDetails,
	}
	for _, seg := range segments {
		merged.Output = append(merged.Output, seg.Output...)
	}
	return merged
}

// mergedUsage builds the client-facing usage for an auto-continued response
// (ticket 13): prompt_tokens from the first segment, completion_tokens summed
// across all segments, total accordingly, cached-token details from the first
// segment (prompt side). A single segment returns the same usage as
// usageFromUpstream so the non-continuation path is unchanged.
func mergedUsage(segments []*upstream.Response) convert.Usage {
	if len(segments) == 1 {
		return usageFromUpstream(segments[0].Usage)
	}
	prompt, _ := segments[0].Usage.Normalized()
	completion := 0
	for _, seg := range segments {
		_, c := seg.Usage.Normalized()
		completion += c
	}
	out := convert.Usage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      prompt + completion,
	}
	out.PromptTokensDetails = &convert.PromptTokensDetails{CachedTokens: segments[0].Usage.CachedTokens()}
	return out
}

// ---- management API ----

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "ok"})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.pool.Stats())
}

func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	// There is no removed status anymore (SPEC-upload §2.2). An optional
	// ?status=cooldown filter lists only cooldown accounts (SPEC-cooldown §2.5);
	// without it every account in the pool is returned.
	filter := r.URL.Query().Get("status")
	writeJSON(w, http.StatusOK, map[string]interface{}{"accounts": s.pool.ListByStatus(filter)})
}

// uploadPayload is the POST /api/accounts body: a single account object or an
// array of them. At least one entry with a non-empty email is required.
type uploadPayload struct {
	Items []pool.AccountInput
}

func (p *uploadPayload) UnmarshalJSON(data []byte) error {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if data[0] == '[' {
		var items []pool.AccountInput
		if err := json.Unmarshal(data, &items); err != nil {
			return err
		}
		p.Items = items
		return nil
	}
	var one pool.AccountInput
	if err := json.Unmarshal(data, &one); err != nil {
		return err
	}
	p.Items = []pool.AccountInput{one}
	return nil
}

// handleUploadAccounts upserts one or more accounts and reports added/updated
// counts (SPEC-upload §2.1). The request body is validated before any write so
// a malformed payload is rejected wholesale.
func (s *Server) handleUploadAccounts(w http.ResponseWriter, r *http.Request) {
	// Reject bodies over 4MB explicitly (413) instead of silently truncating
	// with io.LimitReader (2-11).
	if r.ContentLength > 4<<20 {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large", nil)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body failed", err)
		return
	}
	if len(body) > 4<<20 {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large", nil)
		return
	}
	var payload uploadPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", err)
		return
	}
	if len(payload.Items) == 0 {
		writeError(w, http.StatusBadRequest, "accounts list is empty", nil)
		return
	}
	for _, in := range payload.Items {
		if in.Email == "" || in.IdentityToken == "" {
			writeError(w, http.StatusBadRequest, "email and identity_token are required", nil)
			return
		}
	}
	added, updated, err := s.pool.UpsertAccounts(payload.Items)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "upsert failed", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"code": 0,
		"data": map[string]interface{}{"added": added, "updated": updated},
	})
}

// handleDeleteAccount physically deletes the account (SPEC-upload §2.2): the
// DELETE endpoint no longer marks it disabled.
func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	email := r.PathValue("email")
	if email == "" {
		writeError(w, http.StatusBadRequest, "email required", nil)
		return
	}
	masked, err := s.pool.Delete(email)
	if err != nil {
		writeError(w, http.StatusNotFound, "account not found", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"code": 0, "data": masked})
}

func (s *Server) handleAccountBalance(w http.ResponseWriter, r *http.Request) {
	email := r.PathValue("email")
	acc := s.pool.Get(email)
	if acc == nil {
		writeError(w, http.StatusNotFound, "account not found", nil)
		return
	}
	bal, err := s.client.GetBalance(r.Context(), acc.IdentityTokenValue())
	if err != nil {
		writeError(w, http.StatusBadGateway, "balance fetch failed", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"code": 0, "data": bal})
}

func (s *Server) handleAccountRefresh(w http.ResponseWriter, r *http.Request) {
	email := r.PathValue("email")
	masked, err := s.pool.RefreshToken(r.Context(), email)
	if err != nil {
		writeError(w, http.StatusBadGateway, "refresh failed", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"code": 0, "data": masked})
}

// handleRefreshAll manually triggers one refresh round over every active
// account (SPEC refresh-loop §3.3). It shares the implementation with the
// background loop and reports ok/fail counts; if a round is already running it
// returns busy=true instead of starting a second one.
func (s *Server) handleRefreshAll(w http.ResponseWriter, r *http.Request) {
	stats := s.pool.RefreshAll(r.Context())
	writeJSON(w, http.StatusOK, map[string]interface{}{"code": 0, "data": stats})
}

// handleCooldownCheck manually triggers one cooldown round over every cooldown
// account (SPEC-cooldown §2.3). It shares the implementation with the
// background patrol and reports restored/still/total counts; if a round is
// already running it returns busy=true.
func (s *Server) handleCooldownCheck(w http.ResponseWriter, r *http.Request) {
	stats := s.pool.CheckCooldowns(r.Context())
	writeJSON(w, http.StatusOK, map[string]interface{}{"code": 0, "data": stats})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	stats := s.pool.Stats()
	// Removed counter is gone: accounts are physically deleted (SPEC-upload §2.2).
	body := fmt.Sprintf("anuma_total_accounts %d\nanuma_available_accounts %d\nanuma_disabled_accounts %d\nanuma_cooldown_accounts %d\nanuma_total_credits %d\nanuma_today_calls %d\n",
		stats.TotalAccounts, stats.Available, stats.Disabled, stats.Cooldown, stats.TotalCredits, stats.TodayCalls)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string, err error) {
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	log.Printf("api error: %s: %s", msg, detail)
	writeJSON(w, status, map[string]interface{}{
		"error": map[string]interface{}{
			"message": msg,
			"type":    "api_error",
			"detail":  truncate(detail, 300),
		},
	})
}

func writeUpstreamError(w http.ResponseWriter, apiErr *upstream.APIError) {
	log.Printf("upstream error: %s", apiErr.Error())
	writeJSON(w, http.StatusBadGateway, map[string]interface{}{
		"error": map[string]interface{}{
			"message":    apiErr.Message,
			"type":       apiErr.Type,
			"code":       apiErr.Code,
			"trace_id":   apiErr.TraceID,
			"request_id": apiErr.RequestID,
		},
	})
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

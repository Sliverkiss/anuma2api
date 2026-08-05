package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"anuma2api/gateway/internal/convert"
	"anuma2api/gateway/internal/pool"
	"anuma2api/gateway/internal/upstream"
)

// stubUpstream is a fixed upstreamClient for handler tests: it serves a canned
// model list and a canned chat response, and records the last chat payload so
// tests can assert the resolved model was forwarded.
type stubUpstream struct {
	models    []upstream.Model
	resp      *upstream.Response
	lastBody  map[string]interface{}
	chatCalls int

	// streamResp is the first (and only) event delivered on Stream. When nil,
	// Stream returns an already-closed channel (stream closed without data).
	streamResp *upstream.Response
}

func (s *stubUpstream) Models(ctx context.Context) ([]upstream.Model, error) {
	return s.models, nil
}

func (s *stubUpstream) Chat(ctx context.Context, identityToken string, payload []byte) (*upstream.Response, error) {
	s.chatCalls++
	_ = json.Unmarshal(payload, &s.lastBody)
	return s.resp, nil
}

func (s *stubUpstream) Stream(ctx context.Context, identityToken string, payload []byte) (<-chan *upstream.Response, io.Closer, error) {
	_ = json.Unmarshal(payload, &s.lastBody)
	ch := make(chan *upstream.Response, 1)
	if s.streamResp != nil {
		ch <- s.streamResp
	}
	close(ch)
	return ch, io.NopCloser(strings.NewReader("")), nil
}

func (s *stubUpstream) GetBalance(ctx context.Context, identityToken string) (*upstream.Balance, error) {
	return &upstream.Balance{AvailableCredits: 100}, nil
}

// minimalServer returns a Server backed by a stub upstream and a one-account
// pool (a@b.com, 100 credits), with the given allowlist.
func minimalServer(t *testing.T, allowlist []string, stub *stubUpstream) *Server {
	t.Helper()
	p := minimalPool(t)
	if stub == nil {
		stub = &stubUpstream{}
	}
	return New(stub, p, "pw", allowlist)
}

// twelveModels is the allowlist of upstream long names (SPEC model-alias §2).
var twelveModels = []string{
	"inclusionai/ling-2.6-flash",
	"kimi/kimi-k3",
	"openai/gpt-5.6-luna",
	"qwen/qwen-3.7-plus",
	"qwen/qwen-3.6-plus",
	"glm/glm-5.2",
	"minimax/minimax-m3",
	"minimax/minimax-m2.5",
	"anthropic/claude-sonnet-5",
	"gemini/gemini-3.1-pro-preview",
	"gemini/gemini-3-flash-preview",
	"grok/grok-4.5",
}

// stubModels is a curated-models response covering the whitelist plus a few
// non-whitelisted upstream models that /v1/models must hide.
func stubModels() []upstream.Model {
	out := make([]upstream.Model, 0, len(twelveModels)+2)
	for _, id := range twelveModels {
		out = append(out, upstream.Model{ID: id, Provider: strings.SplitN(id, "/", 2)[0], Category: "general", Active: true})
	}
	out = append(out,
		upstream.Model{ID: "openai/gpt-5.6-sol", Provider: "openai", Active: true},          // tier-gated, hidden
		upstream.Model{ID: "anthropic/claude-fable-5", Provider: "anthropic", Active: true}, // tier-gated, hidden
	)
	return out
}

// input builds an upload payload.
func input(email string, credits int) pool.AccountInput {
	return pool.AccountInput{
		Email:            email,
		WalletAddress:    "0x1",
		UserID:           "u1",
		Tier:             "basic",
		IdentityToken:    "id.token." + email,
		AccessToken:      "acc.token." + email,
		RefreshToken:     "ref.token." + email,
		AvailableCredits: credits,
		ExpiresAt:        1785723984,
	}
}

// minimalPool returns a pool with one account (uploaded) backed by a temp DB.
func minimalPool(t *testing.T) *pool.Pool {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	p, err := pool.New(nil, dbPath, pool.Options{})
	if err != nil {
		t.Fatalf("pool.New: %v", err)
	}
	if _, _, err := p.UpsertAccounts([]pool.AccountInput{input("a@b.com", 100)}); err != nil {
		t.Fatalf("UpsertAccounts: %v", err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

// get performs an authenticated GET and returns the recorder.
func get(t *testing.T, s *Server, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func post(t *testing.T, s *Server, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func del(t *testing.T, s *Server, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// decodeUploadResp decodes a {"code":0,"data":{"added":N,"updated":M}} body.
func decodeUploadResp(t *testing.T, rec *httptest.ResponseRecorder) (added, updated int) {
	t.Helper()
	var resp struct {
		Code int `json:"code"`
		Data struct {
			Added   int `json:"added"`
			Updated int `json:"updated"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.Data.Added, resp.Data.Updated
}

// TestFrontendRemoved verifies GET / returns 404 and no admin panel is served
// (SPEC-hardening 3.1).
func TestFrontendRemoved(t *testing.T) {
	s := New(nil, minimalPool(t), "pw", []string{"inclusionai/ling-2.6-flash"})
	rec := get(t, s, "/", "pw")
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET / = %d, want 404", rec.Code)
	}
	// /web/* must not exist either.
	rec = get(t, s, "/web/app.js", "pw")
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /web/app.js = %d, want 404", rec.Code)
	}
	// /healthz remains public.
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", rec.Code)
	}
}

// TestModelsUnauthorized verifies /v1/models requires a bearer token (auth is
// enforced before any upstream call; the client here is nil).
func TestModelsUnauthorized(t *testing.T) {
	s := New(nil, minimalPool(t), "pw", []string{"inclusionai/ling-2.6-flash"})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /v1/models unauthenticated = %d, want 401", rec.Code)
	}
}

// TestModelNotAllowed verifies a chat request with a non-whitelisted model is
// rejected with 400 model_not_allowed before any upstream call (SPEC 3.5).
func TestModelNotAllowed(t *testing.T) {
	s := New(nil, minimalPool(t), "pw", []string{"inclusionai/ling-2.6-flash"})
	body := `{"model":"glm/glm-5.2","messages":[{"role":"user","content":"hi"}]}`
	rec := post(t, s, "/v1/chat/completions", "pw", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var resp struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error.Type != "model_not_allowed" {
		t.Errorf("error.type = %q, want model_not_allowed", resp.Error.Type)
	}
}

// TestUploadRequiresAuth verifies POST /api/accounts is behind Bearer auth.
func TestUploadRequiresAuth(t *testing.T) {
	s := New(nil, minimalPool(t), "pw", []string{"inclusionai/ling-2.6-flash"})
	rec := post(t, s, "/api/accounts", "", `{"email":"x@y.com","identity_token":"id"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated upload = %d, want 401", rec.Code)
	}
	// Wrong token also 401.
	rec = post(t, s, "/api/accounts", "wrong", `{"email":"x@y.com","identity_token":"id"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong-token upload = %d, want 401", rec.Code)
	}
}

// TestUploadAddsAndStats verifies a new upload bumps stats (SPEC-upload §5).
func TestUploadAddsAndStats(t *testing.T) {
	p := minimalPool(t)
	s := New(nil, p, "pw", []string{"inclusionai/ling-2.6-flash"})

	before := p.Stats().TotalAccounts
	body := `{"email":"new@b.com","wallet_address":"0x2","tier":"basic","identity_token":"id.new","access_token":"acc.new","refresh_token":"ref.new","available_credits":100,"expires_at":1785723984}`
	rec := post(t, s, "/api/accounts", "pw", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload status = %d, body=%s", rec.Code, rec.Body.String())
	}
	added, updated := decodeUploadResp(t, rec)
	if added != 1 || updated != 0 {
		t.Errorf("upload added=%d updated=%d, want 1/0", added, updated)
	}
	if got := p.Stats().TotalAccounts; got != before+1 {
		t.Errorf("total accounts = %d, want %d", got, before+1)
	}
}

// TestUploadBatchArray verifies an array body upserts multiple accounts.
func TestUploadBatchArray(t *testing.T) {
	s := New(nil, minimalPool(t), "pw", []string{"inclusionai/ling-2.6-flash"})
	body := `[
		{"email":"x@b.com","identity_token":"id.x","available_credits":10},
		{"email":"y@b.com","identity_token":"id.y","available_credits":20}
	]`
	rec := post(t, s, "/api/accounts", "pw", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload status = %d, body=%s", rec.Code, rec.Body.String())
	}
	added, updated := decodeUploadResp(t, rec)
	if added != 2 || updated != 0 {
		t.Errorf("batch upload added=%d updated=%d, want 2/0", added, updated)
	}
}

// TestUploadRepeatUpdates verifies re-uploading the same email reports updated
// and replaces the token (SPEC-upload §5).
func TestUploadRepeatUpdates(t *testing.T) {
	p := minimalPool(t)
	s := New(nil, p, "pw", []string{"inclusionai/ling-2.6-flash"})
	body := `{"email":"a@b.com","identity_token":"id.updated","available_credits":7}`
	rec := post(t, s, "/api/accounts", "pw", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload status = %d", rec.Code)
	}
	added, updated := decodeUploadResp(t, rec)
	if added != 0 || updated != 1 {
		t.Errorf("repeat upload added=%d updated=%d, want 0/1", added, updated)
	}
	a := p.Get("a@b.com")
	if a.AvailableCredits != 7 || a.IdentityToken != "id.updated" {
		t.Errorf("account not updated: credits=%d token=%q", a.AvailableCredits, a.IdentityToken)
	}
}

// TestUploadInvalidBody verifies malformed / incomplete bodies are rejected
// wholesale before any write.
func TestUploadInvalidBody(t *testing.T) {
	p := minimalPool(t)
	before := p.Stats().TotalAccounts
	s := New(nil, p, "pw", []string{"inclusionai/ling-2.6-flash"})
	for _, body := range []string{
		`{not json`,
		`[]`,
		`{"email":"","identity_token":"id"}`,
		`{"email":"x@b.com","identity_token":""}`,
	} {
		rec := post(t, s, "/api/accounts", "pw", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q = %d, want 400", body, rec.Code)
		}
	}
	if got := p.Stats().TotalAccounts; got != before {
		t.Errorf("invalid uploads changed accounts: %d -> %d", before, got)
	}
}

// TestDeleteAccountPhysical verifies DELETE /api/accounts/{email} removes the
// account and GET /api/accounts no longer lists it (SPEC-upload §5).
func TestDeleteAccountPhysical(t *testing.T) {
	p := minimalPool(t)
	s := New(nil, p, "pw", []string{"inclusionai/ling-2.6-flash"})

	rec := del(t, s, "/api/accounts/a@b.com", "pw")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d", rec.Code)
	}
	if got := p.Stats().TotalAccounts; got != 0 {
		t.Errorf("total accounts after delete = %d, want 0", got)
	}
	// DELETE of a missing account returns 404.
	rec = del(t, s, "/api/accounts/a@b.com", "pw")
	if rec.Code != http.StatusNotFound {
		t.Errorf("second delete = %d, want 404", rec.Code)
	}
	// The list no longer contains the account.
	rec = get(t, s, "/api/accounts", "pw")
	var body struct {
		Accounts []pool.Masked `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Accounts) != 0 {
		t.Errorf("accounts after delete = %d, want 0", len(body.Accounts))
	}
}

// TestResurrectViaUpload verifies re-uploading after DELETE re-creates the
// account (SPEC-upload §5).
func TestResurrectViaUpload(t *testing.T) {
	p := minimalPool(t)
	s := New(nil, p, "pw", []string{"inclusionai/ling-2.6-flash"})
	if rec := del(t, s, "/api/accounts/a@b.com", "pw"); rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d", rec.Code)
	}
	body := `{"email":"a@b.com","identity_token":"id.back","available_credits":80}`
	rec := post(t, s, "/api/accounts", "pw", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("resurrect upload status = %d", rec.Code)
	}
	added, updated := decodeUploadResp(t, rec)
	if added != 1 || updated != 0 {
		t.Errorf("resurrect added=%d updated=%d, want 1/0", added, updated)
	}
	if p.Get("a@b.com") == nil {
		t.Errorf("account not resurrected")
	}
}

// TestStatsNoRemoved verifies /api/stats has no removed counter (SPEC-upload
// §2.2) and physical delete drops total_accounts.
func TestStatsNoRemoved(t *testing.T) {
	p := minimalPool(t)
	s := New(nil, p, "pw", []string{"inclusionai/ling-2.6-flash"})
	rec := get(t, s, "/api/stats", "pw")
	var st struct {
		Total   int `json:"total_accounts"`
		Removed int `json:"removed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Removed != 0 {
		t.Errorf("removed counter leaked into stats: %d", st.Removed)
	}
	if st.Total != 1 {
		t.Errorf("total = %d, want 1", st.Total)
	}
	p.Delete("a@b.com")
	rec = get(t, s, "/api/stats", "pw")
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Total != 0 {
		t.Errorf("total after delete = %d, want 0", st.Total)
	}
}

// TestStatsCooldownField verifies /api/stats carries a cooldown counter
// (SPEC-cooldown §2.5). A 0-credit upload maps to cooldown on pool load.
func TestStatsCooldownField(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	p, err := pool.New(nil, dbPath, pool.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.UpsertAccounts([]pool.AccountInput{input("a@b.com", 0)}); err != nil {
		t.Fatal(err)
	}
	p.Close()

	p2, err := pool.New(nil, dbPath, pool.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	s := New(nil, p2, "pw", []string{"inclusionai/ling-2.6-flash"})

	rec := get(t, s, "/api/stats", "pw")
	var st struct {
		Cooldown int `json:"cooldown"`
		Disabled int `json:"disabled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Cooldown != 1 {
		t.Errorf("cooldown = %d, want 1 (0-credit account on load)", st.Cooldown)
	}
}

// TestListAccountsStatusFilter verifies GET /api/accounts?status=cooldown
// returns only cooldown accounts (SPEC-cooldown §2.5).
func TestListAccountsStatusFilter(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	p, err := pool.New(nil, dbPath, pool.Options{})
	if err != nil {
		t.Fatal(err)
	}
	// a@b.com: 0 credits -> cooldown on load. c@d.com: positive -> active.
	if _, _, err := p.UpsertAccounts([]pool.AccountInput{
		input("a@b.com", 0), input("c@d.com", 100),
	}); err != nil {
		t.Fatal(err)
	}
	p.Close()

	p2, err := pool.New(nil, dbPath, pool.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	s := New(nil, p2, "pw", []string{"inclusionai/ling-2.6-flash"})

	rec := get(t, s, "/api/accounts?status=cooldown", "pw")
	var body struct {
		Accounts []pool.Masked `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Accounts) != 1 || body.Accounts[0].Email != "a@b.com" {
		t.Errorf("cooldown-filtered accounts = %+v, want only a@b.com", body.Accounts)
	}

	// Without a filter every account is returned.
	rec = get(t, s, "/api/accounts", "pw")
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Accounts) != 2 {
		t.Errorf("unfiltered accounts = %d, want 2", len(body.Accounts))
	}
}

// TestReloadRouteGone verifies POST /api/accounts/reload is removed
// (SPEC-upload §2.3). The path is still claimed by the DELETE {email} wildcard,
// so the mux answers 405 Method Not Allowed rather than 404 — either way the
// reload handler no longer exists (no 200).
func TestReloadRouteGone(t *testing.T) {
	s := New(nil, minimalPool(t), "pw", []string{"inclusionai/ling-2.6-flash"})
	rec := post(t, s, "/api/accounts/reload", "pw", "")
	if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/accounts/reload = %d, want 404 or 405", rec.Code)
	}
}

// TestModelsShortNames verifies /v1/models exposes the 12 whitelisted models as
// short names (provider prefix stripped) and carries the upstream long name in
// upstream_id, while non-whitelisted upstream models stay hidden (SPEC
// model-alias §3.2).
func TestModelsShortNames(t *testing.T) {
	s := minimalServer(t, twelveModels, &stubUpstream{models: stubModels()})
	rec := get(t, s, "/v1/models", "pw")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/models = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data []map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Data) != len(twelveModels) {
		t.Fatalf("model count = %d, want %d", len(resp.Data), len(twelveModels))
	}
	seen := make(map[string]bool, len(resp.Data))
	for _, m := range resp.Data {
		id, _ := m["id"].(string)
		up, _ := m["upstream_id"].(string)
		if strings.Contains(id, "/") {
			t.Errorf("model id %q still has a provider prefix", id)
		}
		// The displayed short name must map back to the upstream long name.
		if !contains(twelveModels, up) {
			t.Errorf("upstream_id %q not allowlisted", up)
		}
		if convertShortName(up) != id {
			t.Errorf("id=%q does not match upstream_id %q", id, up)
		}
		if seen[id] {
			t.Errorf("duplicate model id %q", id)
		}
		seen[id] = true
	}
	for _, id := range twelveModels {
		if !seen[convertShortName(id)] {
			t.Errorf("whitelisted model %q missing from /v1/models", convertShortName(id))
		}
	}
	// Tier-gated models must not appear at all.
	for _, m := range resp.Data {
		if m["upstream_id"] == "openai/gpt-5.6-sol" || m["upstream_id"] == "anthropic/claude-fable-5" {
			t.Errorf("hidden model leaked into /v1/models: %v", m)
		}
	}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func convertShortName(id string) string {
	if i := strings.Index(id, "/"); i >= 0 {
		return id[i+1:]
	}
	return id
}

// TestChatCompletionsShortAndLongName verifies both the short name
// ("claude-sonnet-5") and the long name ("anthropic/claude-sonnet-5") are
// accepted, resolve to the allowlisted upstream id, and are forwarded as the
// upstream model (SPEC model-alias §3.3).
func TestChatCompletionsShortAndLongName(t *testing.T) {
	for _, model := range []string{"claude-sonnet-5", "anthropic/claude-sonnet-5"} {
		stub := &stubUpstream{
			resp: &upstream.Response{
				ID:    "resp-1",
				Model: "anthropic/claude-sonnet-5",
				Output: []upstream.OutputItem{{
					Type:    "message",
					Content: []upstream.ContentBlock{{Type: "output_text", Text: "hi"}},
				}},
				Usage: upstream.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
			},
		}
		s := minimalServer(t, twelveModels, stub)
		body := `{"model":"` + model + `","messages":[{"role":"user","content":"hello"}]}`
		rec := post(t, s, "/v1/chat/completions", "pw", body)
		if rec.Code != http.StatusOK {
			t.Errorf("model %q = %d, want 200, body=%s", model, rec.Code, rec.Body.String())
			continue
		}
		// The upstream payload must carry the resolved long name.
		if stub.chatCalls != 1 {
			t.Errorf("model %q: chat calls = %d, want 1", model, stub.chatCalls)
		}
		if got := stub.lastBody["model"]; got != "anthropic/claude-sonnet-5" {
			t.Errorf("model %q forwarded as %v, want anthropic/claude-sonnet-5", model, got)
		}
		var cc struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &cc); err != nil {
			t.Fatal(err)
		}
		// The response echoes the client-visible short name regardless of how the
		// model was requested (SPEC-usage-fix P1-1).
		if cc.Model != "claude-sonnet-5" {
			t.Errorf("response model = %q, want short name claude-sonnet-5", cc.Model)
		}
	}
}

// TestChatCompletionsOtherWhitelistedModels verifies a couple more short names
// from the 12 (SPEC model-alias §4 acceptance).
func TestChatCompletionsOtherWhitelistedModels(t *testing.T) {
	for _, model := range []string{"grok-4.5", "gemini-3.1-pro-preview"} {
		stub := &stubUpstream{
			resp: &upstream.Response{
				ID: "resp-2", Model: "grok/grok-4.5",
				Output: []upstream.OutputItem{{Type: "message", Content: []upstream.ContentBlock{{Type: "output_text", Text: "hi"}}}},
				Usage:  upstream.Usage{TotalTokens: 1},
			},
		}
		s := minimalServer(t, twelveModels, stub)
		body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
		rec := post(t, s, "/v1/chat/completions", "pw", body)
		if rec.Code != http.StatusOK {
			t.Errorf("model %q = %d, want 200, body=%s", model, rec.Code, rec.Body.String())
		}
	}
}

// TestChatUnknownModelRejected verifies a non-whitelisted model — short name or
// long name — is rejected with 400 model_not_allowed after resolution, with no
// upstream call made (SPEC model-alias §3.3 + §4).
func TestChatUnknownModelRejected(t *testing.T) {
	stub := &stubUpstream{}
	s := minimalServer(t, twelveModels, stub)
	for _, model := range []string{
		"anthropic/claude-opus-5", // tier-gated, not allowlisted
		"some-random-model",       // unknown short name
		"anthropic/claude-fable-5",
	} {
		body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
		rec := post(t, s, "/v1/chat/completions", "pw", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("model %q = %d, want 400", model, rec.Code)
			continue
		}
		var resp struct {
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Error.Type != "model_not_allowed" {
			t.Errorf("model %q error.type = %q, want model_not_allowed", model, resp.Error.Type)
		}
	}
	if stub.chatCalls != 0 {
		t.Errorf("chat was called %d times for rejected models, want 0", stub.chatCalls)
	}
}

// streamedResponse is a complete upstream Response for streaming tests.
func streamedResponse() *upstream.Response {
	return &upstream.Response{
		ID:    "resp-stream-1",
		Model: "anthropic/claude-sonnet-5",
		Output: []upstream.OutputItem{{
			Type:    "message",
			Content: []upstream.ContentBlock{{Type: "output_text", Text: "hi from stream"}},
		}},
		Usage: upstream.Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5},
	}
}

// TestStreamSkeletonErrorNot503 verifies the 503 root cause regression: an
// upstream event carrying a skeleton response object with an EMPTY (non-nil)
// error object must not be treated as an account failure. The stream must
// succeed and emit a full SSE body.
func TestStreamSkeletonErrorNot503(t *testing.T) {
	skeleton := &upstream.Response{ID: "resp-skel", Error: &upstream.RespError{}}
	stub := &stubUpstream{streamResp: skeleton}
	s := minimalServer(t, twelveModels, stub)
	body := `{"model":"claude-sonnet-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	rec := post(t, s, "/v1/chat/completions", "pw", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Errorf("stream missing [DONE]: %q", rec.Body.String())
	}
}

// TestStreamEmptyErrorWithContent verifies an event whose response carries
// content alongside an empty error object is treated as a success (the empty
// error must not trigger a recordFailure / account switch).
func TestStreamEmptyErrorWithContent(t *testing.T) {
	ok := streamedResponse()
	ok.Error = &upstream.RespError{}
	stub := &stubUpstream{streamResp: ok}
	s := minimalServer(t, twelveModels, stub)
	body := `{"model":"claude-sonnet-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	rec := post(t, s, "/v1/chat/completions", "pw", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "hi from stream") {
		t.Errorf("stream missing content: %q", rec.Body.String())
	}
	// The account must have recorded a success (consecutive auth failures reset,
	// still active and healthy).
	a := s.pool.Get("a@b.com")
	if a == nil {
		t.Fatal("account missing")
	}
	if a.Status != pool.StatusActive {
		t.Errorf("account status = %q, want active", a.Status)
	}
}

// TestStreamInsufficientBalance402 verifies an embedded "insufficient balance"
// error on the only account surfaces as 402 (not 503): the client must be able
// to tell "all accounts out of balance" from "all accounts unavailable"
// (P1-3). The body is an SSE frame carrying the error object (2-5).
func TestStreamInsufficientBalance402(t *testing.T) {
	stub := &stubUpstream{streamResp: &upstream.Response{
		Error: &upstream.RespError{Message: "insufficient balance", Code: "payment_required"},
	}}
	s := minimalServer(t, twelveModels, stub)
	body := `{"model":"claude-sonnet-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	rec := post(t, s, "/v1/chat/completions", "pw", body)
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402, body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}
	if !strings.Contains(rec.Body.String(), "all accounts have insufficient balance") {
		t.Errorf("body missing balance message: %q", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Errorf("body missing [DONE]: %q", rec.Body.String())
	}
}

// TestNonStreamInsufficientBalance402 verifies the non-streaming path maps an
// all-accounts-insufficient exhaustion to HTTP 402 (P1-3). It uses an upstream
// stub whose Chat returns ErrInsufficient every time.
func TestNonStreamInsufficientBalance402(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	p, err := pool.New(nil, dbPath, pool.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, _, err := p.UpsertAccounts([]pool.AccountInput{input("a@b.com", 100)}); err != nil {
		t.Fatal(err)
	}

	stub := &insufficientStub{}
	s := New(stub, p, "pw", twelveModels)
	body := `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`
	rec := post(t, s, "/v1/chat/completions", "pw", body)
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402, body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error.Message != "account balance exhausted" {
		t.Errorf("error message = %q, want account balance exhausted", resp.Error.Message)
	}
	// The exhausted account must be parked in cooldown, not disabled (P1-2).
	a := p.Get("a@b.com")
	if a.Status != pool.StatusCooldown {
		t.Errorf("account status = %q, want cooldown after insufficient balance", a.Status)
	}
}

// insufficientStub is an upstream stub whose Chat always returns an
// insufficient-balance error.
type insufficientStub struct{}

func (s *insufficientStub) Models(ctx context.Context) ([]upstream.Model, error) { return nil, nil }
func (s *insufficientStub) Chat(ctx context.Context, identityToken string, payload []byte) (*upstream.Response, error) {
	return nil, &upstream.APIError{Kind: upstream.ErrInsufficient, StatusCode: http.StatusPaymentRequired, Code: "payment_required", Message: "insufficient balance"}
}
func (s *insufficientStub) Stream(ctx context.Context, identityToken string, payload []byte) (<-chan *upstream.Response, io.Closer, error) {
	return nil, nil, nil
}
func (s *insufficientStub) GetBalance(ctx context.Context, identityToken string) (*upstream.Balance, error) {
	return nil, nil
}

// TestNonStreamModelShortNameAndDetails verifies the SPEC-usage-fix P1-1/P0-2
// acceptance: the non-streaming response echoes the client-requested short name
// (glm-5.2) instead of the upstream long/canonical id, and usage carries
// prompt_tokens_details.cached_tokens. The stub upstream returns an upstream
// canonical model id (accounts/fireworks/models/glm-5p2) and no cache details.
func TestNonStreamModelShortNameAndDetails(t *testing.T) {
	stub := &stubUpstream{
		resp: &upstream.Response{
			ID:    "resp-1",
			Model: "accounts/fireworks/models/glm-5p2",
			Output: []upstream.OutputItem{{
				Type:    "message",
				Content: []upstream.ContentBlock{{Type: "output_text", Text: "OK"}},
			}},
			Usage: upstream.Usage{PromptTokens: 2563, CompletionTokens: 14, TotalTokens: 2577},
		},
	}
	s := minimalServer(t, twelveModels, stub)
	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`
	rec := post(t, s, "/v1/chat/completions", "pw", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var cc struct {
		Model string `json:"model"`
		Usage struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			TotalTokens         int `json:"total_tokens"`
			PromptTokensDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cc); err != nil {
		t.Fatal(err)
	}
	if cc.Model != "glm-5.2" {
		t.Errorf("response model = %q, want short name glm-5.2", cc.Model)
	}
	if cc.Usage.TotalTokens != 2577 {
		t.Errorf("usage total = %d, want 2577", cc.Usage.TotalTokens)
	}
	if cc.Usage.PromptTokensDetails == nil {
		t.Fatal("usage.prompt_tokens_details missing; want explicit {\"cached_tokens\":0}")
	}
	if cc.Usage.PromptTokensDetails.CachedTokens != 0 {
		t.Errorf("cached_tokens = %d, want 0 (upstream provided none)", cc.Usage.PromptTokensDetails.CachedTokens)
	}
}

// TestStreamUsageNormalized verifies the SPEC-usage-fix P0-1 acceptance on the
// streaming path: when the upstream streamed response.completed carries
// Responses-style usage (input_tokens/output_tokens), the final chunk's usage
// must report non-zero prompt/completion tokens with prompt+completion==total.
// The stub stream returns an input_tokens-only usage, exactly what the upstream
// sends in the streamed event (docs/DISCREPANCIES §5).
func TestStreamUsageNormalized(t *testing.T) {
	stub := &stubUpstream{streamResp: &upstream.Response{
		ID:    "resp-stream-usage",
		Model: "accounts/fireworks/models/glm-5p2",
		Output: []upstream.OutputItem{{
			Type:    "message",
			Content: []upstream.ContentBlock{{Type: "output_text", Text: "hi from stream"}},
		}},
		Usage: upstream.Usage{InputTokens: 2563, OutputTokens: 17, TotalTokens: 2580},
	}}
	s := minimalServer(t, twelveModels, stub)
	body := `{"model":"glm-5.2","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`
	rec := post(t, s, "/v1/chat/completions", "pw", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	// Walk the SSE data payloads; the final data frame before [DONE] carries the
	// finish chunk with usage.
	var lastUsage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		TotalTokens         int `json:"total_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	}
	var lastModel string
	lines := strings.Split(rec.Body.String(), "\n")
	for _, line := range lines {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var frame struct {
			Model string `json:"model"`
			Usage *struct {
				PromptTokens        int `json:"prompt_tokens"`
				CompletionTokens    int `json:"completion_tokens"`
				TotalTokens         int `json:"total_tokens"`
				PromptTokensDetails *struct {
					CachedTokens int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &frame); err != nil {
			t.Fatalf("bad SSE frame %q: %v", payload, err)
		}
		lastModel = frame.Model
		if frame.Usage != nil {
			lastUsage = frame.Usage
		}
	}
	if lastUsage == nil {
		t.Fatal("final chunk missing usage (include_usage not honored)")
	}
	if lastUsage.PromptTokens == 0 || lastUsage.CompletionTokens == 0 {
		t.Errorf("streamed usage prompt/completion = %d/%d, want non-zero (input_tokens normalized)", lastUsage.PromptTokens, lastUsage.CompletionTokens)
	}
	if lastUsage.PromptTokens+lastUsage.CompletionTokens != lastUsage.TotalTokens {
		t.Errorf("prompt+completion (%d+%d) != total (%d)", lastUsage.PromptTokens, lastUsage.CompletionTokens, lastUsage.TotalTokens)
	}
	if lastModel != "glm-5.2" {
		t.Errorf("stream model = %q, want short name glm-5.2", lastModel)
	}
	if lastUsage.PromptTokensDetails == nil {
		t.Error("streamed usage.prompt_tokens_details missing; want explicit {\"cached_tokens\":0}")
	}
}

// TestNonStreamNormalizedInputTokens verifies the non-streaming path also uses
// the normalized token counts, so a stream-naming upstream usage (input_tokens)
// still yields correct prompt/completion values (SPEC-usage-fix P0-1).
func TestNonStreamNormalizedInputTokens(t *testing.T) {
	stub := &stubUpstream{
		resp: &upstream.Response{
			ID:    "resp-1",
			Model: "inclusionai/ling-2.6-flash",
			Output: []upstream.OutputItem{{
				Type:    "message",
				Content: []upstream.ContentBlock{{Type: "output_text", Text: "hi"}},
			}},
			Usage: upstream.Usage{InputTokens: 12, OutputTokens: 7, TotalTokens: 19},
		},
	}
	s := minimalServer(t, twelveModels, stub)
	body := `{"model":"ling-2.6-flash","messages":[{"role":"user","content":"hi"}]}`
	rec := post(t, s, "/v1/chat/completions", "pw", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var cc struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cc); err != nil {
		t.Fatal(err)
	}
	if cc.Usage.PromptTokens != 12 || cc.Usage.CompletionTokens != 7 || cc.Usage.TotalTokens != 19 {
		t.Errorf("usage = %+v, want prompt=12 completion=7 total=19", cc.Usage)
	}
}

// TestLive402AndCooldownE2E is the SPEC §4 acceptance check over a real TCP
// HTTP stack (httptest.NewServer + http.Client), not an in-process recorder:
// an all-accounts-insufficient request must return HTTP 402 (not 503), and the
// exhausted account must be parked in cooldown.
func TestLive402AndCooldownE2E(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	p, err := pool.New(nil, dbPath, pool.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, _, err := p.UpsertAccounts([]pool.AccountInput{input("a@b.com", 100)}); err != nil {
		t.Fatal(err)
	}

	s := New(&insufficientStub{}, p, "pw", twelveModels)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(
		`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer pw")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("live status = %d, want 402, body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "account balance exhausted") {
		t.Errorf("live body missing balance message: %s", body)
	}
	a := p.Get("a@b.com")
	if a.Status != pool.StatusCooldown {
		t.Errorf("live account status = %q, want cooldown after 402", a.Status)
	}
}

// retryStub is an upstreamClient that fails the first failN Chat calls with err
// and succeeds afterwards, recording the identity token used per call so tests
// can distinguish a same-account retry from an account switch.
type retryStub struct {
	failN  int
	err    error
	resp   *upstream.Response
	calls  int
	tokens []string
}

func (s *retryStub) Models(ctx context.Context) ([]upstream.Model, error) { return nil, nil }

func (s *retryStub) Chat(ctx context.Context, identityToken string, payload []byte) (*upstream.Response, error) {
	s.calls++
	s.tokens = append(s.tokens, identityToken)
	if s.calls <= s.failN {
		return nil, s.err
	}
	return s.resp, nil
}

func (s *retryStub) Stream(ctx context.Context, identityToken string, payload []byte) (<-chan *upstream.Response, io.Closer, error) {
	return nil, nil, nil
}

func (s *retryStub) GetBalance(ctx context.Context, identityToken string) (*upstream.Balance, error) {
	return &upstream.Balance{AvailableCredits: 100}, nil
}

// TestNonStreamUpstream5xxSwitchesNoSameAccountRetry verifies P0-1: an upstream
// 5xx (ErrUpstream) is a model-level defect, so the retry loop must switch
// accounts instead of retrying the same account. With a single account and the
// default 3-attempt budget, Chat must be called exactly once per attempt (3
// total) — the old same-account second call would have doubled it to 6 — and
// the exhaustion surfaces as 502 with the upstream error.
func TestNonStreamUpstream5xxSwitchesNoSameAccountRetry(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	p, err := pool.New(nil, dbPath, pool.Options{RetryBackoff: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, _, err := p.UpsertAccounts([]pool.AccountInput{input("a@b.com", 100)}); err != nil {
		t.Fatal(err)
	}

	// failN=100: the stub fails every Chat call so all 3 retry-budget attempts
	// are exercised (the default budget is 3).
	stub := &retryStub{failN: 100, err: &upstream.APIError{Kind: upstream.ErrUpstream, StatusCode: http.StatusInternalServerError, Message: "internal_error"}}
	s := New(stub, p, "pw", twelveModels)
	body := `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`
	rec := post(t, s, "/v1/chat/completions", "pw", body)
	if stub.calls != 3 {
		t.Errorf("chat calls = %d, want 3 (one per attempt, no same-account retry)", stub.calls)
	}
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

// TestNonStreamNetworkErrorRetriesSameAccountOnce verifies P0-1: a network error
// (ErrUnknown) is potentially transient, so the retry loop retries the SAME
// account once before switching. The stub fails the first call and succeeds on
// the same-account retry, so exactly 2 Chat calls on the same identity token
// must be made and the request must succeed.
func TestNonStreamNetworkErrorRetriesSameAccountOnce(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	p, err := pool.New(nil, dbPath, pool.Options{RetryBackoff: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, _, err := p.UpsertAccounts([]pool.AccountInput{
		input("a@b.com", 100), input("c@d.com", 100),
	}); err != nil {
		t.Fatal(err)
	}

	stub := &retryStub{
		failN: 1,
		err:   &upstream.APIError{Kind: upstream.ErrUnknown, Message: "connection reset"},
		resp: &upstream.Response{
			ID:    "resp-ok",
			Model: "anthropic/claude-sonnet-5",
			Output: []upstream.OutputItem{{
				Type:    "message",
				Content: []upstream.ContentBlock{{Type: "output_text", Text: "ok"}},
			}},
			Usage: upstream.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		},
	}
	s := New(stub, p, "pw", twelveModels)
	body := `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`
	rec := post(t, s, "/v1/chat/completions", "pw", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if stub.calls != 2 {
		t.Errorf("chat calls = %d, want 2 (original + same-account retry)", stub.calls)
	}
	if len(stub.tokens) != 2 || stub.tokens[0] != stub.tokens[1] {
		t.Errorf("retry was not on the same account: tokens = %v", stub.tokens)
	}
}

// ---- ticket 12: 续写协议 conversation_id / previous_response_id ----

// TestContinuationNonStream verifies the non-streaming path round-trip: a
// request carrying conversation_id/previous_response_id forwards them to the
// upstream, and the response echoes conversation_id + previous_response_id
// (= the upstream response id).
func TestContinuationNonStream(t *testing.T) {
	stub := &stubUpstream{
		resp: &upstream.Response{
			ID:    "msg_resp_77",
			Model: "anthropic/claude-sonnet-5",
			Output: []upstream.OutputItem{{
				Type:    "message",
				Content: []upstream.ContentBlock{{Type: "output_text", Text: "continued"}},
			}},
			Usage: upstream.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		},
	}
	s := minimalServer(t, twelveModels, stub)
	body := `{"model":"claude-sonnet-5","conversation_id":"conv-http-1","previous_response_id":"msg_prev_9","messages":[{"role":"user","content":"continue"}]}`
	rec := post(t, s, "/v1/chat/completions", "pw", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	// Request side: both fields must reach the upstream payload verbatim.
	if got := stub.lastBody["conversation_id"]; got != "conv-http-1" {
		t.Errorf("upstream conversation_id = %v, want conv-http-1", got)
	}
	if got := stub.lastBody["previous_response_id"]; got != "msg_prev_9" {
		t.Errorf("upstream previous_response_id = %v, want msg_prev_9", got)
	}
	// Response side: conversation_id echoed + previous_response_id = response.id.
	var cc struct {
		ConversationID     string `json:"conversation_id"`
		PreviousResponseID string `json:"previous_response_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cc); err != nil {
		t.Fatal(err)
	}
	if cc.ConversationID != "conv-http-1" {
		t.Errorf("response conversation_id = %q, want conv-http-1", cc.ConversationID)
	}
	if cc.PreviousResponseID != "msg_resp_77" {
		t.Errorf("response previous_response_id = %q, want msg_resp_77 (upstream id)", cc.PreviousResponseID)
	}
}

// TestContinuationStream verifies the streaming path: the final chunk carries
// conversation_id + previous_response_id, and the request fields reach the
// upstream payload.
func TestContinuationStream(t *testing.T) {
	stub := &stubUpstream{streamResp: &upstream.Response{
		ID:    "msg_resp_88",
		Model: "anthropic/claude-sonnet-5",
		Output: []upstream.OutputItem{{
			Type:    "message",
			Content: []upstream.ContentBlock{{Type: "output_text", Text: "hi from stream"}},
		}},
		Usage: upstream.Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5},
	}}
	s := minimalServer(t, twelveModels, stub)
	body := `{"model":"claude-sonnet-5","stream":true,"conversation_id":"conv-http-s1","previous_response_id":"msg_prev_s1","messages":[{"role":"user","content":"continue"}]}`
	rec := post(t, s, "/v1/chat/completions", "pw", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	// Request side passthrough.
	if got := stub.lastBody["conversation_id"]; got != "conv-http-s1" {
		t.Errorf("upstream conversation_id = %v, want conv-http-s1", got)
	}
	if got := stub.lastBody["previous_response_id"]; got != "msg_prev_s1" {
		t.Errorf("upstream previous_response_id = %v, want msg_prev_s1", got)
	}
	// Response side: the final data frame (before [DONE]) carries the ids.
	var lastConv, lastPrev string
	lines := strings.Split(rec.Body.String(), "\n")
	for _, line := range lines {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var frame struct {
			ConversationID     string `json:"conversation_id"`
			PreviousResponseID string `json:"previous_response_id"`
		}
		if err := json.Unmarshal([]byte(payload), &frame); err != nil {
			t.Fatalf("bad SSE frame %q: %v", payload, err)
		}
		if frame.ConversationID != "" {
			lastConv = frame.ConversationID
		}
		if frame.PreviousResponseID != "" {
			lastPrev = frame.PreviousResponseID
		}
	}
	if lastConv != "conv-http-s1" {
		t.Errorf("stream conversation_id = %q, want conv-http-s1", lastConv)
	}
	if lastPrev != "msg_resp_88" {
		t.Errorf("stream previous_response_id = %q, want msg_resp_88", lastPrev)
	}
}

// TestContinuationGenerated verifies a request without client continuation
// fields still carries a gateway-generated conversation_id and the upstream
// response id as previous_response_id — the fields are always attached so a
// client always has the handle to continue after length truncation.
func TestContinuationGenerated(t *testing.T) {
	stub := &stubUpstream{
		resp: &upstream.Response{
			ID:    "msg_plain",
			Model: "anthropic/claude-sonnet-5",
			Output: []upstream.OutputItem{{
				Type:    "message",
				Content: []upstream.ContentBlock{{Type: "output_text", Text: "plain"}},
			}},
			Usage: upstream.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		},
	}
	s := minimalServer(t, twelveModels, stub)
	body := `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`
	rec := post(t, s, "/v1/chat/completions", "pw", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	// The request had no conversation_id, so the gateway generated one; the
	// same value must reach the upstream payload and be echoed back.
	upConv, _ := stub.lastBody["conversation_id"].(string)
	if upConv == "" {
		t.Fatalf("upstream conversation_id = %q, want a generated non-empty value", upConv)
	}
	var cc struct {
		ConversationID     string `json:"conversation_id"`
		PreviousResponseID string `json:"previous_response_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cc); err != nil {
		t.Fatal(err)
	}
	if cc.ConversationID != upConv {
		t.Errorf("response conversation_id = %q, want %q (echoed generated id)", cc.ConversationID, upConv)
	}
	if cc.PreviousResponseID != "msg_plain" {
		t.Errorf("response previous_response_id = %q, want msg_plain", cc.PreviousResponseID)
	}
}

// ---- ticket 13: 网关侧自动续写 ----

// segUpstream is an upstreamClient that serves a queue of responses: each Chat
// call pops the next response, each Stream call pops the next response. failFrom
// (1-based) makes every call at that index or later fail with err so tests can
// exercise the "a continuation segment exhausts its retries, keep the
// accumulated text" path — the continuation loop's fetch runs the full
// doWithRetry budget (default 5) inside one segment, so a single failed call
// would just be retried. lastBodies records every payload so tests can assert
// the continuation input was appended.
type segUpstream struct {
	responses []*upstream.Response
	failFrom  int
	err       error
	chatCalls  int
	streamCalls int
	lastBodies []map[string]interface{}
}

func (s *segUpstream) Models(ctx context.Context) ([]upstream.Model, error) { return nil, nil }

func (s *segUpstream) Chat(ctx context.Context, identityToken string, payload []byte) (*upstream.Response, error) {
	s.chatCalls++
	s.record(payload)
	if s.failFrom > 0 && s.chatCalls >= s.failFrom {
		return nil, s.err
	}
	idx := s.chatCalls - 1
	if idx < len(s.responses) {
		return s.responses[idx], nil
	}
	return s.responses[len(s.responses)-1], nil
}

func (s *segUpstream) Stream(ctx context.Context, identityToken string, payload []byte) (<-chan *upstream.Response, io.Closer, error) {
	s.streamCalls++
	s.record(payload)
	ch := make(chan *upstream.Response, 1)
	idx := s.streamCalls - 1
	if idx < len(s.responses) {
		ch <- s.responses[idx]
	}
	close(ch)
	return ch, io.NopCloser(strings.NewReader("")), nil
}

func (s *segUpstream) GetBalance(ctx context.Context, identityToken string) (*upstream.Balance, error) {
	return &upstream.Balance{AvailableCredits: 100}, nil
}

func (s *segUpstream) record(payload []byte) {
	var body map[string]interface{}
	if err := json.Unmarshal(payload, &body); err != nil {
		body = nil
	}
	s.lastBodies = append(s.lastBodies, body)
}

// lenResp builds a canned upstream response for a given model with the given
// text and completion-token count.
func lenResp(model, text string, completion int) *upstream.Response {
	return &upstream.Response{
		ID:    "resp-" + model + "-" + text,
		Model: model,
		Output: []upstream.OutputItem{{
			Type:    "message",
			Content: []upstream.ContentBlock{{Type: "output_text", Text: text}},
		}},
		Usage: upstream.Usage{PromptTokens: 100, CompletionTokens: completion, TotalTokens: 100 + completion},
	}
}

// autoServer returns a Server backed by segUpstream with auto-continuation
// configured via WithAutoContinue.
func autoServer(t *testing.T, stub upstreamClient, enabled bool, maxSeg int) *Server {
	t.Helper()
	return New(stub, minimalPool(t), "pw", twelveModels, WithAutoContinue(enabled, maxSeg))
}

// TestAutoContinueNonStreamTriggersOnce verifies §3.4 触发: a length response is
// replayed once, the continuation input carries the accumulated assistant text
// + the continue instruction, and the merged output/usage are returned with
// finish_reason=stop from the last segment.
func TestAutoContinueNonStreamTriggersOnce(t *testing.T) {
	stub := &segUpstream{responses: []*upstream.Response{
		lenResp("anthropic/claude-sonnet-5", "第一段。", 1024),
		lenResp("anthropic/claude-sonnet-5", "第二段。", 50),
	}}
	s := autoServer(t, stub, true, 5)
	body := `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"写一篇长文"}]}`
	rec := post(t, s, "/v1/chat/completions", "pw", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if stub.chatCalls != 2 {
		t.Errorf("chat calls = %d, want 2 (first + one continuation)", stub.chatCalls)
	}
	// The continuation payload must append assistant (accumulated text) + user
	// (continue instruction) to the original input, and drop previous_response_id.
	if len(stub.lastBodies) != 2 {
		t.Fatalf("recorded payloads = %d, want 2", len(stub.lastBodies))
	}
	origInput, _ := stub.lastBodies[0]["input"].([]interface{})
	contInput, _ := stub.lastBodies[1]["input"].([]interface{})
	if len(contInput) != len(origInput)+2 {
		t.Errorf("continuation input len = %d, want %d (+ assistant + user)", len(contInput), len(origInput)+2)
	}
	asst := contInput[len(contInput)-2].(map[string]interface{})
	if asst["role"] != "assistant" {
		t.Errorf("continuation input[%d] role = %v, want assistant", len(contInput)-2, asst["role"])
	}
	if got := asst["content"].([]interface{})[0].(map[string]interface{})["text"]; got != "第一段。" {
		t.Errorf("assistant accumulated text = %v, want 第一段。", got)
	}
	usr := contInput[len(contInput)-1].(map[string]interface{})
	if usr["role"] != "user" {
		t.Errorf("continuation input[%d] role = %v, want user", len(contInput)-1, usr["role"])
	}
	if got := usr["content"].([]interface{})[0].(map[string]interface{})["text"]; got != continueInstruction {
		t.Errorf("continue instruction = %v", got)
	}
	if _, ok := stub.lastBodies[1]["previous_response_id"]; ok {
		t.Errorf("continuation payload kept previous_response_id; want dropped")
	}
	// Model and other request params must be preserved across segments.
	if got := stub.lastBodies[1]["model"]; got != "anthropic/claude-sonnet-5" {
		t.Errorf("continuation model = %v, want anthropic/claude-sonnet-5", got)
	}
	if got := stub.lastBodies[1]["conversation_id"]; got != stub.lastBodies[0]["conversation_id"] {
		t.Errorf("continuation conversation_id changed: %v -> %v", stub.lastBodies[0]["conversation_id"], got)
	}

	var cc struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cc); err != nil {
		t.Fatal(err)
	}
	if cc.Choices[0].Message.Content != "第一段。第二段。" {
		t.Errorf("content = %q, want 第一段。第二段。", cc.Choices[0].Message.Content)
	}
	if cc.Choices[0].FinishReason == nil || *cc.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %v, want stop (from last segment)", cc.Choices[0].FinishReason)
	}
	// usage: prompt from first segment, completion summed.
	if cc.Usage.PromptTokens != 100 || cc.Usage.CompletionTokens != 1024+50 || cc.Usage.TotalTokens != 100+1024+50 {
		t.Errorf("usage = %+v, want prompt=100 completion=%d total=%d", cc.Usage, 1024+50, 100+1024+50)
	}
}

// TestAutoContinueNonStreamMaxSegments verifies §3.4 上限: consecutive length
// responses stop at the segment cap and the final finish_reason stays length.
func TestAutoContinueNonStreamMaxSegments(t *testing.T) {
	stub := &segUpstream{responses: []*upstream.Response{
		lenResp("anthropic/claude-sonnet-5", "段1。", 1024),
		lenResp("anthropic/claude-sonnet-5", "段2。", 1024),
		lenResp("anthropic/claude-sonnet-5", "段3。", 1024),
		lenResp("anthropic/claude-sonnet-5", "段4。", 1024),
	}}
	s := autoServer(t, stub, true, 3)
	body := `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"长文"}]}`
	rec := post(t, s, "/v1/chat/completions", "pw", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if stub.chatCalls != 3 {
		t.Errorf("chat calls = %d, want 3 (segment cap inclusive of first)", stub.chatCalls)
	}
	var cc struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cc); err != nil {
		t.Fatal(err)
	}
	if cc.Choices[0].Message.Content != "段1。段2。段3。" {
		t.Errorf("content = %q, want 段1。段2。段3。", cc.Choices[0].Message.Content)
	}
	if cc.Choices[0].FinishReason == nil || *cc.Choices[0].FinishReason != "length" {
		t.Errorf("finish_reason = %v, want length (still truncated at cap)", cc.Choices[0].FinishReason)
	}
	if cc.Usage.CompletionTokens != 1024*3 {
		t.Errorf("completion = %d, want %d", cc.Usage.CompletionTokens, 1024*3)
	}
}

// TestAutoContinueNonStreamStopNoContinue verifies §3.4 不触发: a natural stop
// response is returned as-is with a single upstream call.
func TestAutoContinueNonStreamStopNoContinue(t *testing.T) {
	stub := &segUpstream{responses: []*upstream.Response{
		lenResp("anthropic/claude-sonnet-5", "完整回答。", 42),
	}}
	s := autoServer(t, stub, true, 5)
	body := `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`
	rec := post(t, s, "/v1/chat/completions", "pw", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if stub.chatCalls != 1 {
		t.Errorf("chat calls = %d, want 1 (stop does not continue)", stub.chatCalls)
	}
	var cc struct {
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cc); err != nil {
		t.Fatal(err)
	}
	if cc.Choices[0].FinishReason == nil || *cc.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %v, want stop", cc.Choices[0].FinishReason)
	}
}

// TestAutoContinueNonStreamToolCallNoContinue verifies §3.4 不触发: a response
// carrying a function_call (even at the token cap) ends the turn and is NOT
// replayed.
func TestAutoContinueNonStreamToolCallNoContinue(t *testing.T) {
	stub := &segUpstream{responses: []*upstream.Response{{
		ID:    "resp-tool",
		Model: "anthropic/claude-sonnet-5",
		Output: []upstream.OutputItem{{
			Type:      "function_call",
			Name:      "get_weather",
			CallID:    "fc-13",
			Arguments: `{"city":"Beijing"}`,
		}},
		Usage: upstream.Usage{PromptTokens: 10, CompletionTokens: 1024, TotalTokens: 1034},
	}}}
	s := autoServer(t, stub, true, 5)
	body := `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"weather?"}]}`
	rec := post(t, s, "/v1/chat/completions", "pw", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if stub.chatCalls != 1 {
		t.Errorf("chat calls = %d, want 1 (tool_calls does not continue)", stub.chatCalls)
	}
	var cc struct {
		Choices []struct {
			Message struct {
				ToolCalls []convert.ToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cc); err != nil {
		t.Fatal(err)
	}
	if cc.Choices[0].FinishReason == nil || *cc.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %v, want tool_calls", cc.Choices[0].FinishReason)
	}
	if len(cc.Choices[0].Message.ToolCalls) != 1 {
		t.Errorf("tool calls = %d, want 1", len(cc.Choices[0].Message.ToolCalls))
	}
}

// TestAutoContinueNonStreamNoCapModel verifies §3.4 不触发: a model without a
// measured cap never infers length, so no replay happens even at a huge
// completion count.
func TestAutoContinueNonStreamNoCapModel(t *testing.T) {
	stub := &segUpstream{responses: []*upstream.Response{
		lenResp("grok/grok-4.5", "长输出。", 1230),
	}}
	s := autoServer(t, stub, true, 5)
	body := `{"model":"grok-4.5","messages":[{"role":"user","content":"hi"}]}`
	rec := post(t, s, "/v1/chat/completions", "pw", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if stub.chatCalls != 1 {
		t.Errorf("chat calls = %d, want 1 (no-cap model never continues)", stub.chatCalls)
	}
}

// TestAutoContinueDisabled verifies §3.4 配置: with AutoContinue=false the
// length-truncated response is returned after a single call, still marked
// finish_reason=length.
func TestAutoContinueDisabled(t *testing.T) {
	stub := &segUpstream{responses: []*upstream.Response{
		lenResp("anthropic/claude-sonnet-5", "被截断。", 1024),
	}}
	s := autoServer(t, stub, false, 5)
	body := `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`
	rec := post(t, s, "/v1/chat/completions", "pw", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if stub.chatCalls != 1 {
		t.Errorf("chat calls = %d, want 1 (auto-continue disabled)", stub.chatCalls)
	}
	var cc struct {
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cc); err != nil {
		t.Fatal(err)
	}
	if cc.Choices[0].FinishReason == nil || *cc.Choices[0].FinishReason != "length" {
		t.Errorf("finish_reason = %v, want length (still surfaced when disabled)", cc.Choices[0].FinishReason)
	}
}

// TestAutoContinueNonStreamContinuationFails verifies §3.1 失败兜底: when a
// continuation segment exhausts its retry budget (all calls fail), the
// accumulated content is still returned with finish_reason=length from the last
// successful segment — the partially-written answer is never discarded.
func TestAutoContinueNonStreamContinuationFails(t *testing.T) {
	stub := &segUpstream{
		responses: []*upstream.Response{
			lenResp("anthropic/claude-sonnet-5", "第一段。", 1024),
		},
		failFrom: 2, // first segment succeeds; every continuation attempt fails
		err:      &upstream.APIError{Kind: upstream.ErrUnknown, StatusCode: http.StatusServiceUnavailable, Message: "boom"},
	}
	s := autoServer(t, stub, true, 5)
	body := `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`
	rec := post(t, s, "/v1/chat/completions", "pw", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	// The continuation segment burned the full retry budget inside its fetch
	// (first call + MaxRetries-1 retries); the loop then stopped and returned
	// what was accumulated.
	if stub.chatCalls <= 1 {
		t.Errorf("chat calls = %d, want > 1 (a continuation attempt was made)", stub.chatCalls)
	}
	var cc struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cc); err != nil {
		t.Fatal(err)
	}
	if cc.Choices[0].Message.Content != "第一段。" {
		t.Errorf("content = %q, want 第一段。 (accumulated text kept)", cc.Choices[0].Message.Content)
	}
	if cc.Choices[0].FinishReason == nil || *cc.Choices[0].FinishReason != "length" {
		t.Errorf("finish_reason = %v, want length (last successful segment)", cc.Choices[0].FinishReason)
	}
}

// TestAutoContinueStream verifies the streaming path: a length response is
// replayed once, the SSE body carries both segments' text, the final finish
// chunk reports stop, and usage is accumulated (include_usage).
func TestAutoContinueStream(t *testing.T) {
	stub := &segUpstream{responses: []*upstream.Response{
		lenResp("anthropic/claude-sonnet-5", "流第一段。", 1024),
		lenResp("anthropic/claude-sonnet-5", "流第二段。", 60),
	}}
	s := autoServer(t, stub, true, 5)
	body := `{"model":"claude-sonnet-5","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`
	rec := post(t, s, "/v1/chat/completions", "pw", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if stub.streamCalls != 2 {
		t.Errorf("stream calls = %d, want 2", stub.streamCalls)
	}
	// The continuation stream payload appends assistant + user as well.
	if len(stub.lastBodies) != 2 {
		t.Fatalf("recorded payloads = %d, want 2", len(stub.lastBodies))
	}
	origInput, _ := stub.lastBodies[0]["input"].([]interface{})
	contInput, _ := stub.lastBodies[1]["input"].([]interface{})
	if len(contInput) != len(origInput)+2 {
		t.Errorf("continuation input len = %d, want %d", len(contInput), len(origInput)+2)
	}

	// Walk the SSE frames: collect all content deltas and the final finish + usage.
	var content strings.Builder
	var finish string
	var lastUsage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	}
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var frame struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &frame); err != nil {
			t.Fatalf("bad SSE frame %q: %v", payload, err)
		}
		if len(frame.Choices) > 0 {
			content.WriteString(frame.Choices[0].Delta.Content)
			if frame.Choices[0].FinishReason != nil {
				finish = *frame.Choices[0].FinishReason
			}
		}
		if frame.Usage != nil {
			lastUsage = frame.Usage
		}
	}
	if content.String() != "流第一段。流第二段。" {
		t.Errorf("stream content = %q, want 流第一段。流第二段。", content.String())
	}
	if finish != "stop" {
		t.Errorf("stream finish = %q, want stop", finish)
	}
	if lastUsage == nil {
		t.Fatal("final chunk missing usage")
	}
	if lastUsage.PromptTokens != 100 || lastUsage.CompletionTokens != 1024+60 || lastUsage.TotalTokens != 100+1024+60 {
		t.Errorf("stream usage = %+v, want prompt=100 completion=%d total=%d", lastUsage, 1024+60, 100+1024+60)
	}
}

package pool

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"anuma2api/gateway/internal/upstream"
)

// input builds an upload payload with the given email and credits.
func input(email string, credits int) AccountInput {
	return AccountInput{
		Email:            email,
		WalletAddress:    "0x1",
		WalletID:         "w1",
		UserID:           "u1",
		Tier:             "basic",
		IdentityToken:    "id.token." + email,
		AccessToken:      "acc.token." + email,
		RefreshToken:     "ref.token." + email,
		AvailableCredits: credits,
		ExpiresAt:        1785723984,
	}
}

// newTestPool builds a pool backed by a temp SQLite DB for tests. The
// background loops are not started.
func newTestPool(t *testing.T) *Pool {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	p, err := New(nil, dbPath, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

// fakeClient is a balance/refresh stand-in for the pool's health checks.
type fakeClient struct {
	balance    *upstream.Balance
	balErr     error
	balCalls   int
	refreshed  bool
	refreshErr error // optional error the fake RefreshToken returns
	idToken    string // optional identity token returned by RefreshToken; default is unparseable
}

func (f *fakeClient) GetBalance(ctx context.Context, identityToken string) (*upstream.Balance, error) {
	f.balCalls++
	return f.balance, f.balErr
}

func (f *fakeClient) RefreshToken(ctx context.Context, privyAccessToken, refreshToken string) (*upstream.RefreshResult, error) {
	f.refreshed = true
	if f.refreshErr != nil {
		return nil, f.refreshErr
	}
	id := f.idToken
	if id == "" {
		id = "id.after.refresh"
	}
	return &upstream.RefreshResult{IdentityToken: id}, nil
}

// injectFakeClient replaces the pool's upstream client for health-check tests.
func injectFakeClient(t *testing.T, p *Pool, f *fakeClient) {
	t.Helper()
	orig := p.client
	p.client = f
	t.Cleanup(func() { p.client = orig })
}

// TestUpsertNewAndUpdate verifies upload semantics (SPEC-upload §2.5): a new
// email is inserted, re-uploading the same email updates the existing row
// (token and credits refreshed) instead of inserting a duplicate.
func TestUpsertNewAndUpdate(t *testing.T) {
	p := newTestPool(t)

	added, updated, err := p.UpsertAccounts([]AccountInput{input("a@b.com", 100)})
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 || updated != 0 {
		t.Fatalf("first upsert added=%d updated=%d, want 1/0", added, updated)
	}
	if got := len(p.List()); got != 1 {
		t.Fatalf("accounts = %d, want 1", got)
	}

	in := input("a@b.com", 50)
	in.IdentityToken = "id.token.updated"
	added, updated, err = p.UpsertAccounts([]AccountInput{in})
	if err != nil {
		t.Fatal(err)
	}
	if added != 0 || updated != 1 {
		t.Fatalf("second upsert added=%d updated=%d, want 0/1", added, updated)
	}
	if got := len(p.List()); got != 1 {
		t.Fatalf("accounts after update = %d, want 1", got)
	}
	a := p.Get("a@b.com")
	if a == nil {
		t.Fatal("account missing after update")
	}
	if a.AvailableCredits != 50 || a.IdentityToken != "id.token.updated" {
		t.Errorf("account not fully updated: credits=%d token=%q", a.AvailableCredits, a.IdentityToken)
	}
	if a.Status != StatusActive {
		t.Errorf("status = %q, want active", a.Status)
	}
}

// TestUpsertBatch verifies a batch upload inserts all entries at once.
func TestUpsertBatch(t *testing.T) {
	p := newTestPool(t)
	added, _, err := p.UpsertAccounts([]AccountInput{
		input("a@b.com", 100),
		input("c@d.com", 50),
		input("e@f.com", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if added != 3 {
		t.Errorf("added = %d, want 3", added)
	}
	if got := len(p.List()); got != 3 {
		t.Errorf("accounts = %d, want 3", got)
	}
}

// TestUpsertSkipsEmpty verifies entries without email or identity_token are
// dropped rather than persisted.
func TestUpsertSkipsEmpty(t *testing.T) {
	p := newTestPool(t)
	added, _, err := p.UpsertAccounts([]AccountInput{
		input("a@b.com", 100),
		{Email: "no-token@x.com", AvailableCredits: 10},
		{IdentityToken: "id", AvailableCredits: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 {
		t.Errorf("added = %d, want 1 (only the valid entry)", added)
	}
	if p.Get("no-token@x.com") != nil {
		t.Errorf("entry without identity_token should not be stored")
	}
}

// TestDeletePhysical verifies DELETE physically removes the account: stats
// shrink and List no longer contains it (SPEC-upload §2.5).
func TestDeletePhysical(t *testing.T) {
	p := newTestPool(t)
	if _, _, err := p.UpsertAccounts([]AccountInput{input("a@b.com", 100)}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Delete("a@b.com"); err != nil {
		t.Fatal(err)
	}
	s := p.Stats()
	if s.TotalAccounts != 0 || s.Available != 0 {
		t.Errorf("stats after delete = %+v", s)
	}
	if got := len(p.List()); got != 0 {
		t.Errorf("List after delete = %d, want 0", got)
	}
	if p.Get("a@b.com") != nil {
		t.Errorf("Get after delete should be nil")
	}
}

// TestResurrectAfterDelete verifies re-uploading a deleted email brings the
// account back (SPEC-upload §2.1: no removed record is left behind).
func TestResurrectAfterDelete(t *testing.T) {
	p := newTestPool(t)
	if _, _, err := p.UpsertAccounts([]AccountInput{input("a@b.com", 100)}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Delete("a@b.com"); err != nil {
		t.Fatal(err)
	}
	added, _, err := p.UpsertAccounts([]AccountInput{input("a@b.com", 80)})
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 {
		t.Errorf("resurrect added = %d, want 1", added)
	}
	if got := len(p.List()); got != 1 {
		t.Errorf("accounts after resurrect = %d, want 1", got)
	}
	if a := p.Get("a@b.com"); a == nil || a.AvailableCredits != 80 {
		t.Errorf("resurrected account wrong: %+v", a)
	}
}

// TestHealthCheckNoCreditsCooldowns verifies the async health check parks a
// zero-balance account in cooldown instead of deleting it (SPEC-cooldown §2.2).
func TestHealthCheckNoCreditsCooldowns(t *testing.T) {
	p := newTestPool(t)
	if _, _, err := p.UpsertAccounts([]AccountInput{input("a@b.com", 100)}); err != nil {
		t.Fatal(err)
	}
	a := p.Get("a@b.com")
	injectFakeClient(t, p, &fakeClient{balance: &upstream.Balance{AvailableCredits: 0}})

	p.healthCheck(a)
	if p.Get("a@b.com") == nil {
		t.Fatalf("account with no credits should NOT be deleted (cooldown instead)")
	}
	a = p.Get("a@b.com")
	if a.Status != StatusCooldown {
		t.Errorf("status = %q, want cooldown", a.Status)
	}
	if a.lastError != "no credits" {
		t.Errorf("lastError = %q, want %q", a.lastError, "no credits")
	}
	if got := len(p.List()); got != 1 {
		t.Errorf("List after health-check cooldown = %d, want 1 (not deleted)", got)
	}
	if s := p.Stats(); s.Cooldown != 1 || s.Available != 0 {
		t.Errorf("stats after cooldown = %+v, want cooldown=1 available=0", s)
	}
}

// TestHealthCheckKeepsPositive verifies a positive balance keeps the account.
func TestHealthCheckKeepsPositive(t *testing.T) {
	p := newTestPool(t)
	if _, _, err := p.UpsertAccounts([]AccountInput{input("a@b.com", 10)}); err != nil {
		t.Fatal(err)
	}
	a := p.Get("a@b.com")
	injectFakeClient(t, p, &fakeClient{balance: &upstream.Balance{AvailableCredits: 99}})
	p.healthCheck(a)
	if p.Get("a@b.com") == nil {
		t.Errorf("account with positive balance should remain")
	}
	if p.Get("a@b.com").AvailableCredits != 99 {
		t.Errorf("credits = %d, want 99 (updated from balance)", p.Get("a@b.com").AvailableCredits)
	}
}

// TestAccountHealthAndStickyPick verifies rotation and stickiness.
func TestAccountHealthAndStickyPick(t *testing.T) {
	p := newTestPool(t)
	if _, _, err := p.UpsertAccounts([]AccountInput{input("a@b.com", 100), input("c@d.com", 100)}); err != nil {
		t.Fatal(err)
	}
	if len(p.List()) != 2 {
		t.Fatalf("accounts = %d", len(p.List()))
	}
	a1 := p.Pick("conv-abc")
	a2 := p.Pick("conv-abc")
	if a1 == nil || a1 != a2 {
		t.Errorf("sticky pick failed: %v vs %v", a1, a2)
	}
	a1.Disable("test")
	a3 := p.Pick("conv-abc")
	if a3 == nil || a3 == a1 {
		t.Errorf("expected a different account after disable, got %v", a3)
	}
}

// TestStatsAfterDisable verifies disabled accounts are counted but not removed.
func TestStatsAfterDisable(t *testing.T) {
	p := newTestPool(t)
	if _, _, err := p.UpsertAccounts([]AccountInput{input("a@b.com", 100), input("c@d.com", 50)}); err != nil {
		t.Fatal(err)
	}
	s := p.Stats()
	if s.TotalAccounts != 2 || s.Available != 2 || s.TotalCredits != 150 {
		t.Errorf("stats = %+v", s)
	}
	p.Get("a@b.com").Disable("x")
	s = p.Stats()
	if s.Available != 1 || s.Disabled != 1 || s.TotalCredits != 150 {
		t.Errorf("stats after disable = %+v", s)
	}
}

// TestListMaskedTokens verifies List returns every in-pool account with masked
// tokens.
func TestListMaskedTokens(t *testing.T) {
	p := newTestPool(t)
	if _, _, err := p.UpsertAccounts([]AccountInput{input("a@b.com", 100), input("c@d.com", 50)}); err != nil {
		t.Fatal(err)
	}
	got := p.List()
	if len(got) != 2 {
		t.Errorf("List len = %d, want 2", len(got))
	}
	for _, m := range got {
		if m.IdentityToken == "id.token."+m.Email {
			t.Errorf("masked view leaked full token for %s", m.Email)
		}
	}
}

// TestPersistSurvivesRestart verifies uploaded accounts survive a pool restart
// (SQLite load, SPEC-upload §2.3 startup path).
func TestPersistSurvivesRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	p, err := New(nil, dbPath, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.UpsertAccounts([]AccountInput{input("a@b.com", 100)}); err != nil {
		t.Fatal(err)
	}
	p.Close()

	p2, err := New(nil, dbPath, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	if got := len(p2.List()); got != 1 {
		t.Errorf("accounts after restart = %d, want 1", got)
	}
	if p2.Get("a@b.com").AvailableCredits != 100 {
		t.Errorf("credits after restart = %d, want 100", p2.Get("a@b.com").AvailableCredits)
	}
}

// TestPurgeLegacyRemoved verifies legacy status='removed' rows are purged on
// startup (SPEC-upload §2.2 migration path).
func TestPurgeLegacyRemoved(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	p, err := New(nil, dbPath, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.UpsertAccounts([]AccountInput{input("a@b.com", 100)}); err != nil {
		t.Fatal(err)
	}
	// Simulate a legacy removed row.
	if _, err := p.store.db.Exec(`UPDATE accounts SET status='removed' WHERE email='a@b.com'`); err != nil {
		t.Fatal(err)
	}
	p.Close()

	p2, err := New(nil, dbPath, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	if got := len(p2.List()); got != 0 {
		t.Errorf("accounts after purge = %d, want 0 (removed row deleted)", got)
	}
}

// ---- cooldown (SPEC-cooldown) ----

// TestEnterCooldownIdempotent verifies enterCooldown persists the state and is
// idempotent: calling it twice keeps the cooldown status.
func TestEnterCooldownIdempotent(t *testing.T) {
	p := newTestPool(t)
	if _, _, err := p.UpsertAccounts([]AccountInput{input("a@b.com", 100)}); err != nil {
		t.Fatal(err)
	}
	a := p.Get("a@b.com")
	a.enterCooldown("no credits")
	a.enterCooldown("no credits again")
	if a.Status != StatusCooldown {
		t.Errorf("status = %q, want cooldown", a.Status)
	}
	if a.lastError != "no credits again" {
		t.Errorf("lastError = %q, want the latest reason", a.lastError)
	}
	// Persisted: reload from the store keeps cooldown.
	rows, err := p.store.loadAccounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != StatusCooldown {
		t.Errorf("reloaded status = %q, want cooldown", rows[0].Status)
	}
}

// TestEnterCooldownSkipsManualDisabled verifies a manually disabled account is
// never moved to cooldown (the operator's decision wins).
func TestEnterCooldownSkipsManualDisabled(t *testing.T) {
	p := newTestPool(t)
	if _, _, err := p.UpsertAccounts([]AccountInput{input("a@b.com", 100)}); err != nil {
		t.Fatal(err)
	}
	a := p.Get("a@b.com")
	// Simulate a manually disabled account the way store.loadAccounts does.
	a.mu.Lock()
	a.Status = StatusDisabled
	a.lastError = manualDisabledReason
	a.manualDisabled = true
	a.mu.Unlock()
	a.enterCooldown("no credits")
	if a.Status != StatusDisabled {
		t.Errorf("status = %q, want disabled (manual wins)", a.Status)
	}
}

// TestCooldownRecoverRestoresActive verifies the cooldown round restores an
// account whose real balance is positive again (monthly replenishment).
func TestCooldownRecoverRestoresActive(t *testing.T) {
	p := newTestPool(t)
	if _, _, err := p.UpsertAccounts([]AccountInput{input("a@b.com", 100)}); err != nil {
		t.Fatal(err)
	}
	a := p.Get("a@b.com")
	a.enterCooldown("no credits")
	if s := p.Stats(); s.Cooldown != 1 {
		t.Fatalf("pre-check stats = %+v, want cooldown=1", s)
	}
	injectFakeClient(t, p, &fakeClient{balance: &upstream.Balance{AvailableCredits: 100}})

	st := p.CheckCooldowns(context.Background())
	if st.Restored != 1 || st.Still != 0 || st.Total != 1 {
		t.Errorf("cooldown round = %+v, want restored=1 still=0 total=1", st)
	}
	a = p.Get("a@b.com")
	if a.Status != StatusActive {
		t.Errorf("status = %q, want active after recovery", a.Status)
	}
	if a.AvailableCredits != 100 || a.lastError != "" {
		t.Errorf("credits=%d lastError=%q, want 100 / empty", a.AvailableCredits, a.lastError)
	}
	if s := p.Stats(); s.Cooldown != 0 || s.Available != 1 {
		t.Errorf("stats after recovery = %+v, want cooldown=0 available=1", s)
	}
}

// TestCooldownRoundKeepsZero verifies a still-zero balance keeps the account in
// cooldown (no state churn, no deletion).
func TestCooldownRoundKeepsZero(t *testing.T) {
	p := newTestPool(t)
	if _, _, err := p.UpsertAccounts([]AccountInput{input("a@b.com", 100)}); err != nil {
		t.Fatal(err)
	}
	a := p.Get("a@b.com")
	a.enterCooldown("no credits")
	injectFakeClient(t, p, &fakeClient{balance: &upstream.Balance{AvailableCredits: 0}})

	st := p.CheckCooldowns(context.Background())
	if st.Restored != 0 || st.Still != 1 {
		t.Errorf("cooldown round = %+v, want restored=0 still=1", st)
	}
	if a.Status != StatusCooldown {
		t.Errorf("status = %q, want cooldown", a.Status)
	}
}

// TestCooldownRoundSkipsActive verifies the round only touches cooldown
// accounts (an active account is neither checked nor counted).
func TestCooldownRoundSkipsActive(t *testing.T) {
	p := newTestPool(t)
	if _, _, err := p.UpsertAccounts([]AccountInput{input("a@b.com", 100)}); err != nil {
		t.Fatal(err)
	}
	injectFakeClient(t, p, &fakeClient{balance: &upstream.Balance{AvailableCredits: 50}})
	st := p.CheckCooldowns(context.Background())
	if st.Total != 0 || st.Restored != 0 {
		t.Errorf("cooldown round over active-only pool = %+v, want total=0", st)
	}
}

// TestPickSkipsCooldown verifies cooldown accounts are not selected for
// conversations (isHealthy checks status==active).
func TestPickSkipsCooldown(t *testing.T) {
	p := newTestPool(t)
	if _, _, err := p.UpsertAccounts([]AccountInput{input("a@b.com", 100)}); err != nil {
		t.Fatal(err)
	}
	a := p.Get("a@b.com")
	a.enterCooldown("no credits")
	if got := p.Pick("conv"); got != nil {
		t.Errorf("Pick selected a cooldown account: %v", got)
	}
}

// TestListByStatusFilter verifies GET /api/accounts?status=cooldown returns
// only cooldown accounts (SPEC-cooldown §2.5).
func TestListByStatusFilter(t *testing.T) {
	p := newTestPool(t)
	if _, _, err := p.UpsertAccounts([]AccountInput{
		input("a@b.com", 100), input("c@d.com", 0), input("e@f.com", 50),
	}); err != nil {
		t.Fatal(err)
	}
	// c@d.com uploaded with 0 credits is loaded as cooldown; e@f.com stays active.
	p.Get("e@f.com").Disable("test")

	all := p.ListByStatus("")
	if len(all) != 3 {
		t.Errorf("ListByStatus('') = %d, want 3", len(all))
	}
	cool := p.ListByStatus(StatusCooldown)
	if len(cool) != 1 || cool[0].Email != "c@d.com" {
		t.Errorf("ListByStatus(cooldown) = %+v, want just c@d.com", cool)
	}
	act := p.ListByStatus(StatusActive)
	if len(act) != 1 || act[0].Email != "a@b.com" {
		t.Errorf("ListByStatus(active) = %+v, want just a@b.com", act)
	}
}

// TestGetBalanceRetryOnce verifies a transient balance error (network / 5xx)
// is retried exactly once, and a success on retry is honored.
func TestGetBalanceRetryOnce(t *testing.T) {
	p := newTestPool(t)
	f := &fakeClient{balErr: &upstream.APIError{Kind: upstream.ErrUpstream, StatusCode: 503}}
	injectFakeClient(t, p, f)

	bal, err := p.getBalanceWithRetry(context.Background(), "id")
	if err == nil {
		t.Fatalf("expected error after both attempts, got balance %+v", bal)
	}
	if f.balCalls != 2 {
		t.Errorf("GetBalance calls = %d, want 2 (original + 1 retry)", f.balCalls)
	}
}

// statefulBalance is a balance fake that fails the first N calls then returns a
// balance, for retry-once tests.
type statefulBalance struct {
	failFirst int
	calls     int
	bal       *upstream.Balance
	err       error
}

func (s *statefulBalance) GetBalance(ctx context.Context, identityToken string) (*upstream.Balance, error) {
	s.calls++
	if s.calls <= s.failFirst {
		return nil, s.err
	}
	return s.bal, nil
}

func (s *statefulBalance) RefreshToken(ctx context.Context, privyAccessToken, refreshToken string) (*upstream.RefreshResult, error) {
	return &upstream.RefreshResult{IdentityToken: "id.after.refresh"}, nil
}

// TestGetBalanceRetryRecoversStateful verifies a transient failure followed by
// a successful retry returns the balance (SPEC-cooldown §2.4).
func TestGetBalanceRetryRecoversStateful(t *testing.T) {
	p := newTestPool(t)
	f := &statefulBalance{
		failFirst: 1,
		bal:       &upstream.Balance{AvailableCredits: 7},
		err:       &upstream.APIError{Kind: upstream.ErrUpstream, StatusCode: 503},
	}
	orig := p.client
	p.client = f
	t.Cleanup(func() { p.client = orig })

	bal, err := p.getBalanceWithRetry(context.Background(), "id")
	if err != nil {
		t.Fatalf("expected success on retry, got %v", err)
	}
	if bal.AvailableCredits != 7 {
		t.Errorf("credits = %d, want 7", bal.AvailableCredits)
	}
	if f.calls != 2 {
		t.Errorf("GetBalance calls = %d, want 2", f.calls)
	}
}

// TestHealthCheckQueryFailureCooldowns verifies a persistent balance-query
// failure after refresh parks the account in cooldown rather than deleting it
// (SPEC-cooldown §2.2).
func TestHealthCheckQueryFailureCooldowns(t *testing.T) {
	p := newTestPool(t)
	if _, _, err := p.UpsertAccounts([]AccountInput{input("a@b.com", 100)}); err != nil {
		t.Fatal(err)
	}
	a := p.Get("a@b.com")
	injectFakeClient(t, p, &fakeClient{balErr: &upstream.APIError{Kind: upstream.ErrUpstream, StatusCode: 503}})

	p.healthCheck(a)
	if p.Get("a@b.com") == nil {
		t.Fatalf("account should NOT be deleted on balance-query failure")
	}
	if p.Get("a@b.com").Status != StatusCooldown {
		t.Errorf("status = %q, want cooldown", p.Get("a@b.com").Status)
	}
}

// ---- refresh round (SPEC refresh-loop) ----

// fakeJWT builds a minimal parseable JWT carrying the given exp claim, so the
// real refresh path can decode a fresh expiry from it.
func fakeJWT(exp int64) string {
	payload, _ := json.Marshal(map[string]int64{"exp": exp})
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

// TestRefreshUpdatesExpiresAt verifies that a successful privy refresh also
// refreshes the persisted expires_at to the fresh identity token's exp claim,
// instead of leaving the stale upload-time value that made every account look
// expired. An unparseable refreshed token must not clobber a known expiry.
func TestRefreshUpdatesExpiresAt(t *testing.T) {
	p := newTestPool(t)
	if _, _, err := p.UpsertAccounts([]AccountInput{input("a@b.com", 100)}); err != nil {
		t.Fatal(err)
	}
	a := p.Get("a@b.com")
	if a.ExpiresAt == 0 {
		t.Fatal("fixture should carry an expires_at")
	}

	future := time.Now().Add(90 * time.Minute).Unix()
	tok := fakeJWT(future)
	injectFakeClient(t, p, &fakeClient{idToken: tok})
	if err := p.refresh(context.Background(), a); err != nil {
		t.Fatalf("refresh (parseable token): %v", err)
	}
	if got := a.ExpiresAt; got != future {
		t.Errorf("expires_at after refresh = %d, want %d (from fresh JWT exp)", got, future)
	}

	// A refreshed token that cannot be parsed (fakeClient default) must keep
	// the previous expiry rather than zeroing it.
	injectFakeClient(t, p, &fakeClient{})
	if err := p.refresh(context.Background(), a); err != nil {
		t.Fatalf("refresh (unparseable token): %v", err)
	}
	if got := a.ExpiresAt; got != future {
		t.Errorf("expires_at clobbered by unparseable token = %d, want %d", got, future)
	}
}

// injectFakeRefresher replaces the pool's per-account refresh with a fake
// returning ok for the whitelisted emails and err for everyone else, then
// restores the production implementation on cleanup. A short per-account delay
// keeps the round fast.
func injectFakeRefresher(t *testing.T, p *Pool, okEmails map[string]bool) {
	t.Helper()
	old := refreshAccountDelay
	refreshAccountDelay = time.Millisecond
	t.Cleanup(func() { refreshAccountDelay = old })
	orig := p.refresher
	p.refresher = func(ctx context.Context, a *Account) error {
		if okEmails[a.Email] {
			return nil
		}
		return errors.New("fake refresh failure")
	}
	t.Cleanup(func() { p.refresher = orig })
}

// TestRunRefreshRoundIncludesCooldown verifies P0-3 (user-required): the
// refresh round targets cooldown accounts too, so their tokens stay fresh and
// they are immediately usable once the cooldown patrol restores them. A
// cooldown account in the pool must be refreshed and counted in the round.
func TestRunRefreshRoundIncludesCooldown(t *testing.T) {
	p := newTestPool(t)
	if _, _, err := p.UpsertAccounts([]AccountInput{
		input("a@b.com", 100), input("b@b.com", 0),
	}); err != nil {
		t.Fatal(err)
	}
	// b@b.com was uploaded with 0 credits -> cooldown; a@b.com stays active.
	b := p.Get("b@b.com")
	if b.Status != StatusCooldown {
		t.Fatalf("b@b.com status = %q, want cooldown", b.Status)
	}
	injectFakeRefresher(t, p, map[string]bool{"a@b.com": true, "b@b.com": true})

	st := p.runRefreshRound(context.Background())
	if st.OK != 2 || st.Fail != 0 {
		t.Errorf("refresh round = %+v, want ok=2 fail=0 (active + cooldown)", st)
	}
}

// TestCooldownRefreshFailureNoEscalation verifies P0-3: a refresh failure on a
// cooldown account must NOT increment consecutiveAuthFailures (which would push
// it toward the disable limit once restored). The account stays in cooldown and
// its auth-failure counter is untouched.
func TestCooldownRefreshFailureNoEscalation(t *testing.T) {
	p := newTestPool(t)
	if _, _, err := p.UpsertAccounts([]AccountInput{
		input("a@b.com", 100), input("b@b.com", 0),
	}); err != nil {
		t.Fatal(err)
	}
	a := p.Get("a@b.com")
	b := p.Get("b@b.com")
	if b.Status != StatusCooldown {
		t.Fatalf("b@b.com status = %q, want cooldown", b.Status)
	}

	// a fake refresher that fails for the cooldown account and succeeds for the
	// active one.
	orig := p.refresher
	p.refresher = func(ctx context.Context, acc *Account) error {
		if acc == b {
			return errors.New("fake refresh failure on cooldown")
		}
		return nil
	}
	t.Cleanup(func() { p.refresher = orig })

	st := p.runRefreshRound(context.Background())
	if st.OK != 1 || st.Fail != 1 {
		t.Errorf("refresh round = %+v, want ok=1 fail=1", st)
	}
	if b.consecutiveAuthFailures != 0 {
		t.Errorf("cooldown refresh failure incremented consecutiveAuthFailures: %d", b.consecutiveAuthFailures)
	}
	if b.Status != StatusCooldown {
		t.Errorf("b@b.com status = %q, want cooldown (not disabled)", b.Status)
	}
	if a.consecutiveAuthFailures != 0 {
		t.Errorf("active account auth failures = %d, want 0", a.consecutiveAuthFailures)
	}
}
func TestRunRefreshRoundCounts(t *testing.T) {
	p := newTestPool(t)
	if _, _, err := p.UpsertAccounts([]AccountInput{
		input("a@b.com", 100), input("b@b.com", 100), input("c@b.com", 100),
	}); err != nil {
		t.Fatal(err)
	}
	// c@b.com is disabled before the round; it must not be refreshed at all.
	p.Get("c@b.com").Disable("test")
	injectFakeRefresher(t, p, map[string]bool{"a@b.com": true, "b@b.com": true})

	st := p.runRefreshRound(context.Background())
	if st.OK != 2 {
		t.Errorf("ok = %d, want 2", st.OK)
	}
	if st.Fail != 0 {
		t.Errorf("fail = %d, want 0", st.Fail)
	}
}

// TestRunRefreshRoundFailures verifies failing refreshes are counted and, when
// the failure is a real 401 (auth-level), the account is queued for a health
// check (SPEC refresh-loop §3.1, P0-B3).
func TestRunRefreshRoundFailures(t *testing.T) {
	p := newTestPool(t)
	if _, _, err := p.UpsertAccounts([]AccountInput{
		input("a@b.com", 100), input("b@b.com", 100),
	}); err != nil {
		t.Fatal(err)
	}
	// b@b.com's refresh returns a real 401 (auth-level) so it must be queued.
	orig := p.refresher
	p.refresher = func(ctx context.Context, a *Account) error {
		if a.Email == "b@b.com" {
			return &upstream.APIError{Kind: upstream.ErrUnauthorized, StatusCode: 401, Message: "invalid refresh token"}
		}
		return nil
	}
	t.Cleanup(func() { p.refresher = orig })

	st := p.runRefreshRound(context.Background())
	if st.OK != 1 {
		t.Errorf("ok = %d, want 1", st.OK)
	}
	if st.Fail != 1 {
		t.Errorf("fail = %d, want 1", st.Fail)
	}
	// A real-401 failed account must be queued for an async health check.
	select {
	case a := <-p.healthCh:
		if a.Email != "b@b.com" {
			t.Errorf("health-check queue got %s, want b@b.com", a.Email)
		}
	default:
		t.Errorf("401 failed account was not queued for a health check")
	}
}

// TestRunRefreshRoundTransientFailureNoQueue verifies P0-B3: a transient
// refresh failure (5xx / network / timeout) must NOT enqueue a health check —
// a health check with a still-valid token against a momentarily-broken portal
// returns 401 → enterCooldown → the 08 cooldown avalanche. Only real 401/403
// failures queue. Three transient kinds are covered: 5xx, network, timeout.
func TestRunRefreshRoundTransientFailureNoQueue(t *testing.T) {
	transientErrs := []struct {
		name string
		err  error
	}{
		{"5xx", &upstream.APIError{Kind: upstream.ErrUpstream, StatusCode: 503, Message: "Unable to process request"}},
		{"network", errors.New("dial tcp: connection reset by peer")},
		{"timeout", context.DeadlineExceeded},
	}
	for _, te := range transientErrs {
		t.Run(te.name, func(t *testing.T) {
			p := newTestPool(t)
			if _, _, err := p.UpsertAccounts([]AccountInput{
				input("a@b.com", 100), input("b@b.com", 100),
			}); err != nil {
				t.Fatal(err)
			}
			orig := p.refresher
			p.refresher = func(ctx context.Context, a *Account) error {
				if a.Email == "b@b.com" {
					return te.err
				}
				return nil
			}
			t.Cleanup(func() { p.refresher = orig })

			st := p.runRefreshRound(context.Background())
			if st.OK != 1 || st.Fail != 1 {
				t.Fatalf("refresh round = %+v, want ok=1 fail=1", st)
			}
			// A transient failure must NOT queue a health check (P0-B3).
			if n := len(p.healthCh); n != 0 {
				t.Errorf("health-check queue has %d accounts, want 0 (transient must not queue)", n)
			}
			// The transiently-failed account must not have its auth-failure
			// counter escalated (P0-B1) — it stays active.
			b := p.Get("b@b.com")
			if b.consecutiveAuthFailures != 0 {
				t.Errorf("transient failure incremented consecutiveAuthFailures: %d", b.consecutiveAuthFailures)
			}
			if b.Status != StatusActive {
				t.Errorf("b@b.com status = %q, want active (transient must not disable)", b.Status)
			}
		})
	}
}

// TestRunRefreshRoundRateLimitedSeparate verifies SPEC-ratelimit-fix P0-2/P0-3:
// Privy 429s are counted in RateLimited (not Fail) and never queue a health
// check — a rate-limited account is healthy, only throttled, and a health check
// would clear the lastError trace it just recorded. The 1s backoff is retained.
func TestRunRefreshRoundRateLimitedSeparate(t *testing.T) {
	p := newTestPool(t)
	if _, _, err := p.UpsertAccounts([]AccountInput{
		input("a@b.com", 100), input("b@b.com", 100), input("c@b.com", 100),
	}); err != nil {
		t.Fatal(err)
	}

	orig := refreshAccountDelay
	refreshAccountDelay = time.Millisecond
	t.Cleanup(func() { refreshAccountDelay = orig })
	origBackoff := refreshRateLimitBackoff
	refreshRateLimitBackoff = time.Millisecond
	t.Cleanup(func() { refreshRateLimitBackoff = origBackoff })

	// Every refresh is rate-limited (Privy 429).
	origRefresher := p.refresher
	p.refresher = func(ctx context.Context, a *Account) error {
		return &upstream.APIError{Kind: upstream.ErrRateLimited, StatusCode: 429, Message: "too many requests"}
	}
	t.Cleanup(func() { p.refresher = origRefresher })

	st := p.runRefreshRound(context.Background())
	if st.OK != 0 || st.Fail != 0 || st.RateLimited != 3 {
		t.Errorf("refresh round = %+v, want ok=0 fail=0 rate_limited=3", st)
	}
	// A 429 must not queue the account for an async health check.
	if n := len(p.healthCh); n != 0 {
		t.Errorf("health-check queue has %d accounts, want 0 (429 must not queue)", n)
	}
}

// TestRefreshAllBusy verifies the concurrency guard: while one round runs, a
// second RefreshAll returns Busy instead of starting another round (SPEC
// refresh-loop §3.3).
func TestRefreshAllBusy(t *testing.T) {
	p := newTestPool(t)
	if _, _, err := p.UpsertAccounts([]AccountInput{input("a@b.com", 100)}); err != nil {
		t.Fatal(err)
	}
	injectFakeRefresher(t, p, map[string]bool{"a@b.com": true})

	entered := make(chan struct{})
	release := make(chan struct{})
	orig := p.refresher
	p.refresher = func(ctx context.Context, a *Account) error {
		close(entered)
		<-release
		return nil
	}
	t.Cleanup(func() { p.refresher = orig })

	done := make(chan RefreshStats, 1)
	go func() {
		done <- p.RefreshAll(context.Background())
	}()
	<-entered // first round is now in progress

	st := p.RefreshAll(context.Background())
	if !st.Busy {
		t.Errorf("second RefreshAll = %+v, want Busy=true", st)
	}
	close(release)
	first := <-done
	if first.Busy || first.OK != 1 {
		t.Errorf("first RefreshAll = %+v, want ok=1", first)
	}
}

// TestRecordSuccessClearsFailCount verifies P1-3: recordSuccess zeroes
// failCount so the metric reflects recent health instead of accumulating
// forever. A failure then a success must leave failCount at 0.
func TestRecordSuccessClearsFailCount(t *testing.T) {
	p := newTestPool(t)
	if _, _, err := p.UpsertAccounts([]AccountInput{input("a@b.com", 100)}); err != nil {
		t.Fatal(err)
	}
	a := p.Get("a@b.com")
	a.recordFailure(upstream.ErrUnknown, "boom")
	if a.failCount == 0 {
		t.Fatalf("precondition: failCount = %d, want >0 after a failure", a.failCount)
	}
	a.recordSuccess(1)
	if a.failCount != 0 {
		t.Errorf("failCount after success = %d, want 0", a.failCount)
	}
	// Persisted too: reloading from the store must show the cleared count.
	rows, err := p.store.loadAccounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].failCount != 0 {
		t.Errorf("persisted failCount = %d, want 0", rows[0].failCount)
	}
}

// TestRecordSuccessDrainsToCooldown verifies P1-1: a successful call that
// exhausts the last credit parks the account in cooldown instead of leaving it
// stranded in active/0, so the monthly patrol can restore it.
func TestRecordSuccessDrainsToCooldown(t *testing.T) {
	p := newTestPool(t)
	if _, _, err := p.UpsertAccounts([]AccountInput{input("a@b.com", 5)}); err != nil {
		t.Fatal(err)
	}
	a := p.Get("a@b.com")
	a.recordSuccess(5) // drain exactly to 0
	if a.Status != StatusCooldown {
		t.Errorf("status = %q, want cooldown after draining to 0", a.Status)
	}
	if a.AvailableCredits != 0 {
		t.Errorf("credits = %d, want 0", a.AvailableCredits)
	}
	if a.lastError != "no credits" {
		t.Errorf("lastError = %q, want %q", a.lastError, "no credits")
	}
	// Persisted too.
	rows, err := p.store.loadAccounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != StatusCooldown {
		t.Errorf("persisted status = %q, want cooldown", rows[0].Status)
	}
}

// TestRecordSuccessDoesNotResurrectDisabled verifies P2-6: a stale success
// reply on an account that is no longer active (disabled / cooldown) must not
// flip it back to active.
func TestRecordSuccessDoesNotResurrectDisabled(t *testing.T) {
	p := newTestPool(t)
	if _, _, err := p.UpsertAccounts([]AccountInput{input("a@b.com", 100)}); err != nil {
		t.Fatal(err)
	}
	a := p.Get("a@b.com")
	a.enterCooldown("no credits")
	a.recordSuccess(1)
	if a.Status != StatusCooldown {
		t.Errorf("status = %q, want cooldown (stale success must not resurrect)", a.Status)
	}
	a.disable("admin")
	a.recordSuccess(1)
	if a.Status != StatusDisabled {
		t.Errorf("status = %q, want disabled (stale success must not resurrect)", a.Status)
	}
}

// concurrentRefreshClient is a fake balanceRefresher that blocks each refresh
// call on a channel so a test can hold two refreshes in flight on the same
// account simultaneously.
type concurrentRefreshClient struct {
	entered chan struct{}
	release chan struct{}
	calls   int
}

func (c *concurrentRefreshClient) GetBalance(ctx context.Context, identityToken string) (*upstream.Balance, error) {
	return &upstream.Balance{AvailableCredits: 100}, nil
}

func (c *concurrentRefreshClient) RefreshToken(ctx context.Context, privyAccessToken, refreshToken string) (*upstream.RefreshResult, error) {
	c.calls++
	c.entered <- struct{}{}
	<-c.release
	return &upstream.RefreshResult{IdentityToken: "id.new", PrivyAccessToken: "acc.new", RefreshToken: "ref.new"}, nil
}

// TestRefreshSerializedPerAccount verifies P1-4: concurrent refreshes of the
// same account are serialized by the per-account refreshMu, so both succeed
// instead of racing the token rotation.
func TestRefreshSerializedPerAccount(t *testing.T) {
	p := newTestPool(t)
	if _, _, err := p.UpsertAccounts([]AccountInput{input("a@b.com", 100)}); err != nil {
		t.Fatal(err)
	}
	a := p.Get("a@b.com")
	f := &concurrentRefreshClient{entered: make(chan struct{}, 2), release: make(chan struct{})}
	orig := p.client
	p.client = f
	t.Cleanup(func() { p.client = orig })

	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			done <- p.refresh(context.Background(), a)
		}()
	}
	// Both goroutines start; the second must block on refreshMu until the first
	// completes (only one call can be in flight at a time).
	<-f.entered
	select {
	case <-f.entered:
		t.Fatalf("second refresh entered while the first was still in flight (not serialized)")
	case <-time.After(50 * time.Millisecond):
	}
	close(f.release)
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Errorf("refresh %d failed: %v", i, err)
		}
	}
	if f.calls != 2 {
		t.Errorf("refresh calls = %d, want 2", f.calls)
	}
}

// TestStatsTodayCallsCountsCalls verifies D6: today_calls counts successful
// calls (per recordSuccess), not distinct accounts used today. Two calls on the
// same account must report 2.
func TestStatsTodayCallsCountsCalls(t *testing.T) {
	p := newTestPool(t)
	if _, _, err := p.UpsertAccounts([]AccountInput{input("a@b.com", 100)}); err != nil {
		t.Fatal(err)
	}
	a := p.Get("a@b.com")
	a.recordSuccess(1)
	a.recordSuccess(1)
	s := p.Stats()
	if s.TodayCalls != 2 {
		t.Errorf("today_calls = %d, want 2 (two calls on one account)", s.TodayCalls)
	}
}

// TestPickSkipsRecentlyFailed verifies P0-2: an account that just failed is
// excluded from Pick (both the sticky slot and the LRU fallback) until the
// pickCooldown window passes.
func TestPickSkipsRecentlyFailed(t *testing.T) {
	p := newTestPool(t)
	if _, _, err := p.UpsertAccounts([]AccountInput{
		input("a@b.com", 100), input("c@d.com", 100),
	}); err != nil {
		t.Fatal(err)
	}
	a := p.Get("a@b.com")
	// a@b.com fails; it enters the short-term cooldown.
	a.recordFailure(upstream.ErrUnknown, "boom")
	// a@b.com must not be picked, even by its sticky hash.
	for i := 0; i < 20; i++ {
		if got := p.Pick(""); got != nil && got == a {
			t.Fatalf("Pick returned the just-failed account %s", got.Email)
		}
	}
}

// TestPickFallbackWhenAllCoolingDown verifies P0-2: when every healthy account
// is inside the short-term failure cooldown, Pick falls back to the plain LRU
// selection instead of returning nil.
func TestPickFallbackWhenAllCoolingDown(t *testing.T) {
	p := newTestPool(t)
	if _, _, err := p.UpsertAccounts([]AccountInput{
		input("a@b.com", 100), input("c@d.com", 100),
	}); err != nil {
		t.Fatal(err)
	}
	a := p.Get("a@b.com")
	c := p.Get("c@d.com")
	// Both accounts just failed.
	a.recordFailure(upstream.ErrUnknown, "boom")
	c.recordFailure(upstream.ErrUnknown, "boom")

	got := p.Pick("")
	if got == nil {
		t.Fatalf("Pick returned nil when all accounts are cooling down, want LRU fallback")
	}
	if got != a && got != c {
		t.Errorf("Pick returned an unexpected account: %v", got)
	}
}

// TestPickClearsCooldownOnSuccess verifies P0-2: recordSuccess clears the
// short-term failure cooldown, so the account becomes pickable again even
// before the 30s window elapses.
func TestPickClearsCooldownOnSuccess(t *testing.T) {
	p := newTestPool(t)
	if _, _, err := p.UpsertAccounts([]AccountInput{
		input("a@b.com", 100), input("c@d.com", 100),
	}); err != nil {
		t.Fatal(err)
	}
	a := p.Get("a@b.com")
	c := p.Get("c@d.com")
	a.recordFailure(upstream.ErrUnknown, "boom")
	c.recordFailure(upstream.ErrUnknown, "boom")
	// c@d.com succeeds immediately, clearing its cooldown.
	c.recordSuccess(1)

	// Only c@d.com should be pickable now (a@b.com is still cooling down).
	for i := 0; i < 20; i++ {
		if got := p.Pick(""); got == nil || got == a {
			t.Fatalf("Pick = %v, want c@d.com (a@b.com still cooling down)", got)
		}
	}
}

// TestRecordFailureClearsCooldownOnDelete verifies the cooldown entry is removed
// when the account is physically deleted (hygiene: recentFails must not grow
// unbounded with deleted accounts).
func TestRecordFailureClearsCooldownOnDelete(t *testing.T) {
	p := newTestPool(t)
	if _, _, err := p.UpsertAccounts([]AccountInput{input("a@b.com", 100)}); err != nil {
		t.Fatal(err)
	}
	a := p.Get("a@b.com")
	a.recordFailure(upstream.ErrUnknown, "boom")
	p.RemoveAccount(a, "test delete")

	p.mu.Lock()
	_, ok := p.recentFails["a@b.com"]
	p.mu.Unlock()
	if ok {
		t.Errorf("recentFails still contains a deleted account")
	}
}

// ---- concurrent refresh round (SPEC refresh-loop §2.1) ----

// TestRunRefreshRoundConcurrencyBounded verifies the worker pool never exceeds
// refreshRoundWorkers in-flight refreshes, and that every account is refreshed
// exactly once (no duplicates).
func TestRunRefreshRoundConcurrencyBounded(t *testing.T) {
	p := newTestPool(t)
	const n = 24 // more than refreshRoundWorkers so the pool is saturated
	inputs := make([]AccountInput, 0, n)
	for i := 0; i < n; i++ {
		inputs = append(inputs, input(fmt.Sprintf("u%02d@b.com", i), 100))
	}
	if _, _, err := p.UpsertAccounts(inputs); err != nil {
		t.Fatal(err)
	}

	orig := refreshAccountDelay
	refreshAccountDelay = time.Millisecond
	t.Cleanup(func() { refreshAccountDelay = orig })
	origBackoff := refreshRateLimitBackoff
	refreshRateLimitBackoff = time.Millisecond
	t.Cleanup(func() { refreshRateLimitBackoff = origBackoff })

	var inFlight, peak int32
	var mu sync.Mutex
	refreshed := make(map[string]int)
	origRefresher := p.refresher
	p.refresher = func(ctx context.Context, a *Account) error {
		cur := atomic.AddInt32(&inFlight, 1)
		defer atomic.AddInt32(&inFlight, -1)
		for {
			old := atomic.LoadInt32(&peak)
			if cur <= old || atomic.CompareAndSwapInt32(&peak, old, cur) {
				break
			}
		}
		mu.Lock()
		refreshed[a.Email]++
		mu.Unlock()
		time.Sleep(time.Millisecond) // widen the in-flight window
		return nil
	}
	t.Cleanup(func() { p.refresher = origRefresher })

	st := p.runRefreshRound(context.Background())
	if st.OK != n || st.Fail != 0 {
		t.Fatalf("refresh round = %+v, want ok=%d fail=0", st, n)
	}
	if got := atomic.LoadInt32(&peak); int(got) > refreshRoundWorkers {
		t.Errorf("peak in-flight refreshes = %d, want <= %d", got, refreshRoundWorkers)
	}
	if len(refreshed) != n {
		t.Errorf("refreshed %d distinct accounts, want %d (all of them)", len(refreshed), n)
	}
	for email, c := range refreshed {
		if c != 1 {
			t.Errorf("account %s refreshed %d times, want exactly 1", email, c)
		}
	}
}

// TestRunRefreshRoundZeroAccounts verifies an empty pool returns an empty
// RefreshStats without logging a round (early exit, SPEC §3.4).
func TestRunRefreshRoundZeroAccounts(t *testing.T) {
	p := newTestPool(t)
	st := p.runRefreshRound(context.Background())
	if st.OK != 0 || st.Fail != 0 || st.Busy {
		t.Errorf("refresh round over empty pool = %+v, want ok=0 fail=0", st)
	}
}

// TestRunRefreshRoundFewAccounts verifies a pool smaller than refreshRoundWorkers
// is fully processed with no deadlock or duplicates (SPEC §3.4).
func TestRunRefreshRoundFewAccounts(t *testing.T) {
	p := newTestPool(t)
	inputs := []AccountInput{input("a@b.com", 100), input("b@b.com", 100), input("c@b.com", 100)}
	if _, _, err := p.UpsertAccounts(inputs); err != nil {
		t.Fatal(err)
	}
	injectFakeRefresher(t, p, map[string]bool{"a@b.com": true, "b@b.com": true, "c@b.com": true})

	st := p.runRefreshRound(context.Background())
	if st.OK != 3 || st.Fail != 0 {
		t.Errorf("refresh round over 3 accounts = %+v, want ok=3 fail=0", st)
	}
}

// captureLog redirects the global logger into a buffer for the duration of the
// test, so cancellation/shutdown log lines can be asserted.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(orig) })
	return &buf
}

// TestRunRefreshRoundTimeoutAborts verifies round cancellation: a pre-cancelled
// context aborts with 0/0, and a mid-round cancel stops issuing new refreshes
// while keeping the partial counts. Both paths log "aborting" (SPEC §3.4).
func TestRunRefreshRoundTimeoutAborts(t *testing.T) {
	p := newTestPool(t)
	inputs := []AccountInput{
		input("a@b.com", 100), input("b@b.com", 100), input("c@b.com", 100),
		input("d@b.com", 100), input("e@b.com", 100), input("f@b.com", 100),
	}
	if _, _, err := p.UpsertAccounts(inputs); err != nil {
		t.Fatal(err)
	}
	orig := refreshAccountDelay
	refreshAccountDelay = time.Millisecond
	t.Cleanup(func() { refreshAccountDelay = orig })

	// Pre-cancelled context: nothing is refreshed, 0/0, aborting log.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	buf := captureLog(t)
	st := p.runRefreshRound(ctx)
	if st.OK != 0 || st.Fail != 0 {
		t.Errorf("pre-cancelled round = %+v, want ok=0 fail=0", st)
	}
	if !strings.Contains(buf.String(), "aborting") {
		t.Errorf("pre-cancelled round log = %q, want aborting", buf.String())
	}

	// Mid-round cancel: the refresher cancels the context on its first call;
	// the round keeps the completed counts and stops issuing new refreshes.
	ctx, cancel = context.WithCancel(context.Background())
	var mu sync.Mutex
	cancelled := false
	calls := 0
	origRefresher := p.refresher
	p.refresher = func(c context.Context, a *Account) error {
		mu.Lock()
		calls++
		first := !cancelled
		cancelled = true
		mu.Unlock()
		if first {
			cancel()
		}
		return nil
	}
	t.Cleanup(func() { p.refresher = origRefresher })

	buf.Reset()
	st = p.runRefreshRound(ctx)
	if st.OK+st.Fail == 0 {
		t.Errorf("mid-cancel round = %+v, want at least one completed refresh", st)
	}
	if st.OK+st.Fail >= len(inputs) {
		t.Errorf("mid-cancel round = %+v, want fewer than %d completed (should abort)", st, len(inputs))
	}
	if !strings.Contains(buf.String(), "aborting") {
		t.Errorf("mid-cancel round log = %q, want aborting", buf.String())
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != st.OK+st.Fail {
		t.Errorf("refresher calls = %d, want %d (every call counted)", got, st.OK+st.Fail)
	}
}

// TestRunRefreshRoundStopChAborts verifies shutdown: closing the pool's stopCh
// makes an in-flight round return promptly (no new refreshes, does not block
// Close), while counting the refresh that was already running.
func TestRunRefreshRoundStopChAborts(t *testing.T) {
	p := newTestPool(t)
	const n = 10
	inputs := make([]AccountInput, 0, n)
	for i := 0; i < n; i++ {
		inputs = append(inputs, input(fmt.Sprintf("s%02d@b.com", i), 100))
	}
	if _, _, err := p.UpsertAccounts(inputs); err != nil {
		t.Fatal(err)
	}
	orig := refreshAccountDelay
	refreshAccountDelay = time.Millisecond
	t.Cleanup(func() { refreshAccountDelay = orig })

	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var calls int32
	origRefresher := p.refresher
	p.refresher = func(ctx context.Context, a *Account) error {
		atomic.AddInt32(&calls, 1)
		select {
		case started <- struct{}{}:
		default:
		}
		<-release // hold the in-flight refresh until shutdown is signalled
		return nil
	}
	t.Cleanup(func() { p.refresher = origRefresher })

	done := make(chan RefreshStats, 1)
	go func() {
		done <- p.runRefreshRound(context.Background())
	}()

	<-started // a worker is mid-refresh
	// Simulate SIGTERM: close stopCh via stopOnce so the test cleanup's Close()
	// (which re-enters stopOnce) stays idempotent. stopCh was not closed before,
	// so this is the single close.
	p.stopOnce.Do(func() { close(p.stopCh) })
	close(release)

	select {
	case st := <-done:
		if st.OK == 0 {
			t.Errorf("round after stopCh = %+v, want the in-flight refresh counted", st)
		}
		if got := atomic.LoadInt32(&calls); int(got) >= n {
			t.Errorf("refreshed %d accounts, want < %d (round must abort early)", got, n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runRefreshRound blocked after stopCh close")
	}
}

// TestRefreshRateLimitedNoEscalation verifies SPEC refresh-loop §2.2: a Privy
// 429 during a refresh is recorded (lastError) but must NOT increment
// consecutiveAuthFailures or disable the account, so a temporary rate limit
// cannot wrongly disable a healthy account.
func TestRefreshRateLimitedNoEscalation(t *testing.T) {
	p := newTestPool(t)
	if _, _, err := p.UpsertAccounts([]AccountInput{input("a@b.com", 100)}); err != nil {
		t.Fatal(err)
	}
	a := p.Get("a@b.com")
	if a.Status != StatusActive {
		t.Fatalf("precondition: status = %q, want active", a.Status)
	}

	f := &fakeClient{refreshErr: &upstream.APIError{Kind: upstream.ErrRateLimited, StatusCode: 429, Message: "too many requests"}}
	orig := p.client
	p.client = f
	t.Cleanup(func() { p.client = orig })

	if err := p.refresh(context.Background(), a); err == nil {
		t.Fatal("refresh over a rate-limited client should return an error")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.consecutiveAuthFailures != 0 {
		t.Errorf("consecutiveAuthFailures = %d, want 0 (429 must not escalate)", a.consecutiveAuthFailures)
	}
	if a.Status != StatusActive {
		t.Errorf("status = %q, want active (429 must not disable)", a.Status)
	}
	if a.lastError == "" {
		t.Errorf("lastError = %q, want the rate-limit message recorded", a.lastError)
	}
	if strings.Contains(a.lastError, "auth failure limit") {
		t.Errorf("lastError = %q, want no disable-limit message", a.lastError)
	}
}

// TestPersistSkipsDeletedAccount verifies P2-3: persistAccount after a
// physical delete must not resurrect the row (TOCTOU guard).
func TestPersistSkipsDeletedAccount(t *testing.T) {
	p := newTestPool(t)
	if _, _, err := p.UpsertAccounts([]AccountInput{input("a@b.com", 100)}); err != nil {
		t.Fatal(err)
	}
	a := p.Get("a@b.com")
	p.RemoveAccount(a, "test delete")
	p.persistAccount(a) // stale persist must be a no-op
	rows, err := p.store.loadAccounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("accounts after delete+persist = %d, want 0 (resurrected)", len(rows))
	}
}

// ---- P0-B1: refresh 失败按错误类型分类 ----

// TestRefreshFailureClassification verifies P0-B1: the refresh() path classifies
// the error so consecutiveAuthFailures is incremented ONLY for a real 401.
// Transient failures (5xx / network / timeout) must not escalate toward the
// disable limit (the 08 cooldown avalanche root cause). A real 401 must still
// escalate so a dead account is eventually disabled.
func TestRefreshFailureClassification(t *testing.T) {
	cases := []struct {
		name           string
		refreshErr     error
		wantAuthEscal  bool // consecutiveAuthFailures should increment
		wantActive     bool // account stays active (not disabled) after one failure
	}{
		{
			name:          "real 401 escalates",
			refreshErr:    &upstream.APIError{Kind: upstream.ErrUnauthorized, StatusCode: 401, Message: "invalid refresh token"},
			wantAuthEscal: true,
			wantActive:    true, // one 401 alone does not hit the limit (default 3)
		},
		{
			name:          "5xx does not escalate",
			refreshErr:    &upstream.APIError{Kind: upstream.ErrUpstream, StatusCode: 500, Message: "Unable to process request"},
			wantAuthEscal: false,
			wantActive:    true,
		},
		{
			name:          "timeout does not escalate",
			refreshErr:    context.DeadlineExceeded,
			wantAuthEscal: false,
			wantActive:    true,
		},
		{
			name:          "network error does not escalate",
			refreshErr:    errors.New("dial tcp: connection reset by peer"),
			wantAuthEscal: false,
			wantActive:    true,
		},
		{
			name:          "ambiguous 403 does not escalate",
			refreshErr:    &upstream.APIError{Kind: upstream.ErrForbidden, StatusCode: 403, Message: "Unable to process request"},
			wantAuthEscal: false,
			wantActive:    true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newTestPool(t)
			if _, _, err := p.UpsertAccounts([]AccountInput{input("a@b.com", 100)}); err != nil {
				t.Fatal(err)
			}
			a := p.Get("a@b.com")
			f := &fakeClient{refreshErr: c.refreshErr}
			orig := p.client
			p.client = f
			t.Cleanup(func() { p.client = orig })

			if err := p.refresh(context.Background(), a); err == nil {
				t.Fatalf("refresh should return the error, got nil")
			}
			a.mu.Lock()
			gotAuth := a.consecutiveAuthFailures
			status := a.Status
			a.mu.Unlock()
			if c.wantAuthEscal {
				if gotAuth != 1 {
					t.Errorf("consecutiveAuthFailures = %d, want 1 (real 401 must escalate)", gotAuth)
				}
			} else {
				if gotAuth != 0 {
					t.Errorf("consecutiveAuthFailures = %d, want 0 (transient must not escalate)", gotAuth)
				}
			}
			if c.wantActive && status != StatusActive {
				t.Errorf("status = %q, want active (one failure must not disable)", status)
			}
		})
	}
}

// TestRefreshReal401DisablesAfterLimit verifies P0-B1 end-to-end: three real
// 401 refresh failures on an active account disable it (the auth-failure limit
// still applies to genuine auth errors), while transient failures never reach
// the limit.
func TestRefreshReal401DisablesAfterLimit(t *testing.T) {
	p := newTestPool(t)
	if _, _, err := p.UpsertAccounts([]AccountInput{input("a@b.com", 100)}); err != nil {
		t.Fatal(err)
	}
	a := p.Get("a@b.com")
	f := &fakeClient{refreshErr: &upstream.APIError{Kind: upstream.ErrUnauthorized, StatusCode: 401, Message: "invalid refresh token"}}
	orig := p.client
	p.client = f
	t.Cleanup(func() { p.client = orig })

	for i := 0; i < p.authFailLimit; i++ {
		_ = p.refresh(context.Background(), a)
	}
	if a.Status != StatusDisabled {
		t.Errorf("status = %q, want disabled after %d real 401 refresh failures", a.Status, p.authFailLimit)
	}
}

// TestPickStickySameConversationID verifies the continuation prerequisite
// (ticket 12): the same conversation_id must select the same account across
// consecutive requests so a "continue" after finish_reason=length lands on the
// account that holds the upstream conversation.
func TestPickStickySameConversationID(t *testing.T) {
	p := newTestPool(t)
	if _, _, err := p.UpsertAccounts([]AccountInput{input("a@b.com", 100), input("c@d.com", 100), input("e@f.com", 100)}); err != nil {
		t.Fatal(err)
	}
	first := p.Pick("conv-sticky-1")
	second := p.Pick("conv-sticky-1")
	if first == nil || second == nil {
		t.Fatalf("Pick returned nil: %v / %v", first, second)
	}
	if first != second {
		t.Errorf("same conversation_id picked different accounts: %s vs %s", first.Email, second.Email)
	}
	// A different conversation may select a different account, but must remain
	// sticky to itself across two calls.
	other1 := p.Pick("conv-sticky-2")
	other2 := p.Pick("conv-sticky-2")
	if other1 == nil || other2 == nil {
		t.Fatalf("second conversation Pick returned nil: %v / %v", other1, other2)
	}
	if other1 != other2 {
		t.Errorf("conversation 2 not sticky: %s vs %s", other1.Email, other2.Email)
	}
}

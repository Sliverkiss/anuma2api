// Package pool manages the account pool backed by SQLite: least-recently-used
// rotation with conversation stickiness, token refresh, upload-driven upserts
// (POST /api/accounts), an async health-check worker, and a daily cooldown
// patrol. Zero-credit accounts enter a cooldown state instead of being deleted
// (SPEC-cooldown §2.2) so they survive until the monthly credit replenishment;
// the cooldown patrol restores them when the real balance is positive. Tokens
// are never logged; admin views get masked accounts.
package pool

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"anuma2api/gateway/internal/upstream"
)

// Account lifecycle statuses (SPEC 3.2 / 3.3). A 'removed' status no longer
// exists: accounts are physically deleted instead (SPEC-upload §2.2).
const (
	StatusActive   = "active"   // participates in rotation
	StatusDisabled = "disabled" // temporarily out of rotation (manual, auth-fail limit)
	// StatusCooldown marks a zero-credit / unqueryable account waiting for the
	// monthly credit replenishment. It is NOT deleted (SPEC-cooldown §2.2): the
	// daily cooldown patrol re-checks its balance and restores it to active.
	StatusCooldown = "cooldown"
)

// manualDisabledReason is recorded in last_error when an operator disables an
// account via the admin API, so the async health check does not resurrect it.
// It is only restored for legacy rows written before physical deletion.
const manualDisabledReason = "manually disabled"

const (
	// healthCheckCooldown is the minimum interval between health checks of the
	// same account (SPEC 3.3, storm protection).
	healthCheckCooldown = 30 * time.Second
	// pickCooldown is how long a recently-failed account is excluded from Pick
	// (P0-2 short-term failure cooldown). It matches the health-check cooldown so
	// a failed account is skipped until its async health check has a chance to
	// run.
	pickCooldown = 30 * time.Second
	// healthChSize bounds the pending health-check queue.
	healthChSize = 256
	// refreshRoundTimeout bounds one full refresh round (SPEC refresh-loop §3.1).
	refreshRoundTimeout = 10 * time.Minute
	// refreshRoundWorkers is the concurrency bound for one refresh round: at most
	// this many accounts are refreshed in flight (SPEC refresh-loop §2.1). Kept
	// small so the aggregate Privy request rate stays well below its rate limit.
	refreshRoundWorkers = 6
)

// refreshAccountDelay is the global refresh pacing period (SPEC refresh-loop
// §3.1: ~120ms per refresh, 856 accounts ≈ 103s, stays below Privy's ~10 QPS
// cap at ≈8.3 QPS with ~17% headroom). A single shared ticker paces every
// refresh start, so the aggregate start-rate is 1/refreshAccountDelay
// regardless of worker count. A variable so tests can shorten it.
var refreshAccountDelay = 120 * time.Millisecond

// refreshRateLimitBackoff is how long a single worker pauses after a 429
// (rate-limited) refresh before taking another job, letting Privy recover.
// A variable so tests can shorten it.
var refreshRateLimitBackoff = time.Second

// cooldownAccountDelay is the per-account pause between cooldown balance
// checks (SPEC-cooldown §2.3, same ~50ms cadence as the refresh loop).
// A variable so tests can shorten it.
var cooldownAccountDelay = 50 * time.Millisecond

// balanceRetryDelay is the 1s pause before a single retry of a failed balance
// query (SPEC-cooldown §2.4).
const balanceRetryDelay = time.Second

// maxRetryBackoff caps the per-failure conversation backoff (SPEC-cooldown §2.4).
const maxRetryBackoff = 2 * time.Second

// Account is an account in the pool. Token fields are kept in memory only and
// never logged; the masked view is produced by Masked(). Runtime mutations
// persist through the owning Pool's SQLite store.
type Account struct {
	Email            string
	WalletAddress    string
	WalletID         string
	UserID           string
	AvailableCredits int
	SubscriptionTier string
	ExpiresAt        int64 // unix seconds; upload value, refreshed on token refresh
	IdentityToken    string
	AccessToken      string
	RefreshToken     string
	Status           string // active | disabled | cooldown
	RemovedAt        int64  // legacy column; no longer written (physical delete)

	mu                      sync.Mutex
	// refreshMu serializes token refreshes for this account so concurrent
	// requests (401 retry + NeedsRefresh path, or background + manual rounds)
	// cannot race the privy refresh-token rotation (P1-4).
	refreshMu               sync.Mutex
	manualDisabled          bool // set by admin disable; blocks health-check resurrection
	deleted                 bool // physically removed from the pool; blocks persistence
	lastError               string
	failCount               int
	consecutiveAuthFailures int
	lastUsedAt              time.Time
	// todayCalls counts successful completions served today (D6): incremented
	// once per recordSuccess and rolled over when the local date changes. It is
	// a runtime counter (not persisted), so a restart resets it — acceptable for
	// a "today" metric that the old account-dedup approximation also undercounted.
	todayCalls     int
	todayCallsDate string // "2006-01-02" of the last successful call

	pool *Pool
}

// Masked is the admin-safe view of an account (SPEC 6: token only first 8
// chars). Never includes full tokens.
type Masked struct {
	Email            string `json:"email"`
	WalletAddress    string `json:"wallet_address"`
	AvailableCredits int    `json:"available_credits"`
	SubscriptionTier string `json:"subscription_tier"`
	ExpiresAt        int64  `json:"expires_at"`
	Status           string `json:"status"`
	Disabled         bool   `json:"disabled"`
	LastError        string `json:"last_error,omitempty"`
	IdentityToken    string `json:"identity_token_masked"`
	AccessToken      string `json:"access_token_masked"`
	RefreshToken     string `json:"refresh_token_masked"`
}

// maskToken keeps only the first 8 and last 4 characters (SPEC 6).
func maskToken(tok string) string {
	if tok == "" {
		return ""
	}
	if len(tok) <= 12 {
		return "****"
	}
	return tok[:8] + "..." + tok[len(tok)-4:]
}

// Masked returns the admin-safe view.
func (a *Account) Masked() Masked {
	a.mu.Lock()
	defer a.mu.Unlock()
	return Masked{
		Email:            a.Email,
		WalletAddress:    a.WalletAddress,
		AvailableCredits: a.AvailableCredits,
		SubscriptionTier: a.SubscriptionTier,
		ExpiresAt:        a.ExpiresAt,
		Status:           a.Status,
		Disabled:         a.Status == StatusDisabled,
		LastError:        a.lastError,
		IdentityToken:    maskToken(a.IdentityToken),
		AccessToken:      maskToken(a.AccessToken),
		RefreshToken:     maskToken(a.RefreshToken),
	}
}

// isRefreshTarget reports whether an account participates in a token refresh
// round: active accounts (keep-alive) and cooldown accounts (token kept fresh
// so the account is immediately usable when the cooldown patrol restores it,
// P0-3). Disabled accounts are never refreshed.
func (a *Account) isRefreshTarget() bool {
	st := a.StatusValue()
	return st == StatusActive || st == StatusCooldown
}

// isHealthy reports whether the account can serve a request right now.
func (a *Account) isHealthy() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.isHealthyLocked()
}

func (a *Account) disable(reason string) {
	a.mu.Lock()
	a.Status = StatusDisabled
	a.lastError = reason
	a.mu.Unlock()
	if a.pool != nil {
		a.pool.persistAccount(a)
	}
}

// enterCooldown moves the account into the cooldown state (SPEC-cooldown §2.2):
// zero credits or an unrecoverable balance query parks the account instead of
// deleting it, so the monthly replenishment can bring it back. A manually
// disabled account is left disabled — the operator's decision wins. Idempotent:
// re-entering cooldown is harmless.
func (a *Account) enterCooldown(reason string) {
	a.mu.Lock()
	if a.manualDisabled {
		a.mu.Unlock()
		return
	}
	a.Status = StatusCooldown
	a.lastError = truncate(reason, 200)
	a.mu.Unlock()
	if a.pool != nil {
		a.pool.persistAccount(a)
	}
	log.Printf("pool: account %s entered cooldown (%s)", AccountIdentity(a.Email), truncate(reason, 120))
}

func (a *Account) recordFailure(kind upstream.ErrorKind, msg string) {
	a.mu.Lock()
	a.failCount++
	a.lastError = truncate(msg, 200)
	a.lastUsedAt = time.Now() // rotate away from a failing account
	if kind == upstream.ErrUnauthorized || kind == upstream.ErrForbidden {
		a.consecutiveAuthFailures++
	} else {
		a.consecutiveAuthFailures = 0
	}
	limit := defaultAuthFailLimit
	if a.pool != nil {
		limit = a.pool.authFailLimit
	}
	if a.consecutiveAuthFailures >= limit && a.Status == StatusActive {
		a.Status = StatusDisabled
		a.lastError = "auth failure limit reached"
	}
	a.mu.Unlock()
	if a.pool != nil {
		// noteFailure takes p.mu; it is called after releasing a.mu so the lock
		// order stays p.mu→a.mu (Pick holds p.mu and takes a.mu via isHealthy) and
		// never inverts to a.mu→p.mu (P0-2).
		a.pool.noteFailure(a.Email)
		a.pool.persistAccount(a)
	}
}

// recordCooldownRefreshFailure records a token-refresh error on a cooldown
// account without counting it as an auth failure (P0-3). The account is parked,
// so a refresh failure is expected (stale token) and must not push
// consecutiveAuthFailures toward the disable limit that would apply once the
// cooldown patrol restores it. Only lastError is recorded for observability.
func (a *Account) recordCooldownRefreshFailure(msg string) {
	a.mu.Lock()
	a.lastError = truncate(msg, 200)
	a.mu.Unlock()
	if a.pool != nil {
		a.pool.persistAccount(a)
	}
}

// recordRefreshRateLimited records a Privy 429 on an active account without
// counting it as an auth failure (SPEC refresh-loop §2.2). A rate-limited
// account is healthy — Privy is temporarily overwhelmed — so escalating its
// consecutiveAuthFailures would wrongly disable it after a few 429s. Only
// lastError and lastUsedAt (rotate away from the account this round) are
// updated; the account is never disabled.
func (a *Account) recordRefreshRateLimited(msg string) {
	a.mu.Lock()
	a.lastError = truncate(msg, 200)
	a.lastUsedAt = time.Now()
	a.mu.Unlock()
	if a.pool != nil {
		a.pool.persistAccount(a)
	}
}

// recordTransientFailure records a transient refresh failure (Privy 5xx /
// network / timeout / EOF) on an active account WITHOUT incrementing
// consecutiveAuthFailures (P0-B1). The failure is not auth-level — the token
// is probably still valid, Privy or the network is just momentarily broken —
// so escalating toward the auth-failure disable limit would wrongly disable a
// healthy account after three transient blips (the 08 cooldown avalanche: 26×
// 500 + 6× timeout were misclassified as auth failures and snowballed into 82
// cooldowns). Only lastError and failCount are updated; the account stays
// active. Mirrors recordCooldownRefreshFailure semantics but for active accounts.
func (a *Account) recordTransientFailure(msg string) {
	a.mu.Lock()
	a.failCount++
	a.lastError = truncate(msg, 200)
	a.lastUsedAt = time.Now() // rotate away from a failing account
	// Deliberately do NOT touch consecutiveAuthFailures: a transient failure
	// must not push an active account toward the disable limit (P0-B1).
	a.mu.Unlock()
	if a.pool != nil {
		a.pool.noteFailure(a.Email)
		a.pool.persistAccount(a)
	}
}

// refreshErrKind classifies a token-refresh error so the refresh path can
// decide whether to treat it as auth-level (real 401) or transient
// (5xx/timeout/network/unknown). A 403 is treated as transient here because
// the 08 incident showed the portal returns 403 "Unable to process request"
// for transient server-side faults too; treating it as auth would snowball.
// P0-B1.
func refreshErrKind(err error) (authFailure bool, transient bool) {
	if err == nil {
		return false, false
	}
	apiErr, ok := err.(*upstream.APIError)
	if !ok {
		// Raw transport error (timeout / EOF / connection reset): transient.
		return false, true
	}
	switch apiErr.Kind {
	case upstream.ErrUnauthorized:
		// Real 401 from Privy: the refresh token is invalid/expired — escalate
		// toward the disable limit so a dead account is removed.
		return true, false
	default:
		// ErrUpstream (5xx), ErrForbidden (ambiguous 403), ErrRateLimited
		// (handled separately above), ErrInsufficient, ErrUnknown: all
		// transient — do not escalate toward the disable limit (P0-B1).
		return false, true
	}
}

func (a *Account) recordSuccess(creditsUsed int) {
	a.mu.Lock()
	a.consecutiveAuthFailures = 0
	// A success means the account just worked, so failCount should reflect the
	// recent history rather than accumulate forever (P1-3).
	a.failCount = 0
	a.lastError = ""
	a.lastUsedAt = time.Now()
	// Do not resurrect an account that is not currently healthy (disabled or in
	// cooldown): a stale success reply must not override the operator's decision
	// or the cooldown lifecycle (P2-6).
	if a.Status != StatusActive {
		a.mu.Unlock()
		if a.pool != nil {
			a.pool.noteSuccess(a.Email)
		}
		return
	}
	a.Status = StatusActive
	// Count the completed call (D6): every successful response bumps the
	// today-call counter, rolled over when the date changes.
	a.countCallLocked()
	if creditsUsed > 0 && a.AvailableCredits > 0 {
		a.AvailableCredits -= creditsUsed
		if a.AvailableCredits < 0 {
			a.AvailableCredits = 0
		}
	}
	// A success that drains the last credit must not leave the account stranded
	// in active/0 limbo: park it in cooldown so the monthly patrol can restore
	// it when the real balance is positive again (P1-1).
	if a.AvailableCredits <= 0 {
		a.Status = StatusCooldown
		a.lastError = "no credits"
	}
	a.mu.Unlock()
	if a.pool != nil {
		// A success clears the short-term failure cooldown even when the account
		// drains to cooldown (the call itself succeeded). noteSuccess takes p.mu
		// after a.mu is released, preserving the p.mu→a.mu lock order (P0-2).
		a.pool.noteSuccess(a.Email)
		a.pool.persistAccount(a)
	}
}

// countCallLocked increments the today call counter, rolling it over on date
// change. The caller must hold a.mu.
func (a *Account) countCallLocked() {
	day := time.Now().Format("2006-01-02")
	if a.todayCallsDate != day {
		a.todayCallsDate = day
		a.todayCalls = 0
	}
	a.todayCalls++
}

// needsRefresh reports whether the identity token should be refreshed before
// use (within 5 minutes of expiry). The token is read under the account lock so
// a concurrent refresh cannot tear it (P1-6).
func (a *Account) needsRefresh() bool {
	exp := upstream.JWTExpiry(a.IdentityTokenValue())
	if exp == 0 {
		return false
	}
	return exp-time.Now().Unix() < 300
}

// NeedsRefresh is the exported form of needsRefresh.
func (a *Account) NeedsRefresh() bool { return a.needsRefresh() }

// Disable is the exported form of disable.
func (a *Account) Disable(reason string) { a.disable(reason) }

// EnterCooldown is the exported form of enterCooldown (P1-2: balance-exhausted
// accounts park in cooldown so the monthly patrol can restore them).
func (a *Account) EnterCooldown(reason string) { a.enterCooldown(reason) }

// RecordFailure is the exported form of recordFailure.
func (a *Account) RecordFailure(kind upstream.ErrorKind, msg string) { a.recordFailure(kind, msg) }

// RecordSuccess is the exported form of recordSuccess.
func (a *Account) RecordSuccess(creditsUsed int) { a.recordSuccess(creditsUsed) }

// IdentityTokenValue returns the identity token under the account lock so
// readers never race the refresh path (P1-6).
func (a *Account) IdentityTokenValue() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.IdentityToken
}

// RefreshTokensValue returns a consistent snapshot of the access/refresh tokens
// under the account lock (P1-6); the refresh path writes them under the same
// lock plus the per-account refreshMu (P1-4).
func (a *Account) RefreshTokensValue() (access, refresh string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.AccessToken, a.RefreshToken
}

// StatusValue returns the lifecycle status under the account lock (P1-6 / 2-2).
func (a *Account) StatusValue() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Status
}

// isDeleted reports whether the account was physically removed from the pool.
func (a *Account) isDeleted() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.deleted
}

// snapshot captures the account fields for persistence under the account lock.
func (a *Account) snapshot() accountRow {
	a.mu.Lock()
	defer a.mu.Unlock()
	return accountRow{
		Email:                   a.Email,
		WalletAddress:           a.WalletAddress,
		WalletID:                a.WalletID,
		UserID:                  a.UserID,
		Tier:                    a.SubscriptionTier,
		IdentityToken:           a.IdentityToken,
		AccessToken:             a.AccessToken,
		RefreshToken:            a.RefreshToken,
		Credits:                 a.AvailableCredits,
		Status:                  a.Status,
		FailCount:               a.failCount,
		ConsecutiveAuthFailures: a.consecutiveAuthFailures,
		LastError:               a.lastError,
		LastUsedAt:              a.lastUsedAt.Unix(),
		RemovedAt:               a.RemovedAt,
		ExpiresAt:               a.ExpiresAt,
	}
}

// balanceRefresher is the slice of the upstream client the pool needs: balance
// queries for the async health check and privy token refresh. *upstream.Client
// satisfies it; tests inject a fake.
type balanceRefresher interface {
	GetBalance(ctx context.Context, identityToken string) (*upstream.Balance, error)
	RefreshToken(ctx context.Context, privyAccessToken, refreshToken string) (*upstream.RefreshResult, error)
}

// Options configures the pool (retry / auth-failure thresholds).
type Options struct {
	MaxRetries    int
	AuthFailLimit int
	// RefreshInterval is how often the background token keep-alive loop runs
	// (SPEC refresh-loop §3.2). <=0 disables the loop.
	RefreshInterval time.Duration
	// CooldownInterval is how often the background cooldown patrol re-checks
	// zero-credit accounts (SPEC-cooldown §2.3). <=0 disables the loop.
	CooldownInterval time.Duration
	// RetryBackoff is the base delay between failed account attempts in the
	// conversation retry loops (SPEC-cooldown §2.4); 0 uses the default 200ms.
	RetryBackoff time.Duration
}

// defaultAuthFailLimit is used when Options.AuthFailLimit is 0 (SPEC 3.3 default 3).
const defaultAuthFailLimit = 3

// Pool is the account pool.
type Pool struct {
	client balanceRefresher
	store  *Store

	maxRetries    int
	authFailLimit int

	// refreshInterval is how often the background keep-alive round runs; <=0
	// means the refresh loop is disabled (SPEC refresh-loop §3.2).
	refreshInterval time.Duration
	// refreshRunning guards against two refresh rounds running concurrently
	// (background loop + manual trigger), SPEC refresh-loop §3.3.
	refreshRunning atomic.Bool
	// refresher is the per-account refresh call. A field so tests can inject a
	// fake; production uses p.refresh.
	refresher func(ctx context.Context, a *Account) error

	// cooldownInterval is how often the cooldown patrol runs; <=0 disables it
	// (SPEC-cooldown §2.3).
	cooldownInterval time.Duration
	// retryBackoff is the base backoff between conversation retries
	// (SPEC-cooldown §2.4); default 200ms when Options.RetryBackoff is 0.
	retryBackoff time.Duration
	// cooldownRunning guards against two cooldown rounds running concurrently
	// (background loop + manual trigger).
	cooldownRunning atomic.Bool

	mu       sync.RWMutex
	accounts []*Account

	// recentFails maps an account email to the time of its last recorded failure
	// (P0-2 short-term failure cooldown). Pick skips accounts whose entry is
	// younger than pickCooldown so the retry loop rotates away from a just-failed
	// account. It is runtime-only (never persisted), bounded by the account count,
	// and entries are lazily ignored once stale and removed on recordSuccess.
	recentFails map[string]time.Time

	healthCh   chan *Account
	cooldownMu sync.Mutex
	cooldowns  map[string]time.Time

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// New creates a pool backed by SQLite at dbPath and loads the in-memory account
// set from the database. The gateway no longer imports accounts.csv (SPEC-upload
// §2.3): accounts are pushed to the gateway via POST /api/accounts.
func New(client *upstream.Client, dbPath string, opts Options) (*Pool, error) {
	if opts.MaxRetries <= 0 {
		opts.MaxRetries = 3
	}
	if opts.AuthFailLimit <= 0 {
		opts.AuthFailLimit = defaultAuthFailLimit
	}
	if opts.RetryBackoff <= 0 {
		opts.RetryBackoff = 200 * time.Millisecond
	}
	store, err := openStore(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite store: %w", err)
	}
	// Rows left with status='removed' predate physical deletion (SPEC-upload
	// §2.2); purge them so the pool starts clean and only live accounts remain.
	if _, err := store.purgeRemoved(); err != nil {
		store.Close()
		return nil, fmt.Errorf("purge legacy removed accounts: %w", err)
	}
	p := &Pool{
		client:           client,
		store:            store,
		maxRetries:       opts.MaxRetries,
		authFailLimit:    opts.AuthFailLimit,
		refreshInterval:  opts.RefreshInterval,
		cooldownInterval: opts.CooldownInterval,
		retryBackoff:     opts.RetryBackoff,
		healthCh:         make(chan *Account, healthChSize),
		cooldowns:        make(map[string]time.Time),
		recentFails:      make(map[string]time.Time),
		stopCh:           make(chan struct{}),
	}
	p.refresher = p.refresh
	accs, err := store.loadAccounts()
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("load accounts: %w", err)
	}
	for _, a := range accs {
		a.pool = p
	}
	p.accounts = accs
	return p, nil
}

// Start launches the background health-check worker and, when configured, the
// token keep-alive refresh loop and the cooldown patrol loop.
func (p *Pool) Start() {
	p.wg.Add(1)
	go p.healthLoop()
	if p.refreshInterval > 0 {
		p.wg.Add(1)
		go p.refreshLoop()
	}
	if p.cooldownInterval > 0 {
		p.wg.Add(1)
		go p.cooldownLoop()
	}
}

// Close stops background loops and closes the SQLite store.
func (p *Pool) Close() {
	p.stopOnce.Do(func() {
		close(p.stopCh)
		p.wg.Wait()
		if p.store != nil {
			_ = p.store.Close()
		}
	})
}

// MaxRetries returns the per-request retry/account-switch budget.
func (p *Pool) MaxRetries() int { return p.maxRetries }

// RetryBackoff returns the base backoff between failed account attempts
// (SPEC-cooldown §2.4), defaulting to 200ms.
func (p *Pool) RetryBackoff() time.Duration { return p.retryBackoff }

// AccountInput is the upload payload for POST /api/accounts (SPEC-upload §2.1).
// email and identity_token are required; the rest are best-effort upserts.
type AccountInput struct {
	Email            string `json:"email"`
	WalletAddress    string `json:"wallet_address"`
	WalletID         string `json:"wallet_id"`
	UserID           string `json:"user_id"`
	Tier             string `json:"tier"`
	IdentityToken    string `json:"identity_token"`
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	AvailableCredits int    `json:"available_credits"`
	ExpiresAt        int64  `json:"expires_at"`
}

// UpsertAccounts inserts new accounts and fully updates existing ones by email
// (upload is the latest state). It returns the number of accounts added and
// updated. Both SQLite and the in-memory set are refreshed; a physically
// deleted account is re-created (resurrected) by re-uploading it because no
// 'removed' record is left behind (SPEC-upload §2.1).
func (p *Pool) UpsertAccounts(inputs []AccountInput) (added, updated int, err error) {
	added, updated, err = p.store.upsertAccounts(inputs)
	if err != nil {
		return 0, 0, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, in := range inputs {
		if strings.TrimSpace(in.Email) == "" || strings.TrimSpace(in.IdentityToken) == "" {
			continue
		}
		var found *Account
		for _, a := range p.accounts {
			if a.Email == in.Email {
				found = a
				break
			}
		}
		if found == nil {
			a := accountFromInput(in)
			a.pool = p
			p.accounts = append(p.accounts, a)
		} else {
			found.applyInput(in)
		}
	}
	return added, updated, nil
}

// statusForCredits maps an uploaded credit count to a lifecycle status: zero
// credits parks the account in cooldown (waiting for monthly replenishment)
// instead of active (SPEC-cooldown §2.2). Positive credits are active.
func statusForCredits(credits int) (string, string) {
	if credits <= 0 {
		return StatusCooldown, "no credits"
	}
	return StatusActive, ""
}

func accountFromInput(in AccountInput) *Account {
	status, lastErr := statusForCredits(in.AvailableCredits)
	return &Account{
		Email:            in.Email,
		WalletAddress:    in.WalletAddress,
		WalletID:         in.WalletID,
		UserID:           in.UserID,
		SubscriptionTier: in.Tier,
		AvailableCredits: in.AvailableCredits,
		ExpiresAt:        in.ExpiresAt,
		IdentityToken:    in.IdentityToken,
		AccessToken:      in.AccessToken,
		RefreshToken:     in.RefreshToken,
		Status:           status,
		lastError:        lastErr,
	}
}

// applyInput overwrites the uploaded fields on an existing account. An upload
// implies a fresh account state: active when it carries credits, cooldown when
// it does not (SPEC-cooldown §2.2).
func (a *Account) applyInput(in AccountInput) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.WalletAddress = in.WalletAddress
	a.WalletID = in.WalletID
	a.UserID = in.UserID
	a.SubscriptionTier = in.Tier
	a.AvailableCredits = in.AvailableCredits
	a.ExpiresAt = in.ExpiresAt
	a.IdentityToken = in.IdentityToken
	a.AccessToken = in.AccessToken
	a.RefreshToken = in.RefreshToken
	a.Status, a.lastError = statusForCredits(in.AvailableCredits)
	a.manualDisabled = false
	a.failCount = 0
	a.consecutiveAuthFailures = 0
}

// Delete physically removes the account with the given email and returns its
// masked view. It backs DELETE /api/accounts/{email}, which now deletes rather
// than marks removed (SPEC-upload §2.2).
func (p *Pool) Delete(email string) (*Masked, error) {
	a := p.Get(email)
	if a == nil {
		return nil, errors.New("account not found")
	}
	m := a.Masked()
	p.RemoveAccount(a, "deleted via admin API")
	return &m, nil
}

// Stats summarizes the pool (SPEC 6 /api/stats). Cooldown accounts are counted
// separately from disabled ones (SPEC-cooldown §2.5).
type Stats struct {
	TotalAccounts int   `json:"total_accounts"`
	Available     int   `json:"available"`
	Disabled      int   `json:"disabled"`
	Cooldown      int   `json:"cooldown"`
	TotalCredits  int64 `json:"total_credits"`
	TodayCalls    int   `json:"today_calls"`
}

// Stats returns pool statistics.
func (p *Pool) Stats() Stats {
	p.mu.RLock()
	defer p.mu.RUnlock()
	s := Stats{TotalAccounts: len(p.accounts)}
	today := time.Now().Format("2006-01-02")
	for _, a := range p.accounts {
		a.mu.Lock()
		switch a.Status {
		case StatusDisabled:
			s.Disabled++
		case StatusCooldown:
			s.Cooldown++
		default:
			if a.isHealthyLocked() {
				s.Available++
			} else {
				s.Disabled++
			}
		}
		s.TotalCredits += int64(a.AvailableCredits)
		// D6: today_calls sums the real successful-call counter (incremented per
		// recordSuccess), not the number of accounts used today.
		if a.todayCallsDate == today {
			s.TodayCalls += a.todayCalls
		}
		a.mu.Unlock()
	}
	return s
}

// isHealthyLocked assumes a.mu is held and checks health without re-locking.
func (a *Account) isHealthyLocked() bool {
	if a.Status != StatusActive {
		return false
	}
	if a.AvailableCredits <= 0 {
		return false
	}
	limit := defaultAuthFailLimit
	if a.pool != nil {
		limit = a.pool.authFailLimit
	}
	if a.consecutiveAuthFailures >= limit {
		return false
	}
	return true
}

// List returns the masked view of every account in the pool (active + disabled
// + cooldown). There is no status filter anymore: removed accounts are
// physically deleted.
func (p *Pool) List() []Masked {
	return p.ListByStatus("")
}

// ListByStatus returns the masked view of every account whose status matches
// filter ("" or "all" returns all accounts; otherwise a lifecycle status such
// as active/disabled/cooldown). It backs GET /api/accounts?status=cooldown
// (SPEC-cooldown §2.5).
func (p *Pool) ListByStatus(filter string) []Masked {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]Masked, 0, len(p.accounts))
	for _, a := range p.accounts {
		if filter != "" && filter != "all" && a.Status != filter {
			continue
		}
		out = append(out, a.Masked())
	}
	return out
}

// Get returns the account with the given email, or nil.
func (p *Pool) Get(email string) *Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, a := range p.accounts {
		if a.Email == email {
			return a
		}
	}
	return nil
}

// RefreshToken refreshes the identity token for an account via privy.
func (p *Pool) RefreshToken(ctx context.Context, email string) (*Masked, error) {
	a := p.Get(email)
	if a == nil {
		return nil, errors.New("account not found")
	}
	if err := p.refresh(ctx, a); err != nil {
		return nil, err
	}
	m := a.Masked()
	return &m, nil
}

// RefreshAccountToken refreshes the identity token for an account and returns
// only an error (for internal retry loops).
func (p *Pool) RefreshAccountToken(ctx context.Context, email string) error {
	a := p.Get(email)
	if a == nil {
		return errors.New("account not found")
	}
	return p.refresh(ctx, a)
}

func (p *Pool) refresh(ctx context.Context, a *Account) error {
	// Serialize refreshes per account: two goroutines refreshing the same
	// account concurrently would race the privy refresh-token rotation and one
	// could fail against an already-rotated token, inflating the auth-failure
	// counter and disabling a healthy account (P1-4). The caller holds no lock
	// here, so this mutex fully guards the read-tokens → call → write-tokens
	// critical section.
	a.refreshMu.Lock()
	defer a.refreshMu.Unlock()

	access, refresh := a.RefreshTokensValue()
	res, err := p.client.RefreshToken(ctx, access, refresh)
	if err != nil {
		// A cooldown account's refresh failure is expected (its token may be
		// stale) and must not escalate toward the auth-failure disable limit:
		// the account is parked, not being picked, and the failure would
		// unfairly penalize it once the cooldown patrol restores it (P0-3).
		if a.StatusValue() == StatusCooldown {
			a.recordCooldownRefreshFailure("refresh failed: " + err.Error())
		} else if isRateLimited(err) {
			// Privy 429: the account is healthy, so record the rate limit
			// without incrementing consecutiveAuthFailures — three 429s must
			// not disable a working account (SPEC refresh-loop §2.2).
			a.recordRefreshRateLimited("refresh rate limited: " + err.Error())
		} else if authFailure, _ := refreshErrKind(err); authFailure {
			// Real Privy 401: the refresh token is genuinely invalid. Escalate
			// toward the auth-failure disable limit so a dead account is
			// eventually removed (P0-B1).
			a.recordFailure(upstream.ErrUnauthorized, "refresh failed: "+err.Error())
		} else {
			// Transient failure (5xx / network / timeout / EOF / ambiguous 403):
			// record it for observability and rotation but do NOT increment
			// consecutiveAuthFailures — the token is probably still valid and
			// escalating would snowball into a cooldown on a healthy account
			// (P0-B1, the 08 incident root cause).
			a.recordTransientFailure("refresh failed: " + err.Error())
		}
		return err
	}
	a.mu.Lock()
	a.IdentityToken = res.IdentityToken
	a.AccessToken = res.PrivyAccessToken
	if res.RefreshToken != "" {
		a.RefreshToken = res.RefreshToken
	}
	// Keep expires_at truthful: decode the exp claim of the fresh identity
	// token so the persisted column tracks the new token instead of the stale
	// upload-time value (refresh used to leave it untouched, making every
	// account look expired). Guard exp>0 so an unparseable token (e.g. test
	// fakes) never clobbers a known expiry with 0.
	if exp := upstream.JWTExpiry(res.IdentityToken); exp > 0 {
		a.ExpiresAt = exp
	}
	a.consecutiveAuthFailures = 0
	a.mu.Unlock()
	p.persistAccount(a)
	return nil
}

// RefreshStats reports one refresh round's outcome (SPEC refresh-loop §3.3).
// RateLimited counts Privy 429s separately from Fail: a rate-limited account is
// healthy (just throttled), so it must not inflate the dead-account counter
// (SPEC-ratelimit-fix P0-2).
type RefreshStats struct {
	OK          int  `json:"ok"`
	Fail        int  `json:"fail"`
	RateLimited int  `json:"rate_limited,omitempty"`
	Busy        bool `json:"busy,omitempty"`
}

// RefreshAll manually triggers one refresh round over every refresh target
// (active + cooldown accounts). If another round (background loop or a previous manual call) is
// still running, it returns Busy=true immediately and does not start a second
// one (SPEC refresh-loop §3.3). It shares the implementation with the
// background loop via runRefreshRound.
func (p *Pool) RefreshAll(ctx context.Context) RefreshStats {
	if !p.refreshRunning.CompareAndSwap(false, true) {
		return RefreshStats{Busy: true}
	}
	defer p.refreshRunning.Store(false)
	return p.runRefreshRound(ctx)
}

// runRefreshRound walks every refresh target account (active + cooldown) and
// refreshes its tokens (identity/access/refresh via privy). It is shared by the
// background refreshLoop and the manual POST /api/accounts/refresh-all handler.
// Cooldown accounts are included so their tokens stay fresh and they are
// immediately usable when the cooldown patrol restores them (P0-3).
//
// It enforces a 10-minute round timeout, a per-account 120ms delay to stay
// below Privy's rate cap, and only logs counts — never tokens (SPEC
// refresh-loop §3.1).
func (p *Pool) runRefreshRound(ctx context.Context) RefreshStats {
	ctx, cancel := context.WithTimeout(ctx, refreshRoundTimeout)
	defer cancel()

	// 1. Snapshot refresh targets (active + cooldown, P0-3) under the pool lock.
	p.mu.RLock()
	targets := make([]*Account, 0, len(p.accounts))
	for _, a := range p.accounts {
		if a.isRefreshTarget() {
			targets = append(targets, a)
		}
	}
	p.mu.RUnlock()
	if len(targets) == 0 {
		log.Printf("refresh round: ok=0 fail=0 total=0")
		return RefreshStats{}
	}

	// 2. Bounded worker pool: at most refreshRoundWorkers goroutines consume the
	// job channel; fewer workers when there are fewer accounts (behaviorally
	// equivalent to the serial loop for small pools).
	workers := refreshRoundWorkers
	if len(targets) < workers {
		workers = len(targets)
	}
	jobs := make(chan *Account, workers)

	// 3. Global pacing: a single shared ticker gates every refresh start, so the
	// aggregate start-rate is 1/refreshAccountDelay regardless of worker count
	// (SPEC refresh-loop §2.1). Each tick is consumed by exactly one worker.
	pace := refreshAccountDelay
	if pace <= 0 {
		pace = 50 * time.Millisecond // time.NewTicker(0) panics; defend against it
	}
	pacer := time.NewTicker(pace)
	defer pacer.Stop()

	var okCount, failCount, rateLimitCount atomic.Int32
	// failKinds tallies the failure ErrorKind distribution for the round log
	// (P1-B5). It is aggregated without any account identifier so no PII leaks.
	failKinds := make(map[string]int)
	var failKindsMu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// The receive on jobs is not wrapped in a select: workers rely on the
			// feeder closing jobs on cancellation to unblock idle ones, so there
			// is no path where a worker hangs waiting for a job after the round
			// is aborted.
			for a := range jobs {
				// The account may have been disabled/deleted by another goroutine
				// while queued; skip it rather than hammering the API.
				if !a.isRefreshTarget() || a.isDeleted() {
					continue
				}
				if ctx.Err() != nil { // timeout/cancel: drop remaining queued accounts
					return
				}
				select { // global pacing: take one tick before each refresh
				case <-pacer.C:
				case <-ctx.Done():
					return
				case <-p.stopCh: // pool is closing (SIGTERM): stop issuing new requests
					return
				}
				if err := p.refresher(ctx, a); err != nil {
					if isRateLimited(err) {
						// 429: the account is healthy, only throttled — count it
						// separately and do NOT queue a health check (SPEC
						// refresh-loop §2.2). refresh already records the rate limit
						// in lastError; a health check would clear that trace by
						// recovering the account. Back off this worker to let Privy
						// recover.
						rateLimitCount.Add(1)
						select {
						case <-time.After(refreshRateLimitBackoff):
						case <-ctx.Done():
						case <-p.stopCh:
						}
					} else {
						failCount.Add(1)
						// Classify the failure to decide whether an async health
						// check is useful (P0-B3). A transient failure (5xx /
						// timeout / network / ambiguous 403) must NOT enqueue a
						// health check: the health check queries the balance with
						// the same (still-valid) token, but if the portal is
						// momentarily broken the check returns 401 → enterCooldown
						// → the 08 avalanche (82 cooldowns). Only a real 401/403
						// (auth-level) is worth a health check, since the account
						// may genuinely be dead.
						authFailure, _ := refreshErrKind(err)
						kind := classifyRefreshFailureKind(err)
						if authFailure {
							p.EnqueueHealthCheck(a)
						}
						failKindsMu.Lock()
						failKinds[kind]++
						failKindsMu.Unlock()
					}
				} else {
					okCount.Add(1)
				}
			}
		}()
	}

	// 4. Feeder: enqueue every snapshot account, interruptible by cancellation.
feed:
	for _, a := range targets {
		select {
		case jobs <- a:
		case <-ctx.Done():
			break feed
		case <-p.stopCh:
			break feed
		}
	}
	close(jobs)
	wg.Wait() // synchronize until the whole round finishes; runRefreshRound semantics unchanged

	ok, fail, rateLimited := int(okCount.Load()), int(failCount.Load()), int(rateLimitCount.Load())
	dist := failKindDistribution(failKinds, &failKindsMu)
	if ctx.Err() != nil {
		log.Printf("refresh round: aborting (ok=%d fail=%d rate_limited=%d remaining=%d%s): %v", ok, fail, rateLimited, len(targets)-ok-fail-rateLimited, dist, ctx.Err())
	} else {
		log.Printf("refresh round: ok=%d fail=%d rate_limited=%d total=%d%s", ok, fail, rateLimited, len(targets), dist)
	}
	return RefreshStats{OK: ok, Fail: fail, RateLimited: rateLimited}
}

// isRateLimited reports whether err is a Privy 429 (rate limit). Refresh rounds
// treat it differently from an auth failure: a rate-limited account is healthy,
// so its failure must not escalate toward the disable limit (SPEC refresh-loop
// §2.2).
func isRateLimited(err error) bool {
	var apiErr *upstream.APIError
	return errors.As(err, &apiErr) && apiErr.Kind == upstream.ErrRateLimited
}

// classifyRefreshFailureKind returns a compact, leak-free label for a refresh
// failure so the round log can aggregate the error-type distribution without
// printing any account identifier or token (P1-B5). The label is one of:
// "401", "403", "429", "5xx", "timeout", "network", "unknown".
func classifyRefreshFailureKind(err error) string {
	if err == nil {
		return "unknown"
	}
	apiErr, ok := err.(*upstream.APIError)
	if !ok {
		// Raw transport error: distinguish timeout from other network errors so
		// the 08-style "6× timeout" is visible in the distribution.
		if errors.Is(err, context.DeadlineExceeded) {
			return "timeout"
		}
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline") {
			return "timeout"
		}
		return "network"
	}
	switch apiErr.Kind {
	case upstream.ErrUnauthorized:
		return "401"
	case upstream.ErrForbidden:
		return "403"
	case upstream.ErrRateLimited:
		return "429"
	case upstream.ErrUpstream:
		return "5xx"
	default:
		return "unknown"
	}
}

// failKindDistribution formats the failKinds map as " fail{401:N,5xx:N,...}"
// for the round log (P1-B5). Returns "" when there are no failures so a clean
// round stays uncluttered. The map is read under failKindsMu.
func failKindDistribution(kinds map[string]int, mu *sync.Mutex) string {
	mu.Lock()
	defer mu.Unlock()
	if len(kinds) == 0 {
		return ""
	}
	// Stable, human-readable order.
	order := []string{"401", "403", "429", "5xx", "timeout", "network", "unknown"}
	var parts []string
	seen := make(map[string]bool)
	for _, k := range order {
		if n, ok := kinds[k]; ok && n > 0 {
			parts = append(parts, fmt.Sprintf("%s:%d", k, n))
			seen[k] = true
		}
	}
	for k, n := range kinds {
		if !seen[k] && n > 0 {
			parts = append(parts, fmt.Sprintf("%s:%d", k, n))
		}
	}
	return " fail{" + strings.Join(parts, ",") + "}"
}

// refreshLoop is the background daily keep-alive: it periodically refreshes
// every active account's tokens so the opaque Privy refresh_token stays fresh,
// and discovers dead accounts before they are picked (SPEC refresh-loop §3.1).
func (p *Pool) refreshLoop() {
	defer p.wg.Done()
	t := time.NewTicker(p.refreshInterval)
	defer t.Stop()
	// Run one round shortly after startup, then every interval: with a fresh
	// 24h cadence a restart would otherwise defer keep-alive by a full day.
	// Background rounds go through the same CAS guard as the manual entry so
	// they can never overlap a manual POST /api/accounts/refresh-all (P1-5).
	p.runRefreshRoundGuarded(context.Background())
	for {
		select {
		case <-t.C:
			p.runRefreshRoundGuarded(context.Background())
		case <-p.stopCh:
			return
		}
	}
}

// runRefreshRoundGuarded runs one refresh round under the round-level CAS guard
// (refreshRunning), shared with the manual RefreshAll entry, so background and
// manual rounds never overlap (P1-5). If a round is already in progress the
// tick is skipped without waiting.
func (p *Pool) runRefreshRoundGuarded(ctx context.Context) {
	if !p.refreshRunning.CompareAndSwap(false, true) {
		log.Printf("refresh round: skipped (another round in progress)")
		return
	}
	defer p.refreshRunning.Store(false)
	p.runRefreshRound(ctx)
}

// noteFailure records the time of an account's failure for the short-term Pick
// cooldown (P0-2). It takes p.mu and must be called WITHOUT holding the
// account's a.mu so the lock order stays p.mu→a.mu (Pick holds p.mu then takes
// a.mu via isHealthy).
func (p *Pool) noteFailure(email string) {
	if email == "" {
		return
	}
	p.mu.Lock()
	if p.recentFails == nil {
		p.recentFails = make(map[string]time.Time)
	}
	p.recentFails[email] = time.Now()
	p.mu.Unlock()
}

// noteSuccess clears the short-term failure cooldown for an account (P0-2). Like
// noteFailure it takes p.mu and must be called after releasing a.mu.
func (p *Pool) noteSuccess(email string) {
	if email == "" {
		return
	}
	p.mu.Lock()
	delete(p.recentFails, email)
	p.mu.Unlock()
}

// recentlyFailed reports whether the account failed within the last pickCooldown
// and is therefore excluded from Pick. The caller must hold p.mu; the read is
// lock-free because recentFails is only mutated under p.mu.
func (p *Pool) recentlyFailed(email string) bool {
	t, ok := p.recentFails[email]
	if !ok {
		return false
	}
	// A stale entry is lazily ignored (not removed here: Pick may hold p.mu and
	// a concurrent recordFailure/noteSuccess would race the delete). Expired
	// entries are dropped on the next noteFailure/noteSuccess/Delete.
	return time.Since(t) < pickCooldown
}

// Pick selects an account for a conversation. Same conversation_id sticks to
// the same account when it is healthy (SPEC 3.3); otherwise the healthy
// account used least recently is chosen. Accounts that failed within the last
// pickCooldown (30s) are skipped so the retry loop rotates away from a
// just-failed account (P0-2); if every candidate is cooling down, the pool
// falls back to the plain LRU pick rather than returning nil. Returns nil only
// when there are no healthy accounts at all.
func (p *Pool) Pick(conversationID string) *Account {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.accounts) == 0 {
		return nil
	}
	// Sticky: hash conversation to a fixed position. The sticky account is
	// skipped when it is unhealthy or cooling down (P0-2).
	if conversationID != "" {
		h := fnv.New32a()
		_, _ = h.Write([]byte(conversationID))
		idx := int(h.Sum32()) % len(p.accounts)
		if a := p.accounts[idx]; a.isHealthy() && !p.recentlyFailed(a.Email) {
			return a
		}
	}
	// Least-recently-used fallback over healthy, not-recently-failed accounts.
	// lastUsedAt is written under the account lock (recordSuccess/recordFailure),
	// so read it under the same lock (2-1). If every healthy account is cooling
	// down, fall back to the plain LRU pick so the request still gets an account.
	var best *Account
	var bestCooling *Account
	for _, a := range p.accounts {
		if !a.isHealthy() {
			continue
		}
		a.mu.Lock()
		older := best == nil || a.lastUsedAt.Before(best.lastUsedAt)
		coolingOlder := bestCooling == nil || a.lastUsedAt.Before(bestCooling.lastUsedAt)
		a.mu.Unlock()
		if !p.recentlyFailed(a.Email) && older {
			best = a
		}
		if coolingOlder {
			bestCooling = a
		}
	}
	if best == nil {
		return bestCooling
	}
	return best
}

// persistAccount writes the account's current state to SQLite. Failures are
// logged and swallowed: a failed persist must not break a live request.
// Deleted accounts are never persisted again (no resurrection): the deleted
// flag is re-checked inside the store lock path after snapshot, so a
// concurrent RemoveAccount cannot have a stale snapshot re-inserted after the
// DELETE (TOCTOU, P2-3).
func (p *Pool) persistAccount(a *Account) {
	if p.store == nil || a == nil || a.isDeleted() {
		return
	}
	row := a.snapshot()
	// Re-check after snapshotting: if the account was physically removed while
	// snapshot ran, skip the write so the store delete cannot be undone by the
	// upsert below (P2-3).
	if a.isDeleted() {
		return
	}
	if err := p.store.saveAccount(row); err != nil {
		log.Printf("pool: persist %s failed: %v", AccountIdentity(a.Email), err)
	}
}

// EnqueueHealthCheck queues an account for the async health-check worker
// (SPEC 3.3). Requeueing is harmless: the worker skips deleted accounts and
// enforces a per-account cooldown.
func (p *Pool) EnqueueHealthCheck(a *Account) {
	if a == nil {
		return
	}
	select {
	case p.healthCh <- a:
	default:
	}
}

// RemoveAccount physically deletes an account: the SQLite row and the in-memory
// entry are both removed. No 'removed' record is kept in the pool, so a later
// re-upload resurrects the account (SPEC-upload §2.2).
func (p *Pool) RemoveAccount(a *Account, reason string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.deleted = true
	a.mu.Unlock()
	p.mu.Lock()
	for i, x := range p.accounts {
		if x == a {
			p.accounts = append(p.accounts[:i], p.accounts[i+1:]...)
			break
		}
	}
	delete(p.recentFails, a.Email)
	p.mu.Unlock()
	if err := p.store.deleteAccount(a.Email); err != nil {
		log.Printf("pool: delete %s from store failed: %v", AccountIdentity(a.Email), err)
	}
	log.Printf("pool: account %s removed (%s)", AccountIdentity(a.Email), reason)
}

// ---- background workers ----

func (p *Pool) healthLoop() {
	defer p.wg.Done()
	for {
		select {
		case a := <-p.healthCh:
			p.healthCheck(a)
		case <-p.stopCh:
			return
		}
	}
}

// healthCheck verifies one account's balance asynchronously: credits > 0 keeps
// the account, zero credits parks it in cooldown (not deleted), and a failed
// query triggers one token refresh before a cooldown decision (SPEC 3.3 +
// SPEC-cooldown §2.2). Balance queries are retried once on network / 5xx
// errors (SPEC-cooldown §2.4).
func (p *Pool) healthCheck(a *Account) {
	a.mu.Lock()
	if a.deleted {
		a.mu.Unlock()
		return
	}
	a.mu.Unlock()

	if !p.healthCooldownOK(a.Email) {
		return
	}
	p.markHealthChecked(a.Email)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	bal, err := p.getBalanceWithRetry(ctx, a.IdentityTokenValue())
	if err != nil {
		// Query failed: try one token refresh, then re-query once more.
		if rerr := p.refresh(ctx, a); rerr == nil {
			bal, err = p.getBalanceWithRetry(ctx, a.IdentityTokenValue())
		}
		if err != nil {
			// Token may be temporarily invalid; cooldown (not delete) so the
			// patrol re-checks it later.
			a.enterCooldown("health check failed: " + truncate(err.Error(), 120))
			return
		}
	}
	if bal.AvailableCredits > 0 {
		p.recoverAccount(a, bal.AvailableCredits)
		return
	}
	a.enterCooldown("no credits")
}

// recoverAccount re-enables an account whose balance is verified positive,
// unless it was manually disabled.
func (p *Pool) recoverAccount(a *Account, credits int) {
	a.mu.Lock()
	if a.manualDisabled {
		a.mu.Unlock()
		return
	}
	a.Status = StatusActive
	a.AvailableCredits = credits
	a.failCount = 0
	a.consecutiveAuthFailures = 0
	a.lastError = ""
	a.mu.Unlock()
	p.persistAccount(a)
	log.Printf("pool: account %s healthy, credits=%d", AccountIdentity(a.Email), credits)
}

// CooldownStats reports one cooldown round's outcome (SPEC-cooldown §2.3).
type CooldownStats struct {
	Restored int  `json:"restored"`
	Still    int  `json:"still"`
	Total    int  `json:"total"`
	Busy     bool `json:"busy,omitempty"`
}

// CheckCooldowns manually triggers one cooldown round over every cooldown
// account. If another round (background loop or a previous manual call) is
// still running, it returns Busy=true immediately and does not start a second
// one. It shares the implementation with the background loop via
// runCooldownRound.
func (p *Pool) CheckCooldowns(ctx context.Context) CooldownStats {
	if !p.cooldownRunning.CompareAndSwap(false, true) {
		return CooldownStats{Busy: true}
	}
	defer p.cooldownRunning.Store(false)
	return p.runCooldownRound(ctx)
}

// runCooldownRound walks every StatusCooldown account, re-queries its real
// balance (with one retry), and restores it to active when credits > 0. It is
// shared by the background cooldownLoop and the manual POST
// /api/accounts/cooldowns-check handler. A 10-minute round timeout and a
// per-account 50ms delay keep the round bounded (SPEC-cooldown §2.3).
func (p *Pool) runCooldownRound(ctx context.Context) CooldownStats {
	ctx, cancel := context.WithTimeout(ctx, refreshRoundTimeout)
	defer cancel()

	p.mu.RLock()
	targets := make([]*Account, 0, len(p.accounts))
	for _, a := range p.accounts {
		if a.Status == StatusCooldown {
			targets = append(targets, a)
		}
	}
	p.mu.RUnlock()

	var st CooldownStats
	st.Total = len(targets)
	for _, a := range targets {
		// The account may have been deleted or moved while queued; skip it.
		if a.StatusValue() != StatusCooldown || a.isDeleted() {
			continue
		}
		if ctx.Err() != nil {
			log.Printf("cooldown round: aborting (restored=%d still=%d): %v", st.Restored, st.Still, ctx.Err())
			break
		}
		bal, err := p.getBalanceWithRetry(ctx, a.IdentityTokenValue())
		if err == nil && bal.AvailableCredits > 0 {
			p.recoverAccount(a, bal.AvailableCredits)
			st.Restored++
		} else {
			// Still zero (or unqueryable): keep it in cooldown for the next
			// round rather than churning its state.
			st.Still++
		}
		select {
		case <-time.After(cooldownAccountDelay):
		case <-ctx.Done():
		case <-p.stopCh:
			// Pool is closing (SIGTERM): exit the round early instead of
			// blocking Close() (P1-8).
			log.Printf("cooldown round: aborted on shutdown (restored=%d still=%d total=%d)", st.Restored, st.Still, st.Total)
			return st
		}
	}
	log.Printf("cooldown round: restored=%d still=%d total=%d", st.Restored, st.Still, st.Total)
	return st
}

// cooldownLoop is the background patrol that periodically re-checks cooldown
// accounts for the monthly credit replenishment (SPEC-cooldown §2.3). It runs
// one round shortly after startup, then every CooldownInterval.
func (p *Pool) cooldownLoop() {
	defer p.wg.Done()
	t := time.NewTicker(p.cooldownInterval)
	defer t.Stop()
	// Background rounds go through the same CAS guard as the manual entry so
	// they can never overlap a manual POST /api/accounts/cooldowns-check (P1-5).
	p.runCooldownRoundGuarded(context.Background())
	for {
		select {
		case <-t.C:
			p.runCooldownRoundGuarded(context.Background())
		case <-p.stopCh:
			return
		}
	}
}

// runCooldownRoundGuarded runs one cooldown round under the round-level CAS
// guard (cooldownRunning), shared with the manual CheckCooldowns entry, so
// background and manual rounds never overlap (P1-5).
func (p *Pool) runCooldownRoundGuarded(ctx context.Context) {
	if !p.cooldownRunning.CompareAndSwap(false, true) {
		log.Printf("cooldown round: skipped (another round in progress)")
		return
	}
	defer p.cooldownRunning.Store(false)
	p.runCooldownRound(ctx)
}

// getBalanceWithRetry fetches the balance, retrying once after 1s on a
// transient failure (network error or 5xx) before giving up (SPEC-cooldown
// §2.4).
func (p *Pool) getBalanceWithRetry(ctx context.Context, identityToken string) (*upstream.Balance, error) {
	bal, err := p.client.GetBalance(ctx, identityToken)
	if err == nil || !isTransientBalanceErr(err) {
		return bal, err
	}
	select {
	case <-time.After(balanceRetryDelay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	bal, err = p.client.GetBalance(ctx, identityToken)
	if err == nil || !isTransientBalanceErr(err) {
		return bal, err
	}
	return nil, err
}

// isTransientBalanceErr reports whether a balance query failure is worth one
// retry: network errors and upstream 5xx (SPEC-cooldown §2.4). Auth-level
// failures (401/403/429/402) are deterministic — retrying would not help.
func isTransientBalanceErr(err error) bool {
	apiErr, ok := err.(*upstream.APIError)
	if !ok {
		return true // raw network / transport error
	}
	return apiErr.StatusCode >= 500
}

func (p *Pool) healthCooldownOK(email string) bool {
	p.cooldownMu.Lock()
	defer p.cooldownMu.Unlock()
	last, ok := p.cooldowns[email]
	if ok && time.Since(last) < healthCheckCooldown {
		return false
	}
	return true
}

func (p *Pool) markHealthChecked(email string) {
	p.cooldownMu.Lock()
	p.cooldowns[email] = time.Now()
	p.cooldownMu.Unlock()
}

// AccountIdentity returns a short label for logging without leaking the token.
func AccountIdentity(email string) string {
	if i := strings.Index(email, "@"); i > 0 {
		return email[:i] + "@..."
	}
	return "account"
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

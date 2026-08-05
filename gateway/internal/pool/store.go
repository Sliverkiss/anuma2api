package pool

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no cgo; Docker CGO_ENABLED=0)
)

// Store is the SQLite persistence layer for the account pool. It serializes
// all writes behind a single mutex so concurrent goroutines (requests, health
// worker, uploads) cannot interleave transactions on the same account.
//
// SQLite holds the authoritative runtime state (credits, failure counts,
// status, refreshed tokens) and survives restarts. The gateway no longer reads
// accounts.csv (SPEC-upload §2.3): the CSV is a backup source replayed to the
// gateway by register.py --import-csv via POST /api/accounts.
type Store struct {
	db *sql.DB
	mu sync.Mutex
}

// openStore opens (creating if needed) the SQLite database at path and ensures
// the accounts table exists. busy_timeout avoids SQLITE_BUSY under concurrent
// access; WAL improves read/write concurrency.
func openStore(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// modernc.org/sqlite opens lazily; force a connection so schema errors
	// surface at startup rather than on first request.
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS accounts (
	email                        TEXT PRIMARY KEY,
	wallet_address               TEXT,
	wallet_id                    TEXT,
	user_id                      TEXT,
	tier                         TEXT,
	identity_token               TEXT,
	access_token                 TEXT,
	refresh_token                TEXT,
	credits                      INTEGER,
	status                       TEXT DEFAULT 'active',
	fail_count                   INTEGER DEFAULT 0,
	consecutive_auth_failures    INTEGER DEFAULT 0,
	last_error                   TEXT,
	last_used_at                 INTEGER,
	created_at                   INTEGER,
	removed_at                   INTEGER,
	expires_at                   INTEGER
);`

func (s *Store) migrate() error {
	_, err := s.db.Exec(schema)
	return err
}

func (s *Store) Close() error {
	return s.db.Close()
}

// upsertAccounts inserts new accounts and fully updates existing ones by email
// (SPEC-upload §2.1: upload is the latest state). It returns the counts of
// added and updated rows. Status is derived from credits (cooldown when 0);
// created_at is only set on insert so runtime age is preserved.
func (s *Store) upsertAccounts(inputs []AccountInput) (added, updated int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()

	insStmt, err := tx.Prepare(`INSERT OR IGNORE INTO accounts
		(email, wallet_address, wallet_id, user_id, tier,
		 identity_token, access_token, refresh_token, credits, status,
		 created_at, expires_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return 0, 0, err
	}
	defer insStmt.Close()

	updStmt, err := tx.Prepare(`UPDATE accounts SET
		wallet_address=?, wallet_id=?, user_id=?, tier=?,
		identity_token=?, access_token=?, refresh_token=?, credits=?,
		status=?, expires_at=?
		WHERE email=?`)
	if err != nil {
		return 0, 0, err
	}
	defer updStmt.Close()

	for _, in := range inputs {
		if strings.TrimSpace(in.Email) == "" || strings.TrimSpace(in.IdentityToken) == "" {
			continue
		}
		status, _ := statusForCredits(in.AvailableCredits)
		res, err := insStmt.Exec(
			in.Email, in.WalletAddress, in.WalletID, in.UserID, in.Tier,
			in.IdentityToken, in.AccessToken, in.RefreshToken, in.AvailableCredits, status,
			now, in.ExpiresAt,
		)
		if err != nil {
			return added, updated, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added++
			continue
		}
		if _, err := updStmt.Exec(
			in.WalletAddress, in.WalletID, in.UserID, in.Tier,
			in.IdentityToken, in.AccessToken, in.RefreshToken, in.AvailableCredits, status,
			in.ExpiresAt, in.Email,
		); err != nil {
			return added, updated, err
		}
		updated++
	}
	if err := tx.Commit(); err != nil {
		return added, updated, err
	}
	return added, updated, nil
}

// deleteAccount physically removes one account row (SPEC-upload §2.2).
func (s *Store) deleteAccount(email string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM accounts WHERE email = ?`, email)
	return err
}

// purgeRemoved deletes legacy rows that still carry status='removed' (written
// before physical deletion, SPEC-upload §2.2). Returns the count purged.
func (s *Store) purgeRemoved() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM accounts WHERE status = 'removed'`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		log.Printf("store: purged %d legacy removed account(s)", n)
	}
	return int(n), nil
}

// loadAccounts reads every account from the database into in-memory Account
// structs, restoring runtime state (status, counters, last_used_at).
func (s *Store) loadAccounts() ([]*Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT
		email, wallet_address, wallet_id, user_id, tier,
		identity_token, access_token, refresh_token, credits, status,
		fail_count, consecutive_auth_failures, last_error, last_used_at,
		created_at, removed_at, expires_at
		FROM accounts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Account
	for rows.Next() {
		var (
			a                                        Account
			walletAddress, walletID, userID, tier    sql.NullString
			identityToken, accessToken, refreshToken sql.NullString
			credits                                  sql.NullInt64
			status                                   sql.NullString
			failCount, consecAuth, lastUsed, created sql.NullInt64
			removedAt, expiresAt                     sql.NullInt64
			lastError                                sql.NullString
		)
		if err := rows.Scan(
			&a.Email, &walletAddress, &walletID, &userID, &tier,
			&identityToken, &accessToken, &refreshToken, &credits, &status,
			&failCount, &consecAuth, &lastError, &lastUsed,
			&created, &removedAt, &expiresAt,
		); err != nil {
			return nil, err
		}
		a.WalletAddress = walletAddress.String
		a.WalletID = walletID.String
		a.UserID = userID.String
		a.SubscriptionTier = tier.String
		a.IdentityToken = identityToken.String
		a.AccessToken = accessToken.String
		a.RefreshToken = refreshToken.String
		a.AvailableCredits = int(credits.Int64)
		a.Status = status.String
		if a.Status == "" {
			a.Status = StatusActive
		}
		a.failCount = int(failCount.Int64)
		a.consecutiveAuthFailures = int(consecAuth.Int64)
		a.lastError = lastError.String
		a.lastUsedAt = time.Unix(lastUsed.Int64, 0)
		a.RemovedAt = removedAt.Int64
		a.ExpiresAt = expiresAt.Int64
		// A manually disabled account must not be resurrected by the async
		// health check; the disable reason records that intent.
		if a.Status == StatusDisabled && a.lastError == manualDisabledReason {
			a.manualDisabled = true
		}
		// Fresh account with zero credits: treat as cooldown (SPEC-cooldown
		// §2.2) until the monthly replenishment restores it, rather than
		// deleting it.
		if a.Status == StatusActive && a.AvailableCredits <= 0 {
			a.Status = StatusCooldown
			a.lastError = "no credits"
		}
		out = append(out, &a)
	}
	return out, rows.Err()
}

// accountRow is a consistent snapshot of an Account for persistence. It is
// captured under the account mutex so the DB write does not race field
// updates.
type accountRow struct {
	Email                   string
	WalletAddress           string
	WalletID                string
	UserID                  string
	Tier                    string
	IdentityToken           string
	AccessToken             string
	RefreshToken            string
	Credits                 int
	Status                  string
	FailCount               int
	ConsecutiveAuthFailures int
	LastError               string
	LastUsedAt              int64
	CreatedAt               int64
	RemovedAt               int64
	ExpiresAt               int64
}

// saveAccount upserts the runtime state of one account (SPEC 3.2). Used for
// credit deductions, failure counters, disable/remove transitions, and
// refreshed tokens.
func (s *Store) saveAccount(row accountRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO accounts
		(email, wallet_address, wallet_id, user_id, tier,
		 identity_token, access_token, refresh_token, credits, status,
		 fail_count, consecutive_auth_failures, last_error, last_used_at,
		 created_at, removed_at, expires_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(email) DO UPDATE SET
			wallet_address=excluded.wallet_address,
			wallet_id=excluded.wallet_id,
			user_id=excluded.user_id,
			tier=excluded.tier,
			identity_token=excluded.identity_token,
			access_token=excluded.access_token,
			refresh_token=excluded.refresh_token,
			credits=excluded.credits,
			status=excluded.status,
			fail_count=excluded.fail_count,
			consecutive_auth_failures=excluded.consecutive_auth_failures,
			last_error=excluded.last_error,
			last_used_at=excluded.last_used_at,
			removed_at=excluded.removed_at,
			expires_at=excluded.expires_at`,
		row.Email, row.WalletAddress, row.WalletID, row.UserID, row.Tier,
		row.IdentityToken, row.AccessToken, row.RefreshToken, row.Credits, row.Status,
		row.FailCount, row.ConsecutiveAuthFailures, row.LastError, row.LastUsedAt,
		row.CreatedAt, row.RemovedAt, row.ExpiresAt,
	)
	if err != nil {
		log.Printf("store: save account %s failed: %v", AccountIdentity(row.Email), err)
	}
	return err
}

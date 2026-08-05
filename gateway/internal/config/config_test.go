package config

import (
	"os"
	"testing"
	"time"
)

func setenv(t *testing.T, key, val string) {
	t.Helper()
	old, existed := os.LookupEnv(key)
	if err := os.Setenv(key, val); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(key, old)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

// TestLoadDefaults verifies the SPEC-hardening defaults.
func TestLoadDefaults(t *testing.T) {
	for _, k := range []string{"ANUMA_DB_PATH", "ANUMA_MAX_RETRIES", "ANUMA_AUTH_FAIL_LIMIT", "ANUMA_MODEL_ALLOWLIST"} {
		_ = os.Unsetenv(k)
	}
	cfg := Load()
	if cfg.DBPath != "./gateway.db" {
		t.Errorf("DBPath = %q, want ./gateway.db", cfg.DBPath)
	}
	if cfg.MaxRetries != 5 {
		t.Errorf("MaxRetries = %d, want 5", cfg.MaxRetries)
	}
	if cfg.AuthFailLimit != 3 {
		t.Errorf("AuthFailLimit = %d, want 3", cfg.AuthFailLimit)
	}
	wantModels := []string{
		"inclusionai/ling-2.6-flash",
		"openai/gpt-5.6-luna",
		"qwen/qwen-3.6-plus",
		"minimax/minimax-m2.5",
		"anthropic/claude-sonnet-5",
		"gemini/gemini-3.1-pro-preview",
		"gemini/gemini-3-flash-preview",
		"grok/grok-4.5",
		"glm/glm-5.2",
		"kimi/kimi-k3",
		"minimax/minimax-m3",
		"qwen/qwen-3.7-plus",
	}
	if len(cfg.ModelAllowlist) != len(wantModels) {
		t.Fatalf("ModelAllowlist len = %d, want %d: %v", len(cfg.ModelAllowlist), len(wantModels), cfg.ModelAllowlist)
	}
	for i, want := range wantModels {
		if cfg.ModelAllowlist[i] != want {
			t.Errorf("ModelAllowlist[%d] = %q, want %q", i, cfg.ModelAllowlist[i], want)
		}
	}
	if cfg.ModelCacheTTL != 600*time.Second {
		t.Errorf("ModelCacheTTL = %v", cfg.ModelCacheTTL)
	}
	if cfg.RefreshInterval != 1800*time.Second {
		t.Errorf("RefreshInterval = %v, want 1800s", cfg.RefreshInterval)
	}
	if cfg.CooldownInterval != 86400*time.Second {
		t.Errorf("CooldownInterval = %v, want 86400s", cfg.CooldownInterval)
	}
	if cfg.RetryBackoff != 300*time.Millisecond {
		t.Errorf("RetryBackoff = %v, want 300ms", cfg.RetryBackoff)
	}
	// Auto-continuation (ticket 13): on by default with a 5-segment cap.
	if !cfg.AutoContinue {
		t.Error("AutoContinue = false, want true (default on)")
	}
	if cfg.AutoContinueMaxSegments != 5 {
		t.Errorf("AutoContinueMaxSegments = %d, want 5", cfg.AutoContinueMaxSegments)
	}
}

// TestAutoContinueEnvOverrides verifies the ticket-13 env vars are honored:
// ANUMA_AUTO_CONTINUE=false disables auto-continuation and
// ANUMA_AUTO_CONTINUE_MAX_SEGMENTS overrides the default cap.
func TestAutoContinueEnvOverrides(t *testing.T) {
	setenv(t, "ANUMA_AUTO_CONTINUE", "false")
	setenv(t, "ANUMA_AUTO_CONTINUE_MAX_SEGMENTS", "8")
	cfg := Load()
	if cfg.AutoContinue {
		t.Error("AutoContinue = true, want false")
	}
	if cfg.AutoContinueMaxSegments != 8 {
		t.Errorf("AutoContinueMaxSegments = %d, want 8", cfg.AutoContinueMaxSegments)
	}
}

// TestCooldownRetryEnvOverrides verifies the SPEC-cooldown env vars are honored.
func TestCooldownRetryEnvOverrides(t *testing.T) {
	setenv(t, "ANUMA_COOLDOWN_CHECK_INTERVAL", "3600")
	setenv(t, "ANUMA_RETRY_BACKOFF_MS", "500")
	cfg := Load()
	if cfg.CooldownInterval != 3600*time.Second {
		t.Errorf("CooldownInterval = %v, want 3600s", cfg.CooldownInterval)
	}
	if cfg.RetryBackoff != 500*time.Millisecond {
		t.Errorf("RetryBackoff = %v, want 500ms", cfg.RetryBackoff)
	}
	// <=0 disables the cooldown loop (SPEC-cooldown §2.3).
	setenv(t, "ANUMA_COOLDOWN_CHECK_INTERVAL", "0")
	cfg = Load()
	if cfg.CooldownInterval != 0 {
		t.Errorf("CooldownInterval = %v, want 0 (disabled)", cfg.CooldownInterval)
	}
}

// TestLoadEnvOverrides verifies the new env vars are honored.
func TestLoadEnvOverrides(t *testing.T) {
	setenv(t, "ANUMA_DB_PATH", "/tmp/x.db")
	setenv(t, "ANUMA_MAX_RETRIES", "5")
	setenv(t, "ANUMA_AUTH_FAIL_LIMIT", "7")
	setenv(t, "ANUMA_MODEL_ALLOWLIST", "a/b, c/d ,e/f")
	setenv(t, "ANUMA_REFRESH_INTERVAL", "7200")
	cfg := Load()
	if cfg.DBPath != "/tmp/x.db" {
		t.Errorf("DBPath = %q", cfg.DBPath)
	}
	if cfg.MaxRetries != 5 {
		t.Errorf("MaxRetries = %d", cfg.MaxRetries)
	}
	if cfg.AuthFailLimit != 7 {
		t.Errorf("AuthFailLimit = %d", cfg.AuthFailLimit)
	}
	if cfg.RefreshInterval != 7200*time.Second {
		t.Errorf("RefreshInterval = %v, want 7200s", cfg.RefreshInterval)
	}
	// <=0 disables the background refresh loop (SPEC refresh-loop §3.2).
	setenv(t, "ANUMA_REFRESH_INTERVAL", "0")
	cfg = Load()
	if cfg.RefreshInterval != 0 {
		t.Errorf("RefreshInterval = %v, want 0 (disabled)", cfg.RefreshInterval)
	}
	if len(cfg.ModelAllowlist) != 3 || cfg.ModelAllowlist[0] != "a/b" || cfg.ModelAllowlist[1] != "c/d" || cfg.ModelAllowlist[2] != "e/f" {
		t.Errorf("ModelAllowlist = %v", cfg.ModelAllowlist)
	}
}

// TestSplitList trims whitespace and drops empties.
func TestSplitList(t *testing.T) {
	got := splitList(" a,  ,b,,c ")
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("splitList = %v", got)
	}
	if len(splitList("")) != 0 {
		t.Errorf("splitList('') should be empty")
	}
}

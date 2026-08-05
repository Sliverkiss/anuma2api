// Package config loads gateway configuration from environment variables.
package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all runtime configuration for the gateway.
type Config struct {
	// Port is the HTTP listen address port (ANUMA_PORT, default 7895).
	Port string
	// DBPath is the SQLite database path (ANUMA_DB_PATH, default ./gateway.db).
	DBPath string
	// ModelCacheTTL is how long curated-models is cached (ANUMA_MODEL_CACHE_TTL, seconds, default 600).
	ModelCacheTTL time.Duration
	// UpstreamBaseURL is the portal base URL (ANUMA_UPSTREAM_BASE_URL, default https://portal.anuma.ai/api/v1).
	UpstreamBaseURL string
	// PrivyBaseURL is the privy auth base URL (ANUMA_PRIVY_BASE_URL, default https://auth.privy.io/api/v1).
	PrivyBaseURL string
	// AdminPassword is the Bearer token required for /api/* and /v1/*
	// (ANUMA_ADMIN_PASSWORD, default "YOUR_ADMIN_PASSWORD").
	AdminPassword string
	// MaxRetries is the number of account switches/retries per request
	// (ANUMA_MAX_RETRIES, default 3).
	MaxRetries int
	// AuthFailLimit is the consecutive auth-failure count that marks an account
	// disabled (ANUMA_AUTH_FAIL_LIMIT, default 3).
	AuthFailLimit int
	// ModelAllowlist is the set of model IDs /v1/models returns and
	// /v1/chat/completions accepts (ANUMA_MODEL_ALLOWLIST; default is the
	// 12-model list from SPEC system-inject §2.2: 4 basic models plus the
	// 8 system-prompt-unlocked advanced models).
	ModelAllowlist []string
	// RefreshInterval is how often the background token keep-alive round runs
	// (ANUMA_REFRESH_INTERVAL, seconds, default 86400). <=0 disables the
	// refresh loop (SPEC refresh-loop §3.2).
	RefreshInterval time.Duration
	// CooldownInterval is how often the background cooldown patrol re-checks
	// zero-credit accounts for monthly replenishment (ANUMA_COOLDOWN_CHECK_INTERVAL,
	// seconds, default 86400). <=0 disables the cooldown loop (SPEC-cooldown §2.3).
	CooldownInterval time.Duration
	// RetryBackoff is the base delay between request retries after a failed
	// account attempt (ANUMA_RETRY_BACKOFF_MS, milliseconds, default 200);
	// the sleep is backoff * attempt, capped at 2s (SPEC-cooldown §2.4).
	RetryBackoff time.Duration
	// AutoContinue enables gateway-side auto-continuation (ticket 13): when a
	// response is truncated at the model's output-token cap (finish_reason=
	// length), the gateway re-issues the request with the accumulated text
	// appended to the history until the model stops naturally or the segment
	// cap is reached (ANUMA_AUTO_CONTINUE, default true).
	AutoContinue bool
	// AutoContinueMaxSegments is the maximum number of segments per request,
	// including the first, before the gateway gives up and returns whatever was
	// accumulated (still marked finish_reason=length) (ANUMA_AUTO_CONTINUE_MAX_SEGMENTS,
	// default 5). <=0 disables auto-continuation.
	AutoContinueMaxSegments int
}

// Load reads configuration from the environment, falling back to SPEC defaults.
func Load() *Config {
	cfg := &Config{
		Port:            getenv("ANUMA_PORT", "7895"),
		DBPath:          getenv("ANUMA_DB_PATH", "./gateway.db"),
		ModelCacheTTL:   600 * time.Second,
		UpstreamBaseURL: getenv("ANUMA_UPSTREAM_BASE_URL", "https://portal.anuma.ai/api/v1"),
		PrivyBaseURL:    getenv("ANUMA_PRIVY_BASE_URL", "https://auth.privy.io/api/v1"),
		AdminPassword:   getenv("ANUMA_ADMIN_PASSWORD", "YOUR_ADMIN_PASSWORD"),
		MaxRetries:      getenvInt("ANUMA_MAX_RETRIES", 5),
		AuthFailLimit:   getenvInt("ANUMA_AUTH_FAIL_LIMIT", 3),
		// 12 models实测可用（head9 注入解锁）。以下 5 个模型即使完整 system 也 403
		// （真 tier gate，账号级 gate 无法解锁）—— 明确排除，勿加回白名单
		// (SPEC-cooldown §1.1): anthropic/claude-opus-5, claude-fable-5,
		// openai/gpt-5.6-sol, gpt-5.6-terra, gpt-5.5
		ModelAllowlist:   splitList(getenv("ANUMA_MODEL_ALLOWLIST", "inclusionai/ling-2.6-flash,openai/gpt-5.6-luna,qwen/qwen-3.6-plus,minimax/minimax-m2.5,anthropic/claude-sonnet-5,gemini/gemini-3.1-pro-preview,gemini/gemini-3-flash-preview,grok/grok-4.5,glm/glm-5.2,kimi/kimi-k3,minimax/minimax-m3,qwen/qwen-3.7-plus")),
		RefreshInterval:  time.Duration(getenvInt("ANUMA_REFRESH_INTERVAL", 1800)) * time.Second,
		CooldownInterval: time.Duration(getenvInt("ANUMA_COOLDOWN_CHECK_INTERVAL", 86400)) * time.Second,
		RetryBackoff:     time.Duration(getenvInt("ANUMA_RETRY_BACKOFF_MS", 300)) * time.Millisecond,
		// Auto-continuation (ticket 13): on by default so length-truncated
		// answers are replayed automatically; ANUMA_AUTO_CONTINUE_MAX_SEGMENTS<=0
		// turns it off.
		AutoContinue:            getenvBool("ANUMA_AUTO_CONTINUE", true),
		AutoContinueMaxSegments: getenvInt("ANUMA_AUTO_CONTINUE_MAX_SEGMENTS", 5),
	}
	if len(cfg.ModelAllowlist) == 0 {
		// 与上面默认值一致：12 个实测可用模型。5 个 tier-gate 模型
		// (anthropic/claude-opus-5, claude-fable-5, openai/gpt-5.6-sol,
		// gpt-5.6-terra, gpt-5.5) 勿加入，/v1/models 也不应显示。
		cfg.ModelAllowlist = []string{
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
	}
	if ttl := getenvInt("ANUMA_MODEL_CACHE_TTL", 600); ttl > 0 {
		cfg.ModelCacheTTL = time.Duration(ttl) * time.Second
	}
	return cfg
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func getenvBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

// splitList parses a comma-separated list, trimming whitespace and dropping
// empty entries.
func splitList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

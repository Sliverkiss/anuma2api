package convert

import "strings"

// aliasTable is the source of truth for the 12 whitelisted models: each entry
// maps the externally-displayed short name (no provider prefix) to the upstream
// long name (provider/name). The allowlist in config keeps only the long names;
// clients talk to the gateway exclusively in short names (SPEC model-alias §2).
var aliasTable = []struct{ short, long string }{
	{"ling-2.6-flash", "inclusionai/ling-2.6-flash"},
	{"kimi-k3", "kimi/kimi-k3"},
	{"gpt-5.6-luna", "openai/gpt-5.6-luna"},
	{"qwen-3.7-plus", "qwen/qwen-3.7-plus"},
	{"qwen-3.6-plus", "qwen/qwen-3.6-plus"},
	{"glm-5.2", "glm/glm-5.2"},
	{"minimax-m3", "minimax/minimax-m3"},
	{"minimax-m2.5", "minimax/minimax-m2.5"},
	{"claude-sonnet-5", "anthropic/claude-sonnet-5"},
	{"gemini-3.1-pro-preview", "gemini/gemini-3.1-pro-preview"},
	{"gemini-3-flash-preview", "gemini/gemini-3-flash-preview"},
	{"grok-4.5", "grok/grok-4.5"},
}

// upstreamByInput accepts both the short name and the long name of every
// whitelisted model and resolves either to the upstream long name used for
// forwarding. Unknown models are absent from the map.
var upstreamByInput = buildUpstreamByInput()

func buildUpstreamByInput() map[string]string {
	m := make(map[string]string, len(aliasTable)*2)
	for _, a := range aliasTable {
		m[a.short] = a.long
		m[a.long] = a.long
	}
	return m
}

// DisplayName converts an upstream long id to the externally-shown short name:
// it strips the provider prefix up to and including the first "/"; an id with
// no "/" is returned unchanged. For the 12 whitelisted models this is exactly
// the short name column of aliasTable.
func DisplayName(upstreamID string) string {
	if i := strings.Index(upstreamID, "/"); i >= 0 {
		return upstreamID[i+1:]
	}
	return upstreamID
}

// ResolveUpstream maps an inbound model id (short or long name) to the upstream
// long name used for forwarding. Lookup is bidirectional within the 12
// whitelisted models; any unknown model is returned unchanged so the allowlist
// check that runs after resolution rejects it (SPEC model-alias §3.3).
//
// It is idempotent with DisplayName: ResolveUpstream(DisplayName(x)) == x for
// every whitelisted upstream id x.
func ResolveUpstream(input string) string {
	if long, ok := upstreamByInput[input]; ok {
		return long
	}
	return input
}

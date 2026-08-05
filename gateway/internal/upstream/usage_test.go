package upstream

import "testing"

// TestUsageNormalized covers the three states of the token normalization:
// prompt_tokens present wins, input_tokens is the fallback (streamed
// response.completed naming), and neither present yields 0.
func TestUsageNormalized(t *testing.T) {
	cases := []struct {
		name       string
		u          Usage
		wantPrompt int
		wantComp   int
	}{
		{
			name:       "prompt_tokens preferred",
			u:          Usage{PromptTokens: 2563, CompletionTokens: 17, InputTokens: 999, OutputTokens: 999},
			wantPrompt: 2563,
			wantComp:   17,
		},
		{
			name:       "input_tokens fallback (streaming naming)",
			u:          Usage{InputTokens: 2563, OutputTokens: 17, TotalTokens: 2580},
			wantPrompt: 2563,
			wantComp:   17,
		},
		{
			name:       "input_tokens fallback with zero prompt_tokens",
			u:          Usage{PromptTokens: 0, CompletionTokens: 0, InputTokens: 2563, OutputTokens: 17},
			wantPrompt: 2563,
			wantComp:   17,
		},
		{
			name:       "both absent -> zero",
			u:          Usage{TotalTokens: 2580},
			wantPrompt: 0,
			wantComp:   0,
		},
		{
			name:       "empty usage -> zero",
			u:          Usage{},
			wantPrompt: 0,
			wantComp:   0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, co := c.u.Normalized()
			if p != c.wantPrompt || co != c.wantComp {
				t.Errorf("Normalized() = (%d,%d), want (%d,%d)", p, co, c.wantPrompt, c.wantComp)
			}
		})
	}
}

// TestUsageNormalizedNil verifies the nil receiver returns zeros.
func TestUsageNormalizedNil(t *testing.T) {
	var u *Usage
	p, co := u.Normalized()
	if p != 0 || co != 0 {
		t.Errorf("nil Normalized() = (%d,%d), want (0,0)", p, co)
	}
}

// TestUsageCachedTokens verifies the cached-token lookup prefers
// prompt_tokens_details and falls back to input_tokens_details (the streamed
// Responses-style naming); absent details yield 0.
func TestUsageCachedTokens(t *testing.T) {
	cases := []struct {
		name string
		u    Usage
		want int
	}{
		{
			name: "prompt_tokens_details preferred",
			u: Usage{
				PromptTokensDetails: &TokensDetails{CachedTokens: 120},
				InputTokensDetails:  &TokensDetails{CachedTokens: 999},
			},
			want: 120,
		},
		{
			name: "input_tokens_details fallback (streaming naming)",
			u: Usage{
				InputTokensDetails: &TokensDetails{CachedTokens: 88},
			},
			want: 88,
		},
		{
			name: "absent details -> zero",
			u:    Usage{},
			want: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.u.CachedTokens(); got != c.want {
				t.Errorf("CachedTokens() = %d, want %d", got, c.want)
			}
		})
	}
}

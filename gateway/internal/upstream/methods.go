package upstream

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// Models returns the curated model list, cached for the configured TTL.
// Only models with active=true are returned (SPEC 5.1).
func (c *Client) Models(ctx context.Context) ([]Model, error) {
	c.modelMu.Lock()
	if len(c.models) > 0 && time.Since(c.modelsAt) < c.modelsTTL {
		out := append([]Model(nil), c.models...)
		c.modelMu.Unlock()
		return out, nil
	}
	c.modelMu.Unlock()

	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/curated-models", nil)
	if err != nil {
		return nil, err
	}
	req.Header = c.baseHeaders()
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, classifyError(resp.StatusCode, parseErrorBody(resp.StatusCode, body), "")
	}
	var envelope struct {
		Models []Model `json:"models"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, &APIError{Kind: ErrUpstream, StatusCode: resp.StatusCode, Message: "decode curated-models: " + err.Error()}
	}
	active := make([]Model, 0, len(envelope.Models))
	for _, m := range envelope.Models {
		if m.Active {
			active = append(active, m)
		}
	}
	c.modelMu.Lock()
	c.models = append([]Model(nil), active...)
	c.modelsAt = time.Now()
	c.modelMu.Unlock()
	return active, nil
}

// GetBalance fetches the credit balance for an account.
func (c *Client) GetBalance(ctx context.Context, identityToken string) (*Balance, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/credits/balance", nil)
	if err != nil {
		return nil, err
	}
	h := c.baseHeaders()
	h.Set("Authorization", "Bearer "+identityToken)
	req.Header = h
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, classifyError(resp.StatusCode, parseErrorBody(resp.StatusCode, body), "")
	}
	var b Balance
	if err := json.Unmarshal(body, &b); err != nil {
		return nil, &APIError{Kind: ErrUpstream, StatusCode: resp.StatusCode, Message: "decode balance: " + err.Error()}
	}
	return &b, nil
}

// RefreshResult is the parsed privy session response (SPEC 3.7).
type RefreshResult struct {
	IdentityToken    string `json:"identity_token"`
	PrivyAccessToken string `json:"privy_access_token"`
	RefreshToken     string `json:"refresh_token"`
}

// RefreshToken exchanges a refresh_token for a fresh identity_token.
func (c *Client) RefreshToken(ctx context.Context, privyAccessToken, refreshToken string) (*RefreshResult, error) {
	payload, _ := json.Marshal(map[string]string{"refresh_token": refreshToken})
	req, err := http.NewRequestWithContext(ctx, "POST", c.privyURL+"/sessions", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	h := c.baseHeaders()
	h.Set("Authorization", "Bearer "+privyAccessToken)
	req.Header = h
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, classifyError(resp.StatusCode, parseErrorBody(resp.StatusCode, b), "")
	}
	var out RefreshResult
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, &APIError{Kind: ErrUnknown, StatusCode: resp.StatusCode, Message: "decode refresh: " + err.Error()}
	}
	if out.IdentityToken == "" {
		return nil, &APIError{Kind: ErrUnknown, StatusCode: resp.StatusCode, Message: "refresh response missing identity_token"}
	}
	return &out, nil
}

// JWTExpiry parses the exp claim from a JWT payload segment. Returns 0 if it
// cannot be parsed.
func JWTExpiry(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return 0
	}
	seg := parts[1]
	if rem := len(seg) % 4; rem != 0 {
		seg += strings.Repeat("=", 4-rem)
	}
	decoded, err := base64.URLEncoding.DecodeString(seg)
	if err != nil {
		return 0
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(decoded, &claims); err != nil {
		return 0
	}
	return claims.Exp
}

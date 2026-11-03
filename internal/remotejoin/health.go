package remotejoin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/yetone/magpie/internal/provider"
)

// HealthResult contains classification only, never the gateway's active requests.
type HealthResult struct {
	Healthy      bool
	Reachable    bool
	Unauthorized bool
	Code         string
}

func (c *Client) probeHealth(ctx context.Context, s ClientStatus) HealthResult {
	fail := func(code string) HealthResult { return HealthResult{Code: code} }
	root, err := RootOrigin(s.ServerURL)
	if err != nil || root != s.ServerURL {
		return fail("health_request_invalid")
	}
	p, err := provider.Find(s.ProviderID)
	if err != nil || !p.IsRemoteMagpie() || p.Chat != root+"/v1" {
		return fail("health_binding_unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, root+"/v1/magpie/concurrency", nil)
	if err != nil {
		return fail("health_request_invalid")
	}
	if err = p.Sign(ctx, req, provider.Chat, nil); err != nil {
		return fail("health_signing_failed")
	}
	res, err := c.http.Do(req)
	if err != nil {
		return fail("health_unreachable")
	}
	defer res.Body.Close()
	result := HealthResult{Reachable: true}
	if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
		result.Unauthorized = true
		result.Code = "health_unauthorized"
		return result
	}
	if res.StatusCode != http.StatusOK {
		result.Code = "health_gateway_failed"
		return result
	}
	// The bytes are bounded and discarded in this call; no active request,
	// model, prompt, account or upstream error becomes diagnostic state.
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20+1))
	if err != nil || len(body) > 1<<20 {
		result.Code = "health_response_invalid"
		return result
	}
	var shape struct {
		Object   string          `json:"object"`
		Data     json.RawMessage `json:"data"`
		Requests *int            `json:"requests"`
		Waiting  *int            `json:"waiting"`
		Tools    *int            `json:"tools"`
		Active   json.RawMessage `json:"active"`
	}
	if json.Unmarshal(body, &shape) != nil || shape.Object != "concurrency" || !jsonArray(shape.Data) || !jsonArray(shape.Active) || shape.Requests == nil || shape.Waiting == nil || shape.Tools == nil || *shape.Requests < 0 || *shape.Waiting < 0 || *shape.Tools < 0 {
		result.Code = "health_response_invalid"
		return result
	}
	result.Healthy = true
	return result
}

func jsonArray(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '['
}

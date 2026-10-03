package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/nicolasmarchal/wholphin-companion/internal/domain"
)

type apiClient struct {
	service string
	base    *url.URL
	apiKey  string
	http    *http.Client
}

func newAPIClient(service string, base *url.URL, apiKey string, client *http.Client) apiClient {
	return apiClient{service: service, base: base, apiKey: apiKey, http: noRedirectClient(client)}
}

// API credentials must never follow a redirect to a different (or attacker-
// controlled) origin. Arr/Jellyfin/qBittorrent endpoints are configured as
// canonical URLs, so redirects fail closed and surface as upstream errors.
func noRedirectClient(template *http.Client) *http.Client {
	if template == nil {
		template = http.DefaultClient
	}
	client := *template
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &client
}

func (c apiClient) do(ctx context.Context, method, path string, query url.Values, body []byte, out any) error {
	u := *c.base
	u.Path = strings.TrimRight(c.base.Path, "/") + path
	u.RawQuery = query.Encode()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Api-Key", c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		var netErr net.Error
		ambiguous := method != http.MethodGet && (errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr))
		code := "unreachable"
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
			code = "upstream_timeout"
		}
		return &domain.UpstreamError{Service: c.service, Code: code, Retryable: true, Ambiguous: ambiguous}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		ambiguous := method != http.MethodGet && (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500)
		return &domain.UpstreamError{
			Service:   c.service,
			Status:    resp.StatusCode,
			Code:      statusCode(resp.StatusCode),
			Retryable: resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500,
			Ambiguous: ambiguous,
		}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 32<<20))
	decoder.UseNumber()
	if err := decoder.Decode(out); err != nil {
		return &domain.UpstreamError{Service: c.service, Status: resp.StatusCode, Code: "invalid_response", Retryable: true}
	}
	return nil
}

func statusCode(status int) string {
	switch status {
	case http.StatusNotFound, http.StatusGone:
		return "release_expired"
	case http.StatusConflict, http.StatusUnprocessableEntity:
		return "release_rejected"
	case http.StatusUnauthorized, http.StatusForbidden:
		return "upstream_authentication_failed"
	case http.StatusTooManyRequests:
		return "upstream_rate_limited"
	default:
		return fmt.Sprintf("upstream_http_%d", status)
	}
}

func marshal(value any) ([]byte, error) { return json.Marshal(value) }

func intValue(value any) int {
	switch value := value.(type) {
	case json.Number:
		v, _ := value.Int64()
		return int(v)
	case float64:
		return int(value)
	case int:
		return value
	case string:
		v, _ := strconv.Atoi(value)
		return v
	default:
		return 0
	}
}

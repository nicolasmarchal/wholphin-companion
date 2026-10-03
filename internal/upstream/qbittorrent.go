package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"

	"github.com/nicolasmarchal/wholphin-companion/internal/domain"
)

type QBittorrentClient struct {
	base     *url.URL
	username string
	password string
	http     *http.Client
	mu       sync.Mutex
	loggedIn bool
}

func NewQBittorrent(base *url.URL, username, password string, template *http.Client) (*QBittorrentClient, error) {
	if base == nil {
		return &QBittorrentClient{}, nil
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	client := *noRedirectClient(template)
	client.Jar = jar
	return &QBittorrentClient{base: base, username: username, password: password, http: &client}, nil
}

func (c *QBittorrentClient) Enabled() bool { return c.base != nil }

func (c *QBittorrentClient) Stats(ctx context.Context, hash string) (domain.TorrentStats, error) {
	if !c.Enabled() || strings.TrimSpace(hash) == "" {
		return domain.TorrentStats{}, errors.New("qBittorrent metrics are unavailable")
	}
	if err := c.ensureLogin(ctx); err != nil {
		return domain.TorrentStats{}, err
	}
	result, status, err := c.info(ctx, hash)
	if status == http.StatusForbidden {
		c.mu.Lock()
		c.loggedIn = false
		c.mu.Unlock()
		if loginErr := c.ensureLogin(ctx); loginErr != nil {
			return domain.TorrentStats{}, loginErr
		}
		result, _, err = c.info(ctx, hash)
	}
	return result, err
}

func (c *QBittorrentClient) ensureLogin(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loggedIn {
		return nil
	}
	form := url.Values{"username": {c.username}, "password": {c.password}}
	resp, err := c.request(ctx, http.MethodPost, "/api/v2/auth/login", form.Encode(), "application/x-www-form-urlencoded")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32))
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != "Ok." {
		return &domain.UpstreamError{Service: "qbittorrent", Status: resp.StatusCode, Code: "authentication_failed"}
	}
	c.loggedIn = true
	return nil
}

func (c *QBittorrentClient) info(ctx context.Context, hash string) (domain.TorrentStats, int, error) {
	path := "/api/v2/torrents/info?" + url.Values{"hashes": {hash}}.Encode()
	resp, err := c.request(ctx, http.MethodGet, path, "", "")
	if err != nil {
		return domain.TorrentStats{}, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return domain.TorrentStats{}, resp.StatusCode, &domain.UpstreamError{Service: "qbittorrent", Status: resp.StatusCode, Code: statusCode(resp.StatusCode), Retryable: resp.StatusCode >= 500}
	}
	var torrents []struct {
		Progress   float64 `json:"progress"`
		Downloaded int64   `json:"downloaded"`
		TotalSize  int64   `json:"total_size"`
		Size       int64   `json:"size"`
		DLSpeed    int64   `json:"dlspeed"`
		ETA        int64   `json:"eta"`
		State      string  `json:"state"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&torrents); err != nil {
		return domain.TorrentStats{}, resp.StatusCode, &domain.UpstreamError{Service: "qbittorrent", Code: "invalid_response", Retryable: true}
	}
	if len(torrents) == 0 {
		return domain.TorrentStats{}, resp.StatusCode, &domain.UpstreamError{Service: "qbittorrent", Status: 404, Code: "torrent_not_found"}
	}
	value := torrents[0]
	total := value.TotalSize
	if total <= 0 {
		total = value.Size
	}
	return domain.TorrentStats{Progress: value.Progress, DownloadedBytes: value.Downloaded, TotalBytes: total, BytesPerSecond: value.DLSpeed, ETASeconds: value.ETA, State: value.State}, resp.StatusCode, nil
}

func (c *QBittorrentClient) request(ctx context.Context, method, path, body, contentType string) (*http.Response, error) {
	u := *c.base
	parts := strings.SplitN(path, "?", 2)
	u.Path = strings.TrimRight(c.base.Path, "/") + parts[0]
	if len(parts) == 2 {
		u.RawQuery = parts[1]
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &domain.UpstreamError{Service: "qbittorrent", Code: "unreachable", Retryable: true}
	}
	return resp, nil
}

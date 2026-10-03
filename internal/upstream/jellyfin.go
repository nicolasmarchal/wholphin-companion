package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/nicolasmarchal/wholphin-companion/internal/domain"
)

type JellyfinClient struct {
	base   *url.URL
	apiKey string
	http   *http.Client
}

func NewJellyfin(base *url.URL, apiKey string, client *http.Client) *JellyfinClient {
	return &JellyfinClient{base: base, apiKey: apiKey, http: noRedirectClient(client)}
}

func (c *JellyfinClient) Authenticate(ctx context.Context, token string) (domain.User, error) {
	var response struct {
		ID     string `json:"Id"`
		Name   string `json:"Name"`
		Policy struct {
			IsAdministrator bool `json:"IsAdministrator"`
			IsDisabled      bool `json:"IsDisabled"`
		} `json:"Policy"`
	}
	if err := c.do(ctx, http.MethodGet, "/Users/Me", nil, nil, token, &response); err != nil {
		return domain.User{}, err
	}
	if response.ID == "" || response.Policy.IsDisabled {
		return domain.User{}, &domain.UpstreamError{Service: "jellyfin", Status: 401, Code: "invalid_jellyfin_session"}
	}
	return domain.User{ID: response.ID, Name: response.Name, IsAdmin: response.Policy.IsAdministrator}, nil
}

func (c *JellyfinClient) Availability(ctx context.Context, userID string, subject domain.Subject, expectedEpisodes []int) (domain.Availability, error) {
	if subject.Kind == domain.Movie {
		items, err := c.items(ctx, userID, url.Values{
			"Recursive": {"true"}, "IncludeItemTypes": {"Movie"},
			"AnyProviderIdEquals": {fmt.Sprintf("Tmdb.%d", subject.TMDBID)},
			"Fields":              {"ProviderIds,MediaSources"}, "EnableTotalRecordCount": {"false"},
		})
		if err != nil {
			return domain.Availability{}, err
		}
		for _, item := range items {
			if providerID(item.ProviderIDs, "Tmdb") != strconv.Itoa(subject.TMDBID) {
				continue
			}
			playable, playErr := c.playable(ctx, userID, item.ID)
			if playErr != nil {
				return domain.Availability{}, playErr
			}
			if playable {
				return domain.Availability{Available: true, ItemID: item.ID}, nil
			}
		}
		return domain.Availability{}, nil
	}

	series, err := c.items(ctx, userID, url.Values{
		"Recursive": {"true"}, "IncludeItemTypes": {"Series"},
		"AnyProviderIdEquals": {fmt.Sprintf("Tmdb.%d", subject.TMDBID)},
		"Fields":              {"ProviderIds"}, "EnableTotalRecordCount": {"false"},
	})
	if err != nil {
		return domain.Availability{}, err
	}
	var matchingSeries []jellyfinItem
	for _, item := range series {
		if providerID(item.ProviderIDs, "Tmdb") == strconv.Itoa(subject.TMDBID) {
			matchingSeries = append(matchingSeries, item)
		}
	}
	if len(matchingSeries) == 0 && subject.TVDBID > 0 {
		series, err = c.items(ctx, userID, url.Values{
			"Recursive": {"true"}, "IncludeItemTypes": {"Series"},
			"AnyProviderIdEquals": {fmt.Sprintf("Tvdb.%d", subject.TVDBID)},
			"Fields":              {"ProviderIds"}, "EnableTotalRecordCount": {"false"},
		})
		if err != nil {
			return domain.Availability{}, err
		}
		for _, item := range series {
			if providerID(item.ProviderIDs, "Tvdb") == strconv.Itoa(subject.TVDBID) {
				matchingSeries = append(matchingSeries, item)
			}
		}
	}
	if len(matchingSeries) == 0 || subject.SeasonNumber == nil || len(expectedEpisodes) == 0 {
		// Empty coverage is intentionally not interpreted as a complete season.
		return domain.Availability{}, nil
	}
	var playbackErr error
	for _, seriesItem := range matchingSeries {
		episodes, itemErr := c.items(ctx, userID, url.Values{
			"ParentId": {seriesItem.ID}, "Recursive": {"true"}, "IncludeItemTypes": {"Episode"},
			"Fields": {"ProviderIds,MediaSources"}, "EnableTotalRecordCount": {"false"},
		})
		if itemErr != nil {
			playbackErr = itemErr
			continue
		}
		byNumber := make(map[int][]jellyfinItem)
		for _, item := range episodes {
			if item.ParentIndexNumber == *subject.SeasonNumber && item.IndexNumber > 0 {
				byNumber[item.IndexNumber] = append(byNumber[item.IndexNumber], item)
			}
		}
		complete := true
		for _, number := range expectedEpisodes {
			versions := byNumber[number]
			if len(versions) == 0 {
				complete = false
				break
			}
			onePlayable := false
			for _, item := range versions {
				playable, playErr := c.playable(ctx, userID, item.ID)
				if playErr != nil {
					playbackErr = playErr
					continue
				}
				if playable {
					onePlayable = true
					break
				}
			}
			if !onePlayable {
				complete = false
				break
			}
		}
		if complete {
			// For TV, navigation targets the series after every expected episode
			// has independently passed PlaybackInfo for this Jellyfin user.
			return domain.Availability{Available: true, ItemID: seriesItem.ID}, nil
		}
	}
	if playbackErr != nil {
		return domain.Availability{}, playbackErr
	}
	return domain.Availability{}, nil
}

type jellyfinItem struct {
	ID                string            `json:"Id"`
	IndexNumber       int               `json:"IndexNumber"`
	ParentIndexNumber int               `json:"ParentIndexNumber"`
	ProviderIDs       map[string]string `json:"ProviderIds"`
}

func (c *JellyfinClient) items(ctx context.Context, userID string, query url.Values) ([]jellyfinItem, error) {
	query.Set("UserId", userID)
	var response struct {
		Items []jellyfinItem `json:"Items"`
	}
	if err := c.do(ctx, http.MethodGet, "/Items", query, nil, c.apiKey, &response); err != nil {
		return nil, err
	}
	return response.Items, nil
}

func (c *JellyfinClient) playable(ctx context.Context, userID, itemID string) (bool, error) {
	var response struct {
		ErrorCode    string `json:"ErrorCode"`
		MediaSources []struct {
			ID                   string `json:"Id"`
			Path                 string `json:"Path"`
			SupportsDirectPlay   bool   `json:"SupportsDirectPlay"`
			SupportsDirectStream bool   `json:"SupportsDirectStream"`
			SupportsTranscoding  bool   `json:"SupportsTranscoding"`
		} `json:"MediaSources"`
	}
	query := url.Values{"UserId": {userID}}
	if err := c.do(ctx, http.MethodPost, "/Items/"+url.PathEscape(itemID)+"/PlaybackInfo", query, []byte(`{}`), c.apiKey, &response); err != nil {
		return false, err
	}
	if response.ErrorCode != "" {
		return false, nil
	}
	for _, source := range response.MediaSources {
		if (source.ID != "" || source.Path != "") && (source.SupportsDirectPlay || source.SupportsDirectStream || source.SupportsTranscoding) {
			return true, nil
		}
	}
	return false, nil
}

func (c *JellyfinClient) do(ctx context.Context, method, path string, query url.Values, body []byte, token string, out any) error {
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
	req.Header.Set("X-Emby-Token", token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		code := "unreachable"
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
			code = "upstream_timeout"
		}
		return &domain.UpstreamError{Service: "jellyfin", Code: code, Retryable: true}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return &domain.UpstreamError{Service: "jellyfin", Status: resp.StatusCode, Code: statusCode(resp.StatusCode), Retryable: resp.StatusCode >= 500}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out); err != nil {
		return &domain.UpstreamError{Service: "jellyfin", Code: "invalid_response", Retryable: true}
	}
	return nil
}

func providerID(values map[string]string, key string) string {
	for actual, value := range values {
		if strings.EqualFold(actual, key) {
			return value
		}
	}
	return ""
}

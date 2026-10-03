package upstream

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nicolasmarchal/wholphin-companion/internal/config"
	"github.com/nicolasmarchal/wholphin-companion/internal/domain"
)

type arrClient struct {
	kind           string
	api            apiClient
	rootFolder     string
	qualityProfile int
}

func NewRadarr(cfg config.Arr, client *http.Client) domain.Arr {
	return &arrClient{kind: "radarr", api: newAPIClient("radarr", cfg.URL, cfg.APIKey, client), rootFolder: cfg.RootFolder, qualityProfile: cfg.QualityProfileID}
}

func NewSonarr(cfg config.Arr, client *http.Client) domain.Arr {
	return &arrClient{kind: "sonarr", api: newAPIClient("sonarr", cfg.URL, cfg.APIKey, client), rootFolder: cfg.RootFolder, qualityProfile: cfg.QualityProfileID}
}

func (c *arrClient) Resolve(ctx context.Context, subject domain.Subject) (domain.ResolvedSubject, error) {
	if c.kind == "radarr" {
		if subject.Kind != domain.Movie {
			return domain.ResolvedSubject{}, errors.New("radarr only resolves movies")
		}
		return c.resolveMovie(ctx, subject)
	}
	if subject.Kind == domain.Movie {
		return domain.ResolvedSubject{}, errors.New("sonarr only resolves series")
	}
	return c.resolveSeries(ctx, subject)
}

func (c *arrClient) resolveMovie(ctx context.Context, subject domain.Subject) (domain.ResolvedSubject, error) {
	var movies []map[string]any
	if err := c.api.do(ctx, http.MethodGet, "/api/v3/movie", url.Values{"tmdbId": {strconv.Itoa(subject.TMDBID)}}, nil, &movies); err != nil {
		return domain.ResolvedSubject{}, err
	}
	movie := exactMap(movies, "tmdbId", subject.TMDBID)
	if movie == nil {
		if err := c.api.do(ctx, http.MethodGet, "/api/v3/movie/lookup", url.Values{"term": {fmt.Sprintf("tmdb:%d", subject.TMDBID)}}, nil, &movies); err != nil {
			return domain.ResolvedSubject{}, err
		}
		movie = exactMap(movies, "tmdbId", subject.TMDBID)
		if movie == nil {
			return domain.ResolvedSubject{}, &domain.UpstreamError{Service: "radarr", Status: 404, Code: "movie_not_found"}
		}
		movie["qualityProfileId"] = c.qualityProfile
		movie["rootFolderPath"] = c.rootFolder
		// A BFF-created item must never be eligible for an RSS/automatic grab.
		// The only grab is the exact cached release posted later by Grab.
		movie["monitored"] = false
		movie["addOptions"] = map[string]any{"monitor": "none", "searchForMovie": false}
		body, _ := marshal(movie)
		var added map[string]any
		if err := c.api.do(ctx, http.MethodPost, "/api/v3/movie", nil, body, &added); err != nil {
			return domain.ResolvedSubject{}, err
		}
		movie = added
	}
	id := intValue(movie["id"])
	if id <= 0 {
		return domain.ResolvedSubject{}, &domain.UpstreamError{Service: "radarr", Code: "invalid_movie_id"}
	}
	return domain.ResolvedSubject{Subject: subject, Backend: "radarr", ArrItemID: id}, nil
}

func (c *arrClient) resolveSeries(ctx context.Context, subject domain.Subject) (domain.ResolvedSubject, error) {
	// TMDb is the public identity. A client-provided TVDb ID is only a hint and
	// must be corroborated by Sonarr lookup before it can influence a download.
	var lookup []map[string]any
	lookupErr := c.api.do(ctx, http.MethodGet, "/api/v3/series/lookup", url.Values{"term": {fmt.Sprintf("tmdb:%d", subject.TMDBID)}}, nil, &lookup)
	lookupItem := exactMap(lookup, "tmdbId", subject.TMDBID)
	if lookupItem == nil && subject.TVDBID > 0 {
		lookup = nil
		if err := c.api.do(ctx, http.MethodGet, "/api/v3/series/lookup", url.Values{"term": {fmt.Sprintf("tvdb:%d", subject.TVDBID)}}, nil, &lookup); err != nil {
			if lookupErr != nil {
				return domain.ResolvedSubject{}, lookupErr
			}
			return domain.ResolvedSubject{}, err
		}
		lookupItem = exactMap(lookup, "tvdbId", subject.TVDBID)
		if lookupItem != nil && intValue(lookupItem["tmdbId"]) != subject.TMDBID {
			return domain.ResolvedSubject{}, &domain.UpstreamError{Service: "sonarr", Status: 422, Code: "subject_identity_mismatch"}
		}
	}
	if lookupItem == nil {
		if lookupErr != nil && subject.TVDBID == 0 {
			return domain.ResolvedSubject{}, lookupErr
		}
		return domain.ResolvedSubject{}, &domain.UpstreamError{Service: "sonarr", Status: 422, Code: "series_mapping_required"}
	}
	derivedTVDBID := intValue(lookupItem["tvdbId"])
	if derivedTVDBID <= 0 {
		return domain.ResolvedSubject{}, &domain.UpstreamError{Service: "sonarr", Status: 422, Code: "series_mapping_required"}
	}
	if subject.TVDBID > 0 && subject.TVDBID != derivedTVDBID {
		return domain.ResolvedSubject{}, &domain.UpstreamError{Service: "sonarr", Status: 422, Code: "subject_identity_mismatch"}
	}
	subject.TVDBID = derivedTVDBID

	var existing []map[string]any
	if err := c.api.do(ctx, http.MethodGet, "/api/v3/series", url.Values{"tvdbId": {strconv.Itoa(subject.TVDBID)}}, nil, &existing); err != nil {
		return domain.ResolvedSubject{}, err
	}
	item := exactMap(existing, "tvdbId", subject.TVDBID)
	if item == nil {
		item = lookupItem
		item["qualityProfileId"] = c.qualityProfile
		item["rootFolderPath"] = c.rootFolder
		item["monitored"] = false
		item["seasonFolder"] = true
		disableSeasonMonitoring(item)
		item["addOptions"] = map[string]any{"monitor": "none", "searchForMissingEpisodes": false, "searchForCutoffUnmetEpisodes": false}
		body, _ := marshal(item)
		var added map[string]any
		if err := c.api.do(ctx, http.MethodPost, "/api/v3/series", nil, body, &added); err != nil {
			return domain.ResolvedSubject{}, err
		}
		item = added
	}
	seriesID := intValue(item["id"])
	if seriesID <= 0 {
		return domain.ResolvedSubject{}, &domain.UpstreamError{Service: "sonarr", Code: "invalid_series_id"}
	}
	query := url.Values{"seriesId": {strconv.Itoa(seriesID)}, "seasonNumber": {strconv.Itoa(*subject.SeasonNumber)}}
	var episodes []struct {
		ID            int  `json:"id"`
		SeasonNumber  int  `json:"seasonNumber"`
		EpisodeNumber int  `json:"episodeNumber"`
		HasFile       bool `json:"hasFile"`
	}
	if err := c.api.do(ctx, http.MethodGet, "/api/v3/episode", query, nil, &episodes); err != nil {
		return domain.ResolvedSubject{}, err
	}
	resolved := domain.ResolvedSubject{Subject: subject, Backend: "sonarr", ArrItemID: seriesID}
	for _, episode := range episodes {
		if episode.EpisodeNumber > 0 {
			resolved.ExpectedEpisodes = append(resolved.ExpectedEpisodes, episode.EpisodeNumber)
		}
		if subject.Kind == domain.Episode && episode.EpisodeNumber == *subject.EpisodeNumber {
			resolved.ArrEpisodeID = episode.ID
		}
	}
	sort.Ints(resolved.ExpectedEpisodes)
	if subject.Kind == domain.Episode {
		resolved.ExpectedEpisodes = []int{*subject.EpisodeNumber}
		if resolved.ArrEpisodeID <= 0 {
			return domain.ResolvedSubject{}, &domain.UpstreamError{Service: "sonarr", Status: 404, Code: "episode_not_found"}
		}
	}
	return resolved, nil
}

func (c *arrClient) Search(ctx context.Context, resolved domain.ResolvedSubject) ([]domain.Candidate, error) {
	query := url.Values{}
	if c.kind == "radarr" {
		query.Set("movieId", strconv.Itoa(resolved.ArrItemID))
	} else {
		query.Set("seriesId", strconv.Itoa(resolved.ArrItemID))
		if resolved.Subject.Kind == domain.Episode {
			query.Set("episodeId", strconv.Itoa(resolved.ArrEpisodeID))
		} else {
			query.Set("seasonNumber", strconv.Itoa(*resolved.Subject.SeasonNumber))
		}
	}
	var raw []json.RawMessage
	if err := c.api.do(ctx, http.MethodGet, "/api/v3/release", query, nil, &raw); err != nil {
		return nil, err
	}
	result := make([]domain.Candidate, 0, len(raw))
	for _, payload := range raw {
		candidate, err := parseRelease(payload, resolved)
		if err != nil {
			continue
		}
		result = append(result, candidate)
	}
	return result, nil
}

func (c *arrClient) Grab(ctx context.Context, payload json.RawMessage) error {
	return c.api.do(ctx, http.MethodPost, "/api/v3/release", nil, payload, nil)
}

func (c *arrClient) Observe(ctx context.Context, job domain.Job) (domain.Observation, error) {
	// Title alone is not a release identity: two indexers (or a manual grab)
	// can expose the same title. Until history proves the selected GUID, do not
	// adopt a queue item or downloadId.
	if job.DownloadID == "" {
		return c.historyObservation(ctx, job)
	}
	query := url.Values{"page": {"1"}, "pageSize": {"100"}}
	if c.kind == "radarr" {
		query.Set("includeMovie", "true")
		query.Set("includeUnknownMovieItems", "true")
		query.Set("movieIds", strconv.Itoa(job.ArrItemID))
	} else {
		query.Set("includeSeries", "true")
		query.Set("includeEpisode", "true")
		query.Set("includeUnknownSeriesItems", "true")
		query.Set("seriesIds", strconv.Itoa(job.ArrItemID))
	}
	var page struct {
		Records []queueRecord `json:"records"`
	}
	if err := c.api.do(ctx, http.MethodGet, "/api/v3/queue", query, nil, &page); err != nil {
		return domain.Observation{}, err
	}
	for _, record := range page.Records {
		if c.kind == "radarr" && record.MovieID != job.ArrItemID {
			continue
		}
		if c.kind == "sonarr" && record.SeriesID != job.ArrItemID {
			continue
		}
		if record.DownloadID == "" || !strings.EqualFold(job.DownloadID, record.DownloadID) {
			continue
		}
		return record.observation(), nil
	}
	return c.historyObservation(ctx, job)
}

func (c *arrClient) historyObservation(ctx context.Context, job domain.Job) (domain.Observation, error) {
	query := url.Values{}
	path := "/api/v3/history/movie"
	if c.kind == "radarr" {
		query.Set("movieId", strconv.Itoa(job.ArrItemID))
	} else {
		path = "/api/v3/history/series"
		query.Set("seriesId", strconv.Itoa(job.ArrItemID))
		query.Set("includeEpisode", "true")
		if job.Subject.SeasonNumber != nil {
			query.Set("seasonNumber", strconv.Itoa(*job.Subject.SeasonNumber))
		}
	}
	var history []struct {
		DownloadID  string         `json:"downloadId"`
		SourceTitle string         `json:"sourceTitle"`
		EventType   string         `json:"eventType"`
		Date        time.Time      `json:"date"`
		Data        map[string]any `json:"data"`
		Episode     struct {
			SeasonNumber  int `json:"seasonNumber"`
			EpisodeNumber int `json:"episodeNumber"`
		} `json:"episode"`
	}
	if err := c.api.do(ctx, http.MethodGet, path, query, nil, &history); err != nil {
		return domain.Observation{}, err
	}
	sort.Slice(history, func(i, j int) bool { return history[i].Date.After(history[j].Date) })
	importedEpisodes := make(map[int]struct{})
	matchedDownloadID := job.DownloadID
	if matchedDownloadID == "" {
		for _, event := range history {
			if event.Date.Before(job.CreatedAt.Add(-2*time.Minute)) || !strings.EqualFold(event.EventType, "grabbed") {
				continue
			}
			if event.DownloadID != "" && matchesReleaseFingerprint(job, event.Data) {
				matchedDownloadID = event.DownloadID
				break
			}
		}
		if matchedDownloadID == "" {
			return domain.Observation{State: job.State, Progress: job.Progress}, nil
		}
	}
	grabbed := false
	for _, event := range history {
		if event.Date.Before(job.CreatedAt.Add(-2 * time.Minute)) {
			continue
		}
		if matchedDownloadID != "" {
			// Sonarr can emit imports without downloadId. Title equality is not
			// a strong identity, so fail closed instead of attributing a manual or
			// concurrent same-title import to this acquisition.
			if event.DownloadID == "" || !strings.EqualFold(matchedDownloadID, event.DownloadID) {
				continue
			}
		}
		switch strings.ToLower(event.EventType) {
		case "downloadfolderimported", "moviefileimported", "episodefileimported":
			if c.kind == "radarr" {
				return domain.Observation{State: domain.Imported, DownloadID: matchedDownloadID, Progress: domain.Progress{Text: "Import completed", Source: "arr"}}, nil
			}
			if event.Episode.EpisodeNumber > 0 && job.Subject.SeasonNumber != nil && event.Episode.SeasonNumber == *job.Subject.SeasonNumber {
				importedEpisodes[event.Episode.EpisodeNumber] = struct{}{}
			}
		case "downloadfailed":
			return domain.Observation{State: domain.Failed, DownloadID: matchedDownloadID, ErrorCode: "download_failed", Error: "Arr reported a failed download"}, nil
		case "grabbed":
			grabbed = true
		}
	}
	if len(importedEpisodes) > 0 {
		actual := make([]int, 0, len(importedEpisodes))
		for number := range importedEpisodes {
			actual = append(actual, number)
		}
		if coversAll(actual, job.ExpectedEpisodes) {
			return domain.Observation{State: domain.Imported, DownloadID: matchedDownloadID, Progress: domain.Progress{Text: "Import completed", Source: "arr"}}, nil
		}
		return domain.Observation{State: domain.Importing, DownloadID: matchedDownloadID, Progress: domain.Progress{Text: "Import in progress", Source: "arr"}}, nil
	}
	if grabbed {
		return domain.Observation{State: domain.Queued, DownloadID: matchedDownloadID, Progress: domain.Progress{Text: "Queued", Source: "arr"}}, nil
	}
	return domain.Observation{State: job.State, DownloadID: job.DownloadID, Progress: job.Progress}, nil
}

func matchesReleaseFingerprint(job domain.Job, data map[string]any) bool {
	if len(job.ReleaseGUIDHash) != sha256.Size {
		return false
	}
	guid := stringValue(data, "guid")
	if guid == "" {
		return false
	}
	digest := sha256.Sum256([]byte(guid))
	if !bytes.Equal(job.ReleaseGUIDHash, digest[:]) {
		return false
	}
	matchedSecondary := false
	if actualIndexer := intValue(valueFold(data, "indexerId")); actualIndexer > 0 && job.ReleaseIndexerID > 0 {
		if actualIndexer != job.ReleaseIndexerID {
			return false
		}
		matchedSecondary = true
	}
	for _, candidate := range []struct {
		stored     []byte
		value      string
		normalized bool
	}{
		{job.ReleaseIndexerHash, stringValue(data, "indexer"), true},
		{job.ReleaseURLHash, stringValue(data, "downloadUrl"), false},
		{job.ReleaseInfoHash, stringValue(data, "torrentInfoHash"), true},
	} {
		if len(candidate.stored) == 0 || candidate.value == "" {
			continue
		}
		value := candidate.value
		if candidate.normalized {
			value = strings.ToLower(strings.TrimSpace(value))
		}
		secondary := sha256.Sum256([]byte(value))
		if !bytes.Equal(candidate.stored, secondary[:]) {
			return false
		}
		matchedSecondary = true
	}
	return matchedSecondary
}

func valueFold(values map[string]any, key string) any {
	for actual, value := range values {
		if strings.EqualFold(actual, key) {
			return value
		}
	}
	return nil
}

func stringValue(values map[string]any, key string) string {
	value, _ := valueFold(values, key).(string)
	return value
}

func (c *arrClient) Cancel(ctx context.Context, job domain.Job) error {
	if job.ArrQueueID <= 0 {
		return &domain.UpstreamError{Service: c.kind, Status: 409, Code: "queue_item_missing"}
	}
	path := "/api/v3/queue/" + strconv.Itoa(job.ArrQueueID)
	query := url.Values{"removeFromClient": {"true"}, "blocklist": {"false"}, "skipRedownload": {"true"}}
	return c.api.do(ctx, http.MethodDelete, path, query, nil, nil)
}

func exactMap(items []map[string]any, key string, expected int) map[string]any {
	if expected <= 0 {
		return nil
	}
	for _, item := range items {
		if intValue(item[key]) == expected {
			return item
		}
	}
	return nil
}

func disableSeasonMonitoring(series map[string]any) {
	seasons, ok := series["seasons"].([]any)
	if !ok {
		return
	}
	for _, raw := range seasons {
		if season, ok := raw.(map[string]any); ok {
			season["monitored"] = false
		}
	}
}

type releaseEnvelope struct {
	GUID                 string   `json:"guid"`
	IndexerID            int      `json:"indexerId"`
	Title                string   `json:"title"`
	Size                 int64    `json:"size"`
	Indexer              string   `json:"indexer"`
	Protocol             string   `json:"protocol"`
	Approved             bool     `json:"approved"`
	Rejected             bool     `json:"rejected"`
	Rejections           []string `json:"rejections"`
	Seeders              *int     `json:"seeders"`
	FullSeason           bool     `json:"fullSeason"`
	SeasonNumber         *int     `json:"seasonNumber"`
	MappedSeasonNumber   *int     `json:"mappedSeasonNumber"`
	EpisodeNumbers       []int    `json:"episodeNumbers"`
	MappedEpisodeNumbers []int    `json:"mappedEpisodeNumbers"`
	Quality              struct {
		Quality struct {
			Name string `json:"name"`
		} `json:"quality"`
	} `json:"quality"`
}

func parseRelease(payload json.RawMessage, resolved domain.ResolvedSubject) (domain.Candidate, error) {
	var value releaseEnvelope
	if err := json.Unmarshal(payload, &value); err != nil || value.Title == "" {
		return domain.Candidate{}, errors.New("invalid release")
	}
	coveredEpisodes := value.MappedEpisodeNumbers
	if len(coveredEpisodes) == 0 {
		coveredEpisodes = value.EpisodeNumbers
	}
	rejected := value.Rejected || len(value.Rejections) > 0
	approved := value.Approved
	rejections := make([]string, 0, len(value.Rejections)+2)
	for _, rejection := range value.Rejections {
		rejections = appendUnique(rejections, publicRejection(rejection))
	}
	if value.Rejected && len(rejections) == 0 {
		rejections = append(rejections, "Rejected by Arr policy")
	}
	if value.GUID == "" || value.IndexerID <= 0 || strings.TrimSpace(value.Indexer) == "" {
		rejected, approved = true, false
		rejections = append(rejections, "BFF: release has no stable Arr selection identity")
	}
	releaseSeason := value.MappedSeasonNumber
	if releaseSeason == nil {
		releaseSeason = value.SeasonNumber
	}
	if resolved.Subject.Kind == domain.Season {
		if releaseSeason == nil || resolved.Subject.SeasonNumber == nil || *releaseSeason != *resolved.Subject.SeasonNumber {
			rejected, approved = true, false
			rejections = append(rejections, "BFF: release season does not match the requested season")
		}
		if !value.FullSeason {
			rejected, approved = true, false
			rejections = append(rejections, "BFF: release is not a full-season pack")
		} else if !coversAll(coveredEpisodes, resolved.ExpectedEpisodes) {
			rejected, approved = true, false
			rejections = append(rejections, "BFF: season pack does not cover all expected episodes")
		}
	} else if resolved.Subject.Kind == domain.Episode {
		if releaseSeason == nil || resolved.Subject.SeasonNumber == nil || *releaseSeason != *resolved.Subject.SeasonNumber {
			rejected, approved = true, false
			rejections = append(rejections, "BFF: release season does not match the requested episode")
		}
		if resolved.Subject.EpisodeNumber == nil || !coversAll(coveredEpisodes, []int{*resolved.Subject.EpisodeNumber}) {
			rejected, approved = true, false
			rejections = append(rejections, "BFF: release does not contain the requested episode")
		}
	}
	expectedForJob := append([]int(nil), coveredEpisodes...)
	if resolved.Subject.Kind == domain.Season && !rejected {
		expectedForJob = append([]int(nil), resolved.ExpectedEpisodes...)
	} else if resolved.Subject.Kind == domain.Episode && !rejected {
		expectedForJob = []int{*resolved.Subject.EpisodeNumber}
	}
	return domain.Candidate{
		Release: domain.Release{
			Title: value.Title, SizeBytes: value.Size, Seeders: value.Seeders,
			Quality: value.Quality.Quality.Name, Indexer: value.Indexer,
			Protocol: normalizedProtocol(value.Protocol), Approved: approved,
			Rejected:         rejected,
			RejectionReasons: rejections, FullSeason: value.FullSeason,
			SeasonNumber: releaseSeason, EpisodeNumbers: coveredEpisodes,
		},
		Payload: append([]byte(nil), payload...), ExpectedEpisodes: expectedForJob,
	}, nil
}

func normalizedProtocol(value string) string {
	switch strings.ToLower(value) {
	case "torrent", "usenet":
		return strings.ToLower(value)
	default:
		return "unknown"
	}
}

type queueRecord struct {
	ID                    int     `json:"id"`
	Title                 string  `json:"title"`
	MovieID               int     `json:"movieId"`
	SeriesID              int     `json:"seriesId"`
	Status                string  `json:"status"`
	TrackedDownloadStatus string  `json:"trackedDownloadStatus"`
	TrackedDownloadState  string  `json:"trackedDownloadState"`
	ErrorMessage          string  `json:"errorMessage"`
	DownloadID            string  `json:"downloadId"`
	Size                  float64 `json:"size"`
	SizeLeft              float64 `json:"sizeleft"`
	TimeLeft              string  `json:"timeleft"`
	StatusMessages        []struct {
		Title    string   `json:"title"`
		Messages []string `json:"messages"`
	} `json:"statusMessages"`
}

func (r queueRecord) observation() domain.Observation {
	state := domain.Queued
	status, tracked, trackedState := strings.ToLower(r.Status), strings.ToLower(r.TrackedDownloadStatus), strings.ToLower(r.TrackedDownloadState)
	switch status {
	case "downloading":
		state = domain.Downloading
	case "completed":
		state = domain.Verifying
	case "failed":
		state = domain.Failed
	case "paused":
		state = domain.DownloadBlocked
	}
	if strings.Contains(trackedState, "import") {
		state = domain.Importing
	}
	if tracked == "warning" || tracked == "error" {
		if r.Size > 0 && r.SizeLeft <= 0 {
			state = domain.ImportFailed
		} else {
			state = domain.DownloadBlocked
		}
	}
	// Arr status messages can contain filesystem paths or download URLs. Only
	// expose an allow-listed state description to the TV client.
	progress := domain.Progress{Text: publicStateText(state), Source: "arr"}
	if r.Size > 0 {
		total, downloaded := int64(r.Size), int64(r.Size-r.SizeLeft)
		if downloaded < 0 {
			downloaded = 0
		}
		if downloaded > total {
			downloaded = total
		}
		percent := float64(downloaded) * 100 / float64(total)
		progress.TotalBytes, progress.DownloadedBytes, progress.Percent = &total, &downloaded, &percent
	}
	if eta, ok := parseTimeLeft(r.TimeLeft); ok {
		progress.ETASeconds = &eta
	}
	observation := domain.Observation{State: state, Progress: progress, QueueID: r.ID, DownloadID: r.DownloadID}
	if state == domain.Failed || state == domain.ImportFailed {
		observation.ErrorCode = "arr_queue_error"
		if state == domain.ImportFailed {
			observation.Error = "Arr reported an import failure"
		} else {
			observation.Error = "Arr reported a download failure"
		}
	}
	return observation
}

func publicRejection(value string) string {
	lower := strings.ToLower(value)
	switch {
	case strings.Contains(lower, "quality"), strings.Contains(lower, "resolution"):
		return "Quality does not meet the configured profile"
	case strings.Contains(lower, "seed"):
		return "Insufficient seeders"
	case strings.Contains(lower, "blocklist"):
		return "Release is blocklisted"
	case strings.Contains(lower, "already"):
		return "Release was already processed"
	case strings.Contains(lower, "indexer"):
		return "Rejected by indexer policy"
	default:
		return "Rejected by Arr policy"
	}
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func publicStateText(state domain.JobState) string {
	switch state {
	case domain.Queued:
		return "Queued"
	case domain.Downloading:
		return "Downloading"
	case domain.DownloadBlocked:
		return "Download blocked"
	case domain.Verifying:
		return "Verifying download"
	case domain.Importing:
		return "Importing"
	case domain.ImportFailed:
		return "Import failed"
	case domain.Imported:
		return "Import completed"
	case domain.Failed:
		return "Download failed"
	default:
		return "Processing"
	}
}

func parseTimeLeft(value string) (int64, bool) {
	parts := strings.Split(value, ":")
	if len(parts) != 3 {
		return 0, false
	}
	hours, e1 := strconv.ParseInt(parts[0], 10, 64)
	minutes, e2 := strconv.ParseInt(parts[1], 10, 64)
	seconds, e3 := strconv.ParseInt(strings.Split(parts[2], ".")[0], 10, 64)
	if e1 != nil || e2 != nil || e3 != nil {
		return 0, false
	}
	return hours*3600 + minutes*60 + seconds, true
}

func coversAll(actual, expected []int) bool {
	if len(expected) == 0 || len(actual) == 0 {
		return false
	}
	set := make(map[int]struct{}, len(actual))
	for _, value := range actual {
		set[value] = struct{}{}
	}
	for _, value := range expected {
		if _, ok := set[value]; !ok {
			return false
		}
	}
	return true
}

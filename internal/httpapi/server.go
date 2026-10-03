package httpapi

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	contract "github.com/nicolasmarchal/wholphin-companion/api"
	"github.com/nicolasmarchal/wholphin-companion/internal/app"
	"github.com/nicolasmarchal/wholphin-companion/internal/domain"
	"github.com/nicolasmarchal/wholphin-companion/internal/secure"
	"github.com/nicolasmarchal/wholphin-companion/internal/store"
)

type Server struct {
	app           *app.App
	log           *slog.Logger
	webhookSecret string
	limiter       *limiter
	urls          *http.ServeMux
}

type errorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	RequestID string `json:"requestId"`
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type searchResponse struct {
	SearchID  string             `json:"searchId"`
	Subject   domain.Subject     `json:"subject"`
	State     domain.SearchState `json:"state"`
	ExpiresAt time.Time          `json:"expiresAt"`
	Releases  []domain.Release   `json:"releases"`
	Error     *errorBody         `json:"error,omitempty"`
}

type acquisitionResponse struct {
	ID                          string          `json:"id"`
	Subject                     domain.Subject  `json:"subject"`
	State                       domain.JobState `json:"state"`
	Title                       string          `json:"title"`
	Progress                    *float64        `json:"progress"`
	BytesDownloaded             *int64          `json:"bytesDownloaded"`
	BytesTotal                  *int64          `json:"bytesTotal"`
	DownloadSpeedBytesPerSecond *int64          `json:"downloadSpeedBytesPerSecond"`
	ETASeconds                  *int64          `json:"etaSeconds"`
	StatusText                  *string         `json:"statusText"`
	ProgressSource              *string         `json:"progressSource"`
	Error                       *errorBody      `json:"error,omitempty"`
	JellyfinItemID              *string         `json:"jellyfinItemId"`
	UpdatedAt                   time.Time       `json:"updatedAt"`
}

func New(application *app.App, logger *slog.Logger, webhookSecret string) http.Handler {
	s := &Server{app: application, log: logger, webhookSecret: webhookSecret, limiter: newLimiter(), urls: http.NewServeMux()}
	s.routes()
	return s.securityHeaders(s.urls)
}

func (s *Server) routes() {
	s.urls.HandleFunc("GET /openapi.yaml", s.openAPI)
	s.urls.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	s.urls.HandleFunc("GET /readyz", s.ready)
	s.urls.HandleFunc("POST /v1/session", s.exchangeSession)
	s.urls.Handle("DELETE /v1/session", s.authenticated(s.revokeSession))
	s.urls.Handle("GET /v1/capabilities", s.authenticated(s.capabilities))
	s.urls.Handle("POST /v1/releases/search", s.authenticated(s.startSearch))
	s.urls.Handle("GET /v1/releases/search/{searchId}", s.authenticated(s.getSearch))
	s.urls.Handle("POST /v1/acquisitions", s.authenticated(s.acquire))
	s.urls.Handle("GET /v1/acquisitions", s.authenticated(s.listAcquisitions))
	s.urls.Handle("GET /v1/acquisitions/{acquisitionId}", s.authenticated(s.getAcquisition))
	s.urls.Handle("POST /v1/acquisitions/{acquisitionId}/cancel", s.authenticated(s.cancelAcquisition))
	s.urls.HandleFunc("POST /v1/hooks/{source}", s.webhook)
}

func (s *Server) openAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(contract.OpenAPI)
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	if err := s.app.Ready(r.Context()); err != nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "not_ready", "Service is not ready", true)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) exchangeSession(w http.ResponseWriter, r *http.Request) {
	if !s.allow(w, r, "session:"+remoteIP(r), 5, time.Minute) {
		return
	}
	token := strings.TrimSpace(r.Header.Get("X-Jellyfin-Token"))
	if token == "" || len(token) > 4096 {
		s.writeError(w, r, http.StatusUnauthorized, "missing_jellyfin_token", "Jellyfin token is required", false)
		return
	}
	session, err := s.app.ExchangeSession(r.Context(), token)
	if err != nil {
		if errors.Is(err, app.ErrForbidden) {
			s.writeError(w, r, http.StatusForbidden, "user_not_allowed", "User is not allowed to request downloads", false)
			return
		}
		var upstream *domain.UpstreamError
		if errors.As(err, &upstream) {
			if upstream.Status == http.StatusUnauthorized || upstream.Status == http.StatusForbidden || upstream.Code == "invalid_jellyfin_session" {
				s.writeError(w, r, http.StatusUnauthorized, "invalid_jellyfin_session", "Jellyfin session could not be validated", false)
				return
			}
			if upstream.Code == "upstream_timeout" {
				s.writeError(w, r, http.StatusGatewayTimeout, "jellyfin_timeout", "Jellyfin session validation timed out", true)
				return
			}
			s.writeError(w, r, http.StatusBadGateway, "jellyfin_unavailable", "Jellyfin session validation failed", true)
			return
		}
		s.writeError(w, r, http.StatusServiceUnavailable, "session_store_unavailable", "Session could not be created", true)
		return
	}
	s.writeJSON(w, http.StatusCreated, session)
}

type authedHandler func(http.ResponseWriter, *http.Request, domain.User, string)

func (s *Server) authenticated(next authedHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r.Header.Get("Authorization"))
		user, err := s.app.Authenticate(r.Context(), token)
		if err != nil {
			s.writeError(w, r, http.StatusUnauthorized, "invalid_session", "BFF session is missing, expired or invalid", false)
			return
		}
		if !s.allow(w, r, "user:"+user.ID, 120, time.Minute) {
			return
		}
		next(w, r, user, token)
	})
}

func (s *Server) revokeSession(w http.ResponseWriter, r *http.Request, _ domain.User, token string) {
	if err := s.app.Revoke(r.Context(), token); err != nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "session_revoke_failed", "Session could not be revoked", true)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) capabilities(w http.ResponseWriter, _ *http.Request, _ domain.User, _ string) {
	s.writeJSON(w, http.StatusOK, map[string]bool{
		"hasQbittorrentMetrics": s.app.HasQBittorrentMetrics(),
		"supportsCancellation":  s.app.SupportsCancellation(),
	})
}

func (s *Server) startSearch(w http.ResponseWriter, r *http.Request, user domain.User, _ string) {
	if !s.allow(w, r, "search:"+user.ID, 12, time.Minute) {
		return
	}
	var subject domain.Subject
	if err := decodeJSON(r, &subject); err != nil || subject.Validate() != nil {
		s.writeError(w, r, http.StatusBadRequest, "invalid_subject", "Movie, season or episode subject is invalid", false)
		return
	}
	search, err := s.app.StartSearch(r.Context(), user, subject)
	if err != nil {
		s.handleError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusAccepted, toSearchResponse(search, r))
}

func (s *Server) getSearch(w http.ResponseWriter, r *http.Request, user domain.User, _ string) {
	search, err := s.app.Search(r.Context(), user, r.PathValue("searchId"))
	if err != nil {
		s.handleError(w, r, err)
		return
	}
	if search.State == domain.SearchExpired {
		s.writeError(w, r, http.StatusGone, "search_expired", "Release results expired; run a new search", true)
		return
	}
	s.writeJSON(w, http.StatusOK, toSearchResponse(search, r))
}

func (s *Server) acquire(w http.ResponseWriter, r *http.Request, user domain.User, _ string) {
	if !s.allow(w, r, "acquire:"+user.ID, 20, time.Minute) {
		return
	}
	idem := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if len(idem) < 16 || len(idem) > 128 {
		s.writeError(w, r, http.StatusBadRequest, "invalid_idempotency_key", "Idempotency-Key must contain 16 to 128 characters", false)
		return
	}
	var request struct {
		Token string `json:"token"`
	}
	if err := decodeJSON(r, &request); err != nil || len(request.Token) < 32 || len(request.Token) > 256 {
		s.writeError(w, r, http.StatusBadRequest, "invalid_selection_token", "A valid selection token is required", false)
		return
	}
	job, created, err := s.app.Acquire(r.Context(), user, request.Token, idem)
	if err != nil {
		s.handleError(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusAccepted
	}
	s.writeJSON(w, status, toAcquisition(job, r))
}

func (s *Server) getAcquisition(w http.ResponseWriter, r *http.Request, user domain.User, _ string) {
	job, err := s.app.Job(r.Context(), user, r.PathValue("acquisitionId"))
	if err != nil {
		s.handleError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, toAcquisition(job, r))
}

func (s *Server) listAcquisitions(w http.ResponseWriter, r *http.Request, user domain.User, _ string) {
	active, err := strconv.ParseBool(defaultValue(r.URL.Query().Get("active"), "false"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "invalid_query", "active must be a boolean", false)
		return
	}
	var subject *domain.Subject
	mediaType := strings.TrimSpace(r.URL.Query().Get("mediaType"))
	if mediaType != "" && mediaType != "movie" && mediaType != "tv" {
		s.writeError(w, r, http.StatusBadRequest, "invalid_query", "mediaType must be movie or tv", false)
		return
	}
	if raw := r.URL.Query().Get("tmdbId"); raw != "" {
		tmdbID, parseErr := strconv.Atoi(raw)
		kind := domain.SubjectKind(r.URL.Query().Get("kind"))
		if parseErr != nil || tmdbID <= 0 || (kind != "" && kind != domain.Movie && kind != domain.Season && kind != domain.Episode) {
			s.writeError(w, r, http.StatusBadRequest, "invalid_query", "tmdbId must be positive and kind, when present, must be valid", false)
			return
		}
		value := domain.Subject{Kind: kind, TMDBID: tmdbID}
		if rawSeason := r.URL.Query().Get("seasonNumber"); rawSeason != "" {
			number, parseErr := strconv.Atoi(rawSeason)
			if parseErr != nil || number < 0 {
				s.writeError(w, r, http.StatusBadRequest, "invalid_query", "invalid seasonNumber", false)
				return
			}
			value.SeasonNumber = &number
		}
		if rawEpisode := r.URL.Query().Get("episodeNumber"); rawEpisode != "" {
			number, parseErr := strconv.Atoi(rawEpisode)
			if parseErr != nil || number <= 0 {
				s.writeError(w, r, http.StatusBadRequest, "invalid_query", "invalid episodeNumber", false)
				return
			}
			value.EpisodeNumber = &number
		}
		subject = &value
	} else if mediaType != "" || r.URL.Query().Get("kind") != "" || r.URL.Query().Get("seasonNumber") != "" || r.URL.Query().Get("episodeNumber") != "" {
		s.writeError(w, r, http.StatusBadRequest, "invalid_query", "tmdbId is required with mediaType or subject filters", false)
		return
	}
	if subject != nil && subject.Kind != "" && mediaType != "" {
		if (mediaType == "movie" && subject.Kind != domain.Movie) || (mediaType == "tv" && subject.Kind == domain.Movie) {
			s.writeError(w, r, http.StatusBadRequest, "invalid_query", "mediaType and kind conflict", false)
			return
		}
	}
	jobs, err := s.app.Jobs(r.Context(), user, subject, mediaType, active)
	if err != nil {
		s.handleError(w, r, err)
		return
	}
	result := make([]acquisitionResponse, 0, len(jobs))
	for _, job := range jobs {
		result = append(result, toAcquisition(job, r))
	}
	s.writeJSON(w, http.StatusOK, result)
}

func (s *Server) cancelAcquisition(w http.ResponseWriter, r *http.Request, user domain.User, _ string) {
	job, err := s.app.Cancel(r.Context(), user, r.PathValue("acquisitionId"))
	if err != nil {
		s.handleError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, toAcquisition(job, r))
}

func (s *Server) webhook(w http.ResponseWriter, r *http.Request) {
	if s.webhookSecret == "" {
		http.NotFound(w, r)
		return
	}
	provided := r.Header.Get("X-Wholphin-Webhook-Secret")
	expectedHash, providedHash := sha256.Sum256([]byte(s.webhookSecret)), sha256.Sum256([]byte(provided))
	if subtle.ConstantTimeCompare(expectedHash[:], providedHash[:]) != 1 {
		s.writeError(w, r, http.StatusUnauthorized, "invalid_webhook_secret", "Webhook secret is invalid", false)
		return
	}
	source := r.PathValue("source")
	if source != "radarr" && source != "sonarr" {
		http.NotFound(w, r)
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
	s.app.WakeReconcile()
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.writeError(w, r, http.StatusNotFound, "not_found", "Resource was not found", false)
	case errors.Is(err, store.ErrExpired):
		s.writeError(w, r, http.StatusGone, "release_expired", "Selection token expired; search again", true)
	case errors.Is(err, store.ErrRejected):
		s.writeError(w, r, http.StatusUnprocessableEntity, "release_rejected", "This release is not selectable", false)
	case errors.Is(err, store.ErrConsumed):
		s.writeError(w, r, http.StatusConflict, "token_consumed", "Selection token was already consumed", false)
	case errors.Is(err, store.ErrSubjectActive):
		s.writeError(w, r, http.StatusConflict, "subject_already_active", "An overlapping acquisition is already active", false)
	case errors.Is(err, store.ErrIdempotencyConflict):
		s.writeError(w, r, http.StatusConflict, "idempotency_conflict", "Idempotency-Key was reused with another request", false)
	case errors.Is(err, store.ErrTerminalState), errors.Is(err, store.ErrConcurrentUpdate):
		s.writeError(w, r, http.StatusConflict, "state_conflict", "Acquisition state changed concurrently", true)
	case errors.Is(err, app.ErrForbidden):
		s.writeError(w, r, http.StatusForbidden, "forbidden", "Operation is not allowed", false)
	case errors.Is(err, app.ErrDispatchUncertain):
		s.writeError(w, r, http.StatusConflict, "dispatch_uncertain", "Cancellation is unsafe until Arr identity is reconciled", true)
	case errors.Is(err, app.ErrCancellationUnavailable):
		s.writeError(w, r, http.StatusConflict, "cancellation_unavailable", "Acquisition can no longer be cancelled safely", false)
	default:
		var upstream *domain.UpstreamError
		if errors.As(err, &upstream) {
			status := http.StatusBadGateway
			if upstream.Status == http.StatusUnprocessableEntity {
				status = http.StatusUnprocessableEntity
			}
			if upstream.Status == http.StatusNotFound || upstream.Status == http.StatusGone {
				status = http.StatusGone
			}
			if upstream.Code == "series_mapping_required" {
				status = http.StatusUnprocessableEntity
			}
			s.writeError(w, r, status, upstream.Code, "Upstream operation failed", upstream.Retryable)
			return
		}
		s.log.Error("request failed", "request_id", requestID(r), "error", err)
		s.writeError(w, r, http.StatusServiceUnavailable, "internal_error", "Operation could not be completed", true)
	}
}

func toSearchResponse(search domain.Search, r *http.Request) searchResponse {
	result := searchResponse{SearchID: search.ID, Subject: search.Subject, State: search.State, ExpiresAt: search.ExpiresAt, Releases: search.Results}
	if result.Releases == nil {
		result.Releases = []domain.Release{}
	}
	if search.ErrorCode != "" {
		result.Error = &errorBody{Code: search.ErrorCode, Message: search.ErrorMessage, Retryable: true, RequestID: requestID(r)}
	}
	return result
}

func toAcquisition(job domain.Job, r *http.Request) acquisitionResponse {
	result := acquisitionResponse{
		ID: job.ID, Subject: job.Subject, State: job.State, Title: job.Title,
		Progress: job.Progress.Percent, BytesDownloaded: job.Progress.DownloadedBytes,
		BytesTotal: job.Progress.TotalBytes, DownloadSpeedBytesPerSecond: job.Progress.BytesPerSecond,
		ETASeconds: job.Progress.ETASeconds, UpdatedAt: job.UpdatedAt,
	}
	if job.Progress.Text != "" {
		value := job.Progress.Text
		result.StatusText = &value
	}
	if job.Progress.Source != "" {
		value := job.Progress.Source
		result.ProgressSource = &value
	}
	if job.ErrorCode != "" {
		result.Error = &errorBody{Code: job.ErrorCode, Message: job.ErrorMessage, Retryable: job.State != domain.Failed, RequestID: requestID(r)}
	}
	if job.JellyfinItemID != "" {
		value := job.JellyfinItemID
		result.JellyfinItemID = &value
	}
	return result
}

func decodeJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("request has trailing JSON")
	}
	return nil
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, status int, code, message string, retryable bool) {
	s.writeJSON(w, status, errorEnvelope{Error: errorBody{Code: code, Message: message, Retryable: retryable, RequestID: requestID(r)}})
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := secure.Token(12)
		r.Header.Set("X-Request-ID", id)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'")
		next.ServeHTTP(w, r)
	})
}

func requestID(r *http.Request) string { return r.Header.Get("X-Request-ID") }

func bearerToken(header string) string {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return parts[1]
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func defaultValue(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

type bucket struct {
	count int
	reset time.Time
}
type limiter struct {
	mu     sync.Mutex
	values map[string]bucket
}

func newLimiter() *limiter { return &limiter{values: make(map[string]bucket)} }
func (l *limiter) allow(key string, maximum int, window time.Duration, now time.Time) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	value := l.values[key]
	if value.reset.Before(now) {
		value = bucket{reset: now.Add(window)}
	}
	if value.count >= maximum {
		retry := int(time.Until(value.reset).Seconds()) + 1
		if retry < 1 {
			retry = 1
		}
		return false, retry
	}
	value.count++
	l.values[key] = value
	return true, 0
}
func (s *Server) allow(w http.ResponseWriter, r *http.Request, key string, maximum int, window time.Duration) bool {
	ok, retry := s.limiter.allow(key, maximum, window, time.Now())
	if ok {
		return true
	}
	w.Header().Set("Retry-After", strconv.Itoa(retry))
	s.writeError(w, r, http.StatusTooManyRequests, "rate_limited", "Too many requests", true)
	return false
}

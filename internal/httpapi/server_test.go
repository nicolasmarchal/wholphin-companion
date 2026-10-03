package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/nicolasmarchal/wholphin-companion/internal/app"
	"github.com/nicolasmarchal/wholphin-companion/internal/domain"
	"github.com/nicolasmarchal/wholphin-companion/internal/secure"
	"github.com/nicolasmarchal/wholphin-companion/internal/store"
)

type fakeJellyfin struct{}

func (fakeJellyfin) Authenticate(_ context.Context, token string) (domain.User, error) {
	if token != "valid-jellyfin-token" {
		return domain.User{}, &domain.UpstreamError{Service: "jellyfin", Status: 401, Code: "invalid_session"}
	}
	return domain.User{ID: "user-id", Name: "Viewer"}, nil
}

type failingJellyfin struct{ err error }

func (failingJellyfin) Authenticate(context.Context, string) (domain.User, error) {
	return domain.User{}, nil
}
func (f failingJellyfin) Availability(context.Context, string, domain.Subject, []int) (domain.Availability, error) {
	return domain.Availability{}, f.err
}

type sessionErrorJellyfin struct{ err error }

func (f sessionErrorJellyfin) Authenticate(context.Context, string) (domain.User, error) {
	return domain.User{}, f.err
}
func (sessionErrorJellyfin) Availability(context.Context, string, domain.Subject, []int) (domain.Availability, error) {
	return domain.Availability{}, nil
}
func (fakeJellyfin) Availability(context.Context, string, domain.Subject, []int) (domain.Availability, error) {
	return domain.Availability{}, nil
}

type fakeArr struct{}

func (fakeArr) Resolve(context.Context, domain.Subject) (domain.ResolvedSubject, error) {
	return domain.ResolvedSubject{}, nil
}
func (fakeArr) Search(context.Context, domain.ResolvedSubject) ([]domain.Candidate, error) {
	return nil, nil
}
func (fakeArr) Grab(context.Context, json.RawMessage) error { return nil }
func (fakeArr) Observe(_ context.Context, job domain.Job) (domain.Observation, error) {
	return domain.Observation{State: job.State}, nil
}
func (fakeArr) Cancel(context.Context, domain.Job) error { return nil }

type fakeQB struct{}

func (fakeQB) Enabled() bool { return false }
func (fakeQB) Stats(context.Context, string) (domain.TorrentStats, error) {
	return domain.TorrentStats{}, nil
}

func TestSessionAuthenticationAndStructuredErrors(t *testing.T) {
	handler, _, _ := testServer(t)

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("missing bearer status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}
	assertErrorCode(t, unauthorized.Body.Bytes(), "invalid_session")

	missingJF := httptest.NewRecorder()
	handler.ServeHTTP(missingJF, httptest.NewRequest(http.MethodPost, "/v1/session", nil))
	if missingJF.Code != http.StatusUnauthorized {
		t.Fatalf("missing Jellyfin status=%d", missingJF.Code)
	}
	assertErrorCode(t, missingJF.Body.Bytes(), "missing_jellyfin_token")

	token := exchange(t, handler)
	capabilities := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	handler.ServeHTTP(capabilities, req)
	if capabilities.Code != http.StatusOK {
		t.Fatalf("capabilities status=%d body=%s", capabilities.Code, capabilities.Body.String())
	}
}

func TestSessionDistinguishesInvalidUnavailableAndTimeout(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want int
		code string
	}{
		{"invalid", &domain.UpstreamError{Service: "jellyfin", Status: 401, Code: "upstream_authentication_failed"}, http.StatusUnauthorized, "invalid_jellyfin_session"},
		{"unavailable", &domain.UpstreamError{Service: "jellyfin", Status: 503, Code: "upstream_http_503", Retryable: true}, http.StatusBadGateway, "jellyfin_unavailable"},
		{"timeout", &domain.UpstreamError{Service: "jellyfin", Code: "upstream_timeout", Retryable: true}, http.StatusGatewayTimeout, "jellyfin_timeout"},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, _, _ := testServerWithJellyfin(t, sessionErrorJellyfin{err: test.err})
			req := httptest.NewRequest(http.MethodPost, "/v1/session", nil)
			req.Header.Set("X-Jellyfin-Token", "some-token")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != test.want {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			assertErrorCode(t, response.Body.Bytes(), test.code)
		})
	}
}

func TestAcquireRejectsUnapprovedCandidateWith422(t *testing.T) {
	handler, st, now := testServer(t)
	token := exchange(t, handler)
	ctx := context.Background()
	search := domain.Search{ID: "search", UserID: "user-id", Subject: domain.Subject{Kind: domain.Movie, TMDBID: 42}, State: domain.SearchPending, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := st.CreateSearch(ctx, search); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimSearch(ctx, now); err != nil {
		t.Fatal(err)
	}
	candidate := domain.Candidate{Release: domain.Release{Title: "Unapproved.Release", Approved: false}, Payload: []byte(`{"guid":"not-approved"}`)}
	if err := st.CompleteSearch(ctx, search.ID, domain.ResolvedSubject{Subject: search.Subject, Backend: "radarr", ArrItemID: 1}, []domain.Candidate{candidate}, now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	result, err := st.Search(ctx, search.ID, "user-id", now)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"token": result.Results[0].Token})
	req := httptest.NewRequest(http.MethodPost, "/v1/acquisitions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Idempotency-Key", "handler-key-0000001")
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	assertErrorCode(t, response.Body.Bytes(), "release_rejected")
}

func TestAcquisitionRehydrationAcceptsTmdbWithoutKindAndIsUserScoped(t *testing.T) {
	handler, st, now := testServer(t)
	token := exchange(t, handler)
	ctx := context.Background()
	search := domain.Search{ID: "rehydrate-search", UserID: "user-id", Subject: domain.Subject{Kind: domain.Movie, TMDBID: 4242}, State: domain.SearchPending, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := st.CreateSearch(ctx, search); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimSearch(ctx, now); err != nil {
		t.Fatal(err)
	}
	candidate := domain.Candidate{Release: domain.Release{Title: "Selected", Indexer: "Test Indexer", Approved: true}, Payload: []byte(`{"guid":"selected","indexerId":1}`)}
	if err := st.CompleteSearch(ctx, search.ID, domain.ResolvedSubject{Subject: search.Subject, Backend: "radarr", ArrItemID: 2}, []domain.Candidate{candidate}, now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	result, err := st.Search(ctx, search.ID, "user-id", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReserveJob(ctx, "user-id", result.Results[0].Token, "rehydrate-key-0001", secure.Hash(result.Results[0].Token), now); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/acquisitions?active=true&tmdbId=4242", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var jobs []acquisitionResponse
	if err := json.Unmarshal(response.Body.Bytes(), &jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Subject.TMDBID != 4242 {
		t.Fatalf("jobs=%#v", jobs)
	}

	season := 1
	seriesSearch := domain.Search{ID: "series-rehydrate", UserID: "user-id", Subject: domain.Subject{Kind: domain.Season, TMDBID: 4242, SeasonNumber: &season}, State: domain.SearchPending, CreatedAt: now.Add(time.Second), UpdatedAt: now.Add(time.Second), ExpiresAt: now.Add(time.Hour)}
	if err := st.CreateSearch(ctx, seriesSearch); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimSearch(ctx, now); err != nil {
		t.Fatal(err)
	}
	seriesCandidate := domain.Candidate{Release: domain.Release{Title: "Series pack", Indexer: "Test Indexer", Approved: true, FullSeason: true}, Payload: []byte(`{"guid":"series","indexerId":2}`), ExpectedEpisodes: []int{1}}
	if err := st.CompleteSearch(ctx, seriesSearch.ID, domain.ResolvedSubject{Subject: seriesSearch.Subject, Backend: "sonarr", ArrItemID: 3, ExpectedEpisodes: []int{1}}, []domain.Candidate{seriesCandidate}, now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	seriesResult, err := st.Search(ctx, seriesSearch.ID, "user-id", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.ReserveJob(ctx, "user-id", seriesResult.Results[0].Token, "series-rehydrate-key", secure.Hash(seriesResult.Results[0].Token), now); err != nil {
		t.Fatal(err)
	}
	tvReq := httptest.NewRequest(http.MethodGet, "/v1/acquisitions?active=true&mediaType=tv&tmdbId=4242", nil)
	tvReq.Header.Set("Authorization", "Bearer "+token)
	tvResponse := httptest.NewRecorder()
	handler.ServeHTTP(tvResponse, tvReq)
	if tvResponse.Code != http.StatusOK {
		t.Fatalf("tv status=%d body=%s", tvResponse.Code, tvResponse.Body.String())
	}
	jobs = nil
	if err = json.Unmarshal(tvResponse.Body.Bytes(), &jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Subject.Kind != domain.Season {
		t.Fatalf("mediaType did not isolate TV namespace: %#v", jobs)
	}
}

func testServer(t *testing.T) (http.Handler, *store.Store, time.Time) {
	return testServerWithJellyfin(t, fakeJellyfin{})
}

func testServerWithJellyfin(t *testing.T, jellyfin domain.Jellyfin) (http.Handler, *store.Store, time.Time) {
	t.Helper()
	sealer, err := secure.NewSealer(bytes.Repeat([]byte{4}, 32))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "server.db"), sealer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	logger := slog.New(slog.NewTextHandler(ioDiscard{}, nil))
	application := app.New(st, fakeArr{}, fakeArr{}, jellyfin, fakeQB{}, app.Options{
		SessionTTL: time.Hour, SelectionTTL: 10 * time.Minute, SearchTimeout: time.Minute,
		UpstreamTimeout: time.Second, ReconcileEvery: time.Minute, AllowAllUsers: true,
	}, logger)
	return New(application, logger, "hook-secret"), st, time.Now().UTC()
}

func exchange(t *testing.T, handler http.Handler) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/session", nil)
	req.Header.Set("X-Jellyfin-Token", "valid-jellyfin-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusCreated {
		t.Fatalf("session status=%d body=%s", response.Code, response.Body.String())
	}
	var session struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &session); err != nil || session.Token == "" {
		t.Fatalf("invalid session: %v %s", err, response.Body.String())
	}
	return session.Token
}

func assertErrorCode(t *testing.T, body []byte, expected string) {
	t.Helper()
	var value struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatal(err)
	}
	if value.Error.Code != expected {
		t.Fatalf("error code=%q want=%q body=%s", value.Error.Code, expected, body)
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }

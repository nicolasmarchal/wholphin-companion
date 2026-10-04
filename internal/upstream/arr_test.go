package upstream

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nicolasmarchal/wholphin-companion/internal/config"
	"github.com/nicolasmarchal/wholphin-companion/internal/domain"
)

func TestRadarrExactReleasePayloadAndSafeProjection(t *testing.T) {
	var grabbed []byte
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/movie", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("tmdbId") != "123" {
			t.Errorf("tmdb query=%q", r.URL.RawQuery)
		}
		_, _ = io.WriteString(w, `[{"id":7,"tmdbId":123,"title":"Movie"}]`)
	})
	mux.HandleFunc("GET /api/v3/release", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("movieId") != "7" {
			t.Errorf("movie query=%q", r.URL.RawQuery)
		}
		_, _ = io.WriteString(w, `[{"guid":"exact-guid","indexerId":4,"title":"Movie.2160p-GRP","size":123456,"seeders":9,"indexer":"Prowlarr","protocol":"torrent","approved":true,"rejected":false,"quality":{"quality":{"name":"Bluray-2160p"}},"downloadUrl":"https://tracker/private-passkey","magnetUrl":"magnet:?xt=secret"}]`)
	})
	mux.HandleFunc("POST /api/v3/release", func(w http.ResponseWriter, r *http.Request) {
		grabbed, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	base, _ := url.Parse(server.URL)
	client := NewRadarr(config.Arr{URL: base, APIKey: "secret", RootFolder: "/movies", QualityProfileID: 1}, server.Client())
	resolved, err := client.Resolve(context.Background(), domain.Subject{Kind: domain.Movie, TMDBID: 123})
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := client.Search(context.Background(), resolved)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates=%d", len(candidates))
	}
	public, _ := json.Marshal(candidates[0].Release)
	if bytes.Contains(public, []byte("private-passkey")) || bytes.Contains(public, []byte("magnet:")) {
		t.Fatalf("public release leaked tracker data: %s", public)
	}
	if err := client.Grab(context.Background(), candidates[0].Payload); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.TrimSpace(grabbed), bytes.TrimSpace(candidates[0].Payload)) {
		t.Fatalf("grab was not exact\nwant %s\ngot  %s", candidates[0].Payload, grabbed)
	}
}

func TestNewRadarrMovieIsUnmonitoredWithoutAutomaticSearch(t *testing.T) {
	var added map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/movie", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `[]`) })
	mux.HandleFunc("GET /api/v3/movie/lookup", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[{"tmdbId":55,"title":"New Movie"}]`)
	})
	mux.HandleFunc("POST /api/v3/movie", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&added); err != nil {
			t.Fatal(err)
		}
		added["id"] = float64(8)
		_ = json.NewEncoder(w).Encode(added)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	base, _ := url.Parse(server.URL)
	client := NewRadarr(config.Arr{URL: base, APIKey: "secret", RootFolder: "/movies", QualityProfileID: 6}, server.Client())
	if _, err := client.Resolve(context.Background(), domain.Subject{Kind: domain.Movie, TMDBID: 55}); err != nil {
		t.Fatal(err)
	}
	if monitored, _ := added["monitored"].(bool); monitored {
		t.Fatal("BFF-created movie must be unmonitored")
	}
	options, _ := added["addOptions"].(map[string]any)
	if search, _ := options["searchForMovie"].(bool); search {
		t.Fatal("automatic movie search was enabled")
	}
	if options["monitor"] != "none" {
		t.Fatalf("monitor=%v", options["monitor"])
	}
}

func TestNewSonarrSeriesAndSeasonsAreUnmonitored(t *testing.T) {
	var added map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/series", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[]`)
	})
	mux.HandleFunc("GET /api/v3/series/lookup", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[{"tmdbId":66,"tvdbId":77,"title":"Show","seasons":[{"seasonNumber":1,"monitored":true},{"seasonNumber":2,"monitored":true}]}]`)
	})
	mux.HandleFunc("POST /api/v3/series", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&added); err != nil {
			t.Fatal(err)
		}
		added["id"] = float64(9)
		_ = json.NewEncoder(w).Encode(added)
	})
	mux.HandleFunc("GET /api/v3/episode", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[{"id":1,"seasonNumber":2,"episodeNumber":1}]`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	base, _ := url.Parse(server.URL)
	client := NewSonarr(config.Arr{URL: base, APIKey: "secret", RootFolder: "/tv", QualityProfileID: 4}, server.Client())
	season := 2
	if _, err := client.Resolve(context.Background(), domain.Subject{Kind: domain.Season, TMDBID: 66, SeasonNumber: &season}); err != nil {
		t.Fatal(err)
	}
	if monitored, _ := added["monitored"].(bool); monitored {
		t.Fatal("BFF-created series must be unmonitored")
	}
	for _, raw := range added["seasons"].([]any) {
		if raw.(map[string]any)["monitored"] != false {
			t.Fatalf("season remained monitored: %#v", raw)
		}
	}
	options := added["addOptions"].(map[string]any)
	if options["searchForMissingEpisodes"] != false || options["searchForCutoffUnmetEpisodes"] != false || options["monitor"] != "none" {
		t.Fatalf("unsafe Sonarr add options: %#v", options)
	}
}

func TestSonarrCorroboratesTMDbAndTVDbIdentityBeforeUse(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/series/lookup", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[{"tmdbId":66,"tvdbId":77,"title":"Correct show"}]`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	base, _ := url.Parse(server.URL)
	client := NewSonarr(config.Arr{URL: base, APIKey: "secret", RootFolder: "/tv", QualityProfileID: 4}, server.Client())
	season := 1
	_, err := client.Resolve(context.Background(), domain.Subject{Kind: domain.Season, TMDBID: 66, TVDBID: 999, SeasonNumber: &season})
	var upstreamErr *domain.UpstreamError
	if !errors.As(err, &upstreamErr) || upstreamErr.Code != "subject_identity_mismatch" {
		t.Fatalf("identity mismatch was accepted: %v", err)
	}
}

func TestSonarrRequeriesCanonicalSeriesBeforeAdd(t *testing.T) {
	posts := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/series/lookup", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[{"tmdbId":66,"tvdbId":77,"title":"Show"}]`)
	})
	mux.HandleFunc("GET /api/v3/series", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[{"id":9,"tmdbId":66,"tvdbId":77,"title":"Show"}]`)
	})
	mux.HandleFunc("POST /api/v3/series", func(w http.ResponseWriter, _ *http.Request) {
		posts++
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("GET /api/v3/episode", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[{"id":1,"seasonNumber":1,"episodeNumber":1}]`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	base, _ := url.Parse(server.URL)
	client := NewSonarr(config.Arr{URL: base, APIKey: "secret", RootFolder: "/tv", QualityProfileID: 4}, server.Client())
	season := 1
	resolved, err := client.Resolve(context.Background(), domain.Subject{Kind: domain.Season, TMDBID: 66, SeasonNumber: &season})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ArrItemID != 9 || posts != 0 {
		t.Fatalf("canonical series was not reused: resolved=%#v posts=%d", resolved, posts)
	}
}

func TestSeasonSearchMakesPartialReleasesUnselectable(t *testing.T) {
	season := 4
	resolved := domain.ResolvedSubject{Subject: domain.Subject{Kind: domain.Season, TMDBID: 1, SeasonNumber: &season}, ExpectedEpisodes: []int{1, 2, 3}}
	tests := []struct {
		name, raw string
		rejected  bool
	}{
		{"single episode", `{"guid":"a","indexerId":1,"indexer":"Test","title":"S04E01","approved":true,"fullSeason":false,"seasonNumber":4,"episodeNumbers":[1]}`, true},
		{"incomplete pack", `{"guid":"b","indexerId":1,"indexer":"Test","title":"S04 pack partial","approved":true,"fullSeason":true,"seasonNumber":4,"episodeNumbers":[1,2]}`, true},
		{"wrong season", `{"guid":"c","indexerId":1,"indexer":"Test","title":"S03 pack","approved":true,"fullSeason":true,"seasonNumber":3,"episodeNumbers":[1,2,3]}`, true},
		{"complete pack", `{"guid":"d","indexerId":1,"indexer":"Test","title":"S04 pack","approved":true,"fullSeason":true,"seasonNumber":4,"episodeNumbers":[1,2,3]}`, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate, err := parseRelease(json.RawMessage(test.raw), resolved)
			if err != nil {
				t.Fatal(err)
			}
			if candidate.Rejected != test.rejected {
				t.Fatalf("rejected=%v reasons=%v", candidate.Rejected, candidate.RejectionReasons)
			}
			if test.rejected && candidate.Approved {
				t.Fatal("partial season release remained approved")
			}
		})
	}
}

func TestReleaseRejectionsAreProjectedWithoutSecrets(t *testing.T) {
	resolved := domain.ResolvedSubject{Subject: domain.Subject{Kind: domain.Movie, TMDBID: 1}}
	candidate, err := parseRelease(json.RawMessage(`{"guid":"g","indexerId":1,"indexer":"Test","title":"Release","approved":false,"rejected":true,"rejections":["Download URL https://tracker.invalid/a?passkey=secret magnet:?xt=secret /private/path"]}`), resolved)
	if err != nil {
		t.Fatal(err)
	}
	public, _ := json.Marshal(candidate.Release)
	for _, secret := range []string{"tracker.invalid", "passkey", "magnet:", "/private/path"} {
		if bytes.Contains(public, []byte(secret)) {
			t.Fatalf("rejection leaked %q: %s", secret, public)
		}
	}
}

func TestSoftArrPolicyRejectionsRemainSelectableByExplicitOverride(t *testing.T) {
	resolved := domain.ResolvedSubject{Subject: domain.Subject{Kind: domain.Movie, TMDBID: 1}}
	for _, rejection := range []string{
		"Quality is not wanted in profile",
		"Custom format score is below minimum",
		"Not enough seeders",
		"Release is blocklisted",
		"28.3 GB is larger than maximum allowed 10.6 GB (for Example)",
	} {
		t.Run(rejection, func(t *testing.T) {
			candidate, err := parseRelease(json.RawMessage(`{"guid":"g","indexerId":1,"indexer":"Test","title":"Release","approved":false,"rejected":true,"rejections":[`+strconv.Quote(rejection)+`]}`), resolved)
			if err != nil {
				t.Fatal(err)
			}
			if !candidate.PolicyOverrideAllowed {
				t.Fatalf("soft rejection was not overrideable: %#v", candidate.Release)
			}
		})
	}
}

func TestOversizedReleaseRejectionIsPreciseAndSelectable(t *testing.T) {
	resolved := domain.ResolvedSubject{Subject: domain.Subject{Kind: domain.Movie, TMDBID: 1}}
	candidate, err := parseRelease(json.RawMessage(`{"guid":"g","indexerId":1,"indexer":"Test","title":"Release","approved":false,"rejected":true,"rejections":["28.3 GB is larger than maximum allowed 10.6 GB (for Example)"]}`), resolved)
	if err != nil {
		t.Fatal(err)
	}
	if !candidate.PolicyOverrideAllowed {
		t.Fatalf("oversized release was not overrideable: %#v", candidate.Release)
	}
	want := []string{"Release size 28.3 GB exceeds the configured maximum of 10.6 GB"}
	if !slices.Equal(candidate.RejectionReasons, want) {
		t.Fatalf("rejections=%q want=%q", candidate.RejectionReasons, want)
	}
}

func TestUnknownOrMixedArrRejectionsCannotBeOverridden(t *testing.T) {
	resolved := domain.ResolvedSubject{Subject: domain.Subject{Kind: domain.Movie, TMDBID: 1}}
	for _, raw := range []string{
		`{"guid":"g","indexerId":1,"indexer":"Test","title":"Release","approved":false,"rejected":true}`,
		`{"guid":"g","indexerId":1,"indexer":"Test","title":"Release","approved":false,"rejected":true,"rejections":["Not enough seeders","Unable to parse movie"]}`,
	} {
		candidate, err := parseRelease(json.RawMessage(raw), resolved)
		if err != nil {
			t.Fatal(err)
		}
		if candidate.PolicyOverrideAllowed {
			t.Fatalf("hard or ambiguous rejection became overrideable: %#v", candidate.Release)
		}
	}
}

func TestSubjectCoverageFailureCannotBeOverriddenBySoftArrPolicy(t *testing.T) {
	season := 2
	resolved := domain.ResolvedSubject{
		Subject:          domain.Subject{Kind: domain.Season, TMDBID: 1, SeasonNumber: &season},
		ExpectedEpisodes: []int{1, 2},
	}
	candidate, err := parseRelease(json.RawMessage(`{"guid":"g","indexerId":1,"indexer":"Test","title":"Wrong season","approved":false,"rejected":true,"rejections":["Not enough seeders"],"fullSeason":true,"seasonNumber":3,"episodeNumbers":[1,2]}`), resolved)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.PolicyOverrideAllowed {
		t.Fatalf("subject mismatch became overrideable: %#v", candidate.Release)
	}
}

func TestSoftPolicySeasonOverrideKeepsCanonicalExpectedEpisodes(t *testing.T) {
	season := 2
	resolved := domain.ResolvedSubject{
		Subject:          domain.Subject{Kind: domain.Season, TMDBID: 1, SeasonNumber: &season},
		ExpectedEpisodes: []int{1, 2},
	}
	candidate, err := parseRelease(json.RawMessage(`{"guid":"g","indexerId":1,"indexer":"Test","title":"Season pack","approved":false,"rejected":true,"rejections":["Quality is not wanted in profile"],"fullSeason":true,"seasonNumber":2,"episodeNumbers":[1,2,99]}`), resolved)
	if err != nil {
		t.Fatal(err)
	}
	if !candidate.PolicyOverrideAllowed || !slices.Equal(candidate.ExpectedEpisodes, resolved.ExpectedEpisodes) {
		t.Fatalf("canonical season coverage was not retained: %#v", candidate)
	}
}

func TestEpisodeSearchRequiresExactSeasonAndEpisodeCoverage(t *testing.T) {
	season, episode := 4, 2
	resolved := domain.ResolvedSubject{Subject: domain.Subject{Kind: domain.Episode, TMDBID: 1, SeasonNumber: &season, EpisodeNumber: &episode}}
	for _, test := range []struct {
		name string
		raw  string
		ok   bool
	}{
		{"exact", `{"guid":"a","indexerId":1,"indexer":"Test","title":"S04E02","approved":true,"seasonNumber":4,"episodeNumbers":[2]}`, true},
		{"wrong season", `{"guid":"b","indexerId":1,"indexer":"Test","title":"S03E02","approved":true,"seasonNumber":3,"episodeNumbers":[2]}`, false},
		{"wrong episode", `{"guid":"c","indexerId":1,"indexer":"Test","title":"S04E03","approved":true,"seasonNumber":4,"episodeNumbers":[3]}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate, err := parseRelease(json.RawMessage(test.raw), resolved)
			if err != nil {
				t.Fatal(err)
			}
			if got := candidate.Approved && !candidate.Rejected; got != test.ok {
				t.Fatalf("selectable=%v reasons=%v", got, candidate.RejectionReasons)
			}
		})
	}
}

func TestMappedSeasonNumberTakesPrecedenceForSceneRelease(t *testing.T) {
	season := 2
	resolved := domain.ResolvedSubject{Subject: domain.Subject{Kind: domain.Season, TMDBID: 1, SeasonNumber: &season}, ExpectedEpisodes: []int{1}}
	candidate, err := parseRelease(json.RawMessage(`{"guid":"scene","indexerId":1,"indexer":"Test","title":"Scene absolute mapping","approved":true,"fullSeason":true,"seasonNumber":1,"mappedSeasonNumber":2,"mappedEpisodeNumbers":[1]}`), resolved)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Rejected || !candidate.Approved || candidate.SeasonNumber == nil || *candidate.SeasonNumber != 2 {
		t.Fatalf("mapped season was ignored: %#v", candidate.Release)
	}
}

func TestObserveCorrelatesStrongFingerprintThenDownloadID(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/queue", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"records":[`+
			`{"id":1,"movieId":7,"title":"Different.Release","downloadId":"wrong","status":"downloading","size":100,"sizeleft":50},`+
			`{"id":2,"movieId":7,"title":"Selected.Release","downloadId":"right","status":"downloading","size":100,"sizeleft":25}`+
			`]}`)
	})
	mux.HandleFunc("GET /api/v3/history/movie", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[{"downloadId":"right","sourceTitle":"Selected.Release","eventType":"grabbed","date":"`+time.Now().UTC().Format(time.RFC3339Nano)+`","data":{"guid":"selected-guid","indexer":"Selected Indexer"}}]`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	base, _ := url.Parse(server.URL)
	client := NewRadarr(config.Arr{URL: base, APIKey: "secret", RootFolder: "/movies", QualityProfileID: 1}, server.Client())
	digest := sha256.Sum256([]byte("selected-guid"))
	indexerDigest := sha256.Sum256([]byte("selected indexer"))
	job := domain.Job{Title: "Selected.Release", Backend: "radarr", ArrItemID: 7, CreatedAt: time.Now().Add(-time.Minute), State: domain.DispatchUncertain, ReleaseGUIDHash: digest[:], ReleaseIndexerID: 4, ReleaseIndexerHash: indexerDigest[:]}
	observation, err := client.Observe(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	if observation.DownloadID != "right" {
		t.Fatalf("history did not establish strong identity: %#v", observation)
	}
	job.DownloadID = observation.DownloadID
	observation, err = client.Observe(context.Background(), job)
	if err != nil || observation.QueueID != 2 || observation.DownloadID != "right" {
		t.Fatalf("wrong queue association after strong match: %#v err=%v", observation, err)
	}
}

func TestObserveDoesNotCorrelateSameTitleWithDifferentGUID(t *testing.T) {
	now := time.Now().UTC()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/history/movie", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[{"downloadId":"wrong","sourceTitle":"Same.Title","eventType":"grabbed","date":"`+now.Format(time.RFC3339Nano)+`","data":{"guid":"other-guid","indexerId":4}}]`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	base, _ := url.Parse(server.URL)
	client := NewRadarr(config.Arr{URL: base, APIKey: "secret", RootFolder: "/movies", QualityProfileID: 1}, server.Client())
	digest := sha256.Sum256([]byte("selected-guid"))
	job := domain.Job{Title: "Same.Title", ArrItemID: 7, CreatedAt: now.Add(-time.Minute), State: domain.DispatchUncertain, ReleaseGUIDHash: digest[:], ReleaseIndexerID: 4}
	observation, err := client.Observe(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	if observation.DownloadID != "" || observation.State != domain.DispatchUncertain {
		t.Fatalf("different GUID was silently substituted: %#v", observation)
	}
}

func TestObserveRequiresGUIDAndSecondHistoryIdentifier(t *testing.T) {
	now := time.Now().UTC()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/history/movie", func(w http.ResponseWriter, _ *http.Request) {
		// Real Arr grabbed history exposes indexer name, not indexerId.
		_, _ = io.WriteString(w, `[{"downloadId":"wrong","sourceTitle":"Same.Title","eventType":"grabbed","date":"`+now.Format(time.RFC3339Nano)+`","data":{"guid":"shared-guid","indexer":"Other Indexer"}}]`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	base, _ := url.Parse(server.URL)
	client := NewRadarr(config.Arr{URL: base, APIKey: "secret", RootFolder: "/movies", QualityProfileID: 1}, server.Client())
	guidHash := sha256.Sum256([]byte("shared-guid"))
	indexerHash := sha256.Sum256([]byte("selected indexer"))
	job := domain.Job{Title: "Same.Title", ArrItemID: 7, CreatedAt: now.Add(-time.Minute), State: domain.DispatchUncertain, ReleaseGUIDHash: guidHash[:], ReleaseIndexerHash: indexerHash[:]}
	observation, err := client.Observe(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	if observation.DownloadID != "" || observation.State != domain.DispatchUncertain {
		t.Fatalf("GUID collision across indexers was accepted: %#v", observation)
	}
}

func TestObserveDoesNotAssociateAmbiguousSameMovie(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/queue", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"records":[{"id":1,"movieId":7,"title":"Other.Release","downloadId":"wrong","status":"downloading"}]}`)
	})
	mux.HandleFunc("GET /api/v3/history/movie", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `[]`) })
	server := httptest.NewServer(mux)
	defer server.Close()
	base, _ := url.Parse(server.URL)
	client := NewRadarr(config.Arr{URL: base, APIKey: "secret", RootFolder: "/movies", QualityProfileID: 1}, server.Client())
	job := domain.Job{Title: "Selected.Release", Backend: "radarr", ArrItemID: 7, CreatedAt: time.Now(), State: domain.DispatchUncertain}
	observation, err := client.Observe(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	if observation.State != domain.DispatchUncertain || strings.TrimSpace(observation.DownloadID) != "" {
		t.Fatalf("ambiguous record was associated: %#v", observation)
	}
}

func TestSeasonImportRequiresAllEpisodes(t *testing.T) {
	now := time.Now().UTC()
	historyCalls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/queue", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"records":[]}`) })
	mux.HandleFunc("GET /api/v3/history/series", func(w http.ResponseWriter, _ *http.Request) {
		historyCalls++
		events := []map[string]any{{
			"downloadId": "known-hash", "sourceTitle": "Chosen.S02.Pack", "eventType": "episodeFileImported", "date": now,
			"episode": map[string]any{"seasonNumber": 2, "episodeNumber": 1},
		}}
		if historyCalls > 1 {
			events = append(events, map[string]any{
				"downloadId": "known-hash", "sourceTitle": "Chosen.S02.Pack", "eventType": "episodeFileImported", "date": now,
				"episode": map[string]any{"seasonNumber": 2, "episodeNumber": 2},
			})
		}
		_ = json.NewEncoder(w).Encode(events)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	base, _ := url.Parse(server.URL)
	client := NewSonarr(config.Arr{URL: base, APIKey: "secret", RootFolder: "/tv", QualityProfileID: 1}, server.Client())
	season := 2
	job := domain.Job{
		Subject: domain.Subject{Kind: domain.Season, TMDBID: 1, SeasonNumber: &season},
		Title:   "Chosen.S02.Pack", ArrItemID: 8, DownloadID: "known-hash", CreatedAt: now.Add(-time.Minute),
		State: domain.Importing, ExpectedEpisodes: []int{1, 2},
	}
	first, err := client.Observe(context.Background(), job)
	if err != nil || first.State != domain.Importing {
		t.Fatalf("first observation=%#v err=%v", first, err)
	}
	second, err := client.Observe(context.Background(), job)
	if err != nil || second.State != domain.Imported {
		t.Fatalf("second observation=%#v err=%v", second, err)
	}
}

func TestBlankDownloadIDImportFailsClosed(t *testing.T) {
	now := time.Now().UTC()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/queue", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"records":[]}`) })
	mux.HandleFunc("GET /api/v3/history/series", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[{"downloadId":"","sourceTitle":"Chosen.S02.Pack","eventType":"episodeFileImported","date":"`+now.Format(time.RFC3339Nano)+`","episode":{"seasonNumber":2,"episodeNumber":1}}]`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	base, _ := url.Parse(server.URL)
	client := NewSonarr(config.Arr{URL: base, APIKey: "secret", RootFolder: "/tv", QualityProfileID: 1}, server.Client())
	season := 2
	job := domain.Job{Subject: domain.Subject{Kind: domain.Season, TMDBID: 1, SeasonNumber: &season}, Title: "Chosen.S02.Pack", ArrItemID: 8, DownloadID: "known-hash", CreatedAt: now.Add(-time.Minute), State: domain.Importing, ExpectedEpisodes: []int{1}}
	observation, err := client.Observe(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	if observation.State == domain.Imported {
		t.Fatalf("unattributed blank-downloadId import was accepted: %#v", observation)
	}
}

func TestQueueStateMapping(t *testing.T) {
	tests := []struct {
		name   string
		record queueRecord
		want   domain.JobState
	}{
		{"downloading", queueRecord{Status: "downloading", Size: 100, SizeLeft: 40}, domain.Downloading},
		{"blocked", queueRecord{Status: "downloading", TrackedDownloadStatus: "warning", Size: 100, SizeLeft: 40}, domain.DownloadBlocked},
		{"verifying", queueRecord{Status: "completed", Size: 100}, domain.Verifying},
		{"importing", queueRecord{Status: "completed", TrackedDownloadState: "importPending", Size: 100}, domain.Importing},
		{"import failed", queueRecord{Status: "completed", TrackedDownloadStatus: "error", Size: 100}, domain.ImportFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.record.observation(); got.State != test.want {
				t.Fatalf("got state %q, want %q", got.State, test.want)
			}
		})
	}
}

func TestQueueProgressIsClamped(t *testing.T) {
	for _, record := range []queueRecord{
		{Status: "downloading", Size: 100, SizeLeft: -50},
		{Status: "downloading", Size: 100, SizeLeft: 200},
	} {
		observation := record.observation()
		if observation.Progress.Percent == nil || *observation.Progress.Percent < 0 || *observation.Progress.Percent > 100 {
			t.Fatalf("percent not clamped: %#v", observation.Progress)
		}
		if observation.Progress.DownloadedBytes == nil || *observation.Progress.DownloadedBytes < 0 || *observation.Progress.DownloadedBytes > 100 {
			t.Fatalf("bytes not clamped: %#v", observation.Progress)
		}
	}
}

func TestQueueMessagesNeverExposeUpstreamSecrets(t *testing.T) {
	record := queueRecord{
		Status: "completed", TrackedDownloadStatus: "error", Size: 100,
		ErrorMessage: `import failed for /media/private and magnet:?xt=urn:btih:secret`,
	}
	record.StatusMessages = append(record.StatusMessages, struct {
		Title    string   `json:"title"`
		Messages []string `json:"messages"`
	}{Title: "https://tracker.invalid/download?passkey=secret"})
	observation := record.observation()
	public, _ := json.Marshal(observation)
	for _, secret := range []string{"/media/private", "magnet:", "passkey=secret", "tracker.invalid"} {
		if bytes.Contains(public, []byte(secret)) {
			t.Fatalf("public observation leaked %q: %s", secret, public)
		}
	}
}

func TestMutatingFiveHundredIsAmbiguousAndNeverFollowRedirect(t *testing.T) {
	var redirected bool
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected = true }))
	defer redirectTarget.Close()

	t.Run("server error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "failed", http.StatusBadGateway) }))
		defer server.Close()
		base, _ := url.Parse(server.URL)
		client := NewRadarr(config.Arr{URL: base, APIKey: "secret", RootFolder: "/movies", QualityProfileID: 1}, server.Client())
		err := client.Grab(context.Background(), json.RawMessage(`{"guid":"x"}`))
		var upstreamErr *domain.UpstreamError
		if !errors.As(err, &upstreamErr) || !upstreamErr.Ambiguous {
			t.Fatalf("error must be ambiguous: %#v", err)
		}
	})

	t.Run("redirect", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Redirect(w, &http.Request{}, redirectTarget.URL, http.StatusTemporaryRedirect)
		}))
		defer server.Close()
		base, _ := url.Parse(server.URL)
		client := NewRadarr(config.Arr{URL: base, APIKey: "secret", RootFolder: "/movies", QualityProfileID: 1}, server.Client())
		if err := client.Grab(context.Background(), json.RawMessage(`{"guid":"x"}`)); err == nil {
			t.Fatal("redirect unexpectedly succeeded")
		}
		if redirected {
			t.Fatal("secret-bearing request followed a redirect")
		}
	})
}

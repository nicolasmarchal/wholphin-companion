package upstream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/nicolasmarchal/wholphin-companion/internal/domain"
)

func TestJellyfinAvailabilityUsesExplicitUserAndPlaybackInfo(t *testing.T) {
	const userID = "jellyfin-user"
	mux := http.NewServeMux()
	mux.HandleFunc("GET /Items", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("UserId") != userID {
			t.Errorf("missing user scope: %s", r.URL.RawQuery)
		}
		if r.Header.Get("X-Emby-Token") != "server-key" {
			t.Error("missing server-side credential")
		}
		_, _ = io.WriteString(w, `{"Items":[{"Id":"movie-item","ProviderIds":{"Tmdb":"42"}}]}`)
	})
	mux.HandleFunc("POST /Items/movie-item/PlaybackInfo", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("UserId") != userID {
			t.Errorf("playback check not scoped to user: %s", r.URL.RawQuery)
		}
		_, _ = io.WriteString(w, `{"MediaSources":[{"Id":"media-source","SupportsDirectPlay":true}]}`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	base, _ := url.Parse(server.URL)
	client := NewJellyfin(base, "server-key", server.Client())
	available, err := client.Availability(context.Background(), userID, domain.Subject{Kind: domain.Movie, TMDBID: 42}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !available.Available || available.ItemID != "movie-item" {
		t.Fatalf("availability=%#v", available)
	}
}

func TestJellyfinTVReturnsSeriesOnlyAfterEveryEpisodeIsPlayable(t *testing.T) {
	const userID = "user"
	failedEpisode := ""
	mux := http.NewServeMux()
	mux.HandleFunc("GET /Items", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("IncludeItemTypes") == "Series" {
			_, _ = io.WriteString(w, `{"Items":[{"Id":"series-item","ProviderIds":{"Tmdb":"77"}}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"Items":[{"Id":"e1","ParentIndexNumber":2,"IndexNumber":1},{"Id":"e2","ParentIndexNumber":2,"IndexNumber":2}]}`)
	})
	mux.HandleFunc("POST /Items/{item}/PlaybackInfo", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("item") == failedEpisode {
			_, _ = io.WriteString(w, `{"MediaSources":[]}`)
			return
		}
		_, _ = io.WriteString(w, `{"MediaSources":[{"Id":"source","SupportsTranscoding":true}]}`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	base, _ := url.Parse(server.URL)
	client := NewJellyfin(base, "server-key", server.Client())
	season, episode := 2, 1
	result, err := client.Availability(context.Background(), userID, domain.Subject{Kind: domain.Episode, TMDBID: 77, SeasonNumber: &season, EpisodeNumber: &episode}, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Available || result.ItemID != "series-item" {
		t.Fatalf("TV target must be series: %#v", result)
	}
	failedEpisode = "e2"
	result, err = client.Availability(context.Background(), userID, domain.Subject{Kind: domain.Season, TMDBID: 77, SeasonNumber: &season}, []int{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	if result.Available {
		t.Fatal("season became available with an unplayable expected episode")
	}
}

func TestJellyfinTVFallsBackToTVDbAndPropagatesPlaybackFailure(t *testing.T) {
	const userID = "user"
	seriesQueries := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /Items", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("IncludeItemTypes") == "Series" {
			seriesQueries++
			if strings.Contains(r.URL.Query().Get("AnyProviderIdEquals"), "Tvdb.987") {
				_, _ = io.WriteString(w, `{"Items":[{"Id":"series-tvdb","ProviderIds":{"Tvdb":"987"}}]}`)
				return
			}
			_, _ = io.WriteString(w, `{"Items":[]}`)
			return
		}
		_, _ = io.WriteString(w, `{"Items":[{"Id":"episode","ParentIndexNumber":1,"IndexNumber":1}]}`)
	})
	mux.HandleFunc("POST /Items/episode/PlaybackInfo", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	base, _ := url.Parse(server.URL)
	client := NewJellyfin(base, "server-key", server.Client())
	season, episode := 1, 1
	result, err := client.Availability(context.Background(), userID, domain.Subject{Kind: domain.Episode, TMDBID: 77, TVDBID: 987, SeasonNumber: &season, EpisodeNumber: &episode}, []int{1})
	if err == nil || result.Available {
		t.Fatalf("PlaybackInfo failure was treated as absence: result=%#v err=%v", result, err)
	}
	if seriesQueries != 2 {
		t.Fatalf("expected TMDb then TVDb lookup, got %d queries", seriesQueries)
	}
}

func TestJellyfinTVTriesAllMatchingLibrariesAndEpisodeVersions(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /Items", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("IncludeItemTypes") == "Series" {
			_, _ = io.WriteString(w, `{"Items":[{"Id":"series-bad","ProviderIds":{"Tmdb":"77"}},{"Id":"series-good","ProviderIds":{"Tmdb":"77"}}]}`)
			return
		}
		switch r.URL.Query().Get("ParentId") {
		case "series-bad":
			_, _ = io.WriteString(w, `{"Items":[{"Id":"bad-e1","ParentIndexNumber":2,"IndexNumber":1}]}`)
		case "series-good":
			_, _ = io.WriteString(w, `{"Items":[{"Id":"unplayable-version","ParentIndexNumber":2,"IndexNumber":1},{"Id":"playable-version","ParentIndexNumber":2,"IndexNumber":1},{"Id":"good-e2","ParentIndexNumber":2,"IndexNumber":2}]}`)
		}
	})
	mux.HandleFunc("POST /Items/{item}/PlaybackInfo", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("item") == "playable-version" || r.PathValue("item") == "good-e2" {
			_, _ = io.WriteString(w, `{"MediaSources":[{"Id":"source","SupportsDirectPlay":true}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"MediaSources":[]}`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	base, _ := url.Parse(server.URL)
	client := NewJellyfin(base, "server-key", server.Client())
	season := 2
	result, err := client.Availability(context.Background(), "user", domain.Subject{Kind: domain.Season, TMDBID: 77, SeasonNumber: &season}, []int{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Available || result.ItemID != "series-good" {
		t.Fatalf("did not select complete playable library: %#v", result)
	}
}

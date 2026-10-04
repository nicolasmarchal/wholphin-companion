package upstream

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestQBittorrentStatsUsesWhitelistWithoutLogin(t *testing.T) {
	loginCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/auth/login":
			loginCalls++
			http.Error(w, "login must not be called", http.StatusInternalServerError)
		case "/api/v2/torrents/info":
			if got := r.URL.Query().Get("hashes"); got != "ABC123" {
				t.Fatalf("hashes=%q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `[{"progress":0.25,"downloaded":250,"total_size":1000,"dlspeed":50,"eta":15,"state":"downloading"}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewQBittorrent(base, "", "", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	stats, err := client.Stats(context.Background(), "ABC123")
	if err != nil {
		t.Fatal(err)
	}
	if loginCalls != 0 {
		t.Fatalf("login calls=%d", loginCalls)
	}
	if stats.Progress != 0.25 || stats.DownloadedBytes != 250 || stats.TotalBytes != 1000 || stats.BytesPerSecond != 50 || stats.ETASeconds != 15 {
		t.Fatalf("unexpected stats: %#v", stats)
	}
}

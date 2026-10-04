package config

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestProgressSchedulingDefaultsAndCanBeConfigured(t *testing.T) {
	setValidEnvironment(t)
	t.Setenv("BFF_PROGRESS_INTERVAL", "")
	t.Setenv("BFF_RECONCILE_WORKERS", "")
	configuration, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if configuration.ProgressEvery != 2*time.Second {
		t.Fatalf("default progress interval=%s", configuration.ProgressEvery)
	}
	if configuration.ReconcileWorkers != 4 {
		t.Fatalf("default reconcile workers=%d", configuration.ReconcileWorkers)
	}

	t.Setenv("BFF_PROGRESS_INTERVAL", "3s")
	t.Setenv("BFF_RECONCILE_WORKERS", "2")
	configuration, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if configuration.ProgressEvery != 3*time.Second {
		t.Fatalf("configured progress interval=%s", configuration.ProgressEvery)
	}
	if configuration.ReconcileWorkers != 2 {
		t.Fatalf("configured reconcile workers=%d", configuration.ReconcileWorkers)
	}
}

func TestQBittorrentAllowsExplicitAuthlessNetworkPolicy(t *testing.T) {
	setValidEnvironment(t)
	t.Setenv("QBITTORRENT_URL", "http://qbittorrent:8080")

	configuration, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if configuration.QBittorrentURL == nil || configuration.QBittorrentUser != "" || configuration.QBittorrentPass != "" {
		t.Fatalf("unexpected qBittorrent config: %#v", configuration)
	}
}

func TestQBittorrentRejectsPartialCredentials(t *testing.T) {
	setValidEnvironment(t)
	t.Setenv("QBITTORRENT_URL", "http://qbittorrent:8080")
	t.Setenv("QBITTORRENT_USERNAME", "admin")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "both be set or both be empty") {
		t.Fatalf("error=%v", err)
	}
}

func setValidEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("BFF_DATA_KEY_FILE", "")
	t.Setenv("BFF_DATA_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)))
	t.Setenv("BFF_ALLOW_ALL_JELLYFIN_USERS", "true")
	t.Setenv("BFF_WEBHOOK_SECRET_FILE", "")
	t.Setenv("BFF_WEBHOOK_SECRET", "")
	t.Setenv("JELLYFIN_URL", "http://jellyfin:8096")
	t.Setenv("JELLYFIN_API_KEY_FILE", "")
	t.Setenv("JELLYFIN_API_KEY", "jellyfin-key")
	t.Setenv("QBITTORRENT_URL", "")
	t.Setenv("QBITTORRENT_USERNAME_FILE", "")
	t.Setenv("QBITTORRENT_USERNAME", "")
	t.Setenv("QBITTORRENT_PASSWORD_FILE", "")
	t.Setenv("QBITTORRENT_PASSWORD", "")
	for _, prefix := range []string{"RADARR", "SONARR"} {
		t.Setenv(prefix+"_URL", "http://"+prefix+":8080")
		t.Setenv(prefix+"_API_KEY_FILE", "")
		t.Setenv(prefix+"_API_KEY", "arr-key")
		t.Setenv(prefix+"_ROOT_FOLDER", "/media")
		t.Setenv(prefix+"_QUALITY_PROFILE_ID", "1")
	}
}

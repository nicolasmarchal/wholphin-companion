package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Arr struct {
	URL              *url.URL
	APIKey           string
	RootFolder       string
	QualityProfileID int
}

type Config struct {
	ListenAddr       string
	DatabasePath     string
	DataKey          []byte
	AllowedUsers     map[string]struct{}
	AllowAllUsers    bool
	AllowCancel      bool
	SessionTTL       time.Duration
	SelectionTTL     time.Duration
	ReconcileEvery   time.Duration
	ProgressEvery    time.Duration
	ReconcileWorkers int
	SearchTimeout    time.Duration
	UpstreamTimeout  time.Duration
	WebhookSecret    string
	Radarr           Arr
	Sonarr           Arr
	JellyfinURL      *url.URL
	JellyfinAPIKey   string
	QBittorrentURL   *url.URL
	QBittorrentUser  string
	QBittorrentPass  string
}

func Load() (Config, error) {
	dataKeyText, err := secret("BFF_DATA_KEY")
	if err != nil {
		return Config{}, err
	}
	jellyfinKey, err := secret("JELLYFIN_API_KEY")
	if err != nil {
		return Config{}, err
	}
	webhookSecret, err := secret("BFF_WEBHOOK_SECRET")
	if err != nil {
		return Config{}, err
	}
	qbUser, err := secret("QBITTORRENT_USERNAME")
	if err != nil {
		return Config{}, err
	}
	qbPass, err := secret("QBITTORRENT_PASSWORD")
	if err != nil {
		return Config{}, err
	}
	c := Config{
		ListenAddr:       env("BFF_LISTEN_ADDR", "127.0.0.1:8090"),
		DatabasePath:     env("BFF_DATABASE_PATH", "companion.db"),
		AllowAllUsers:    envBool("BFF_ALLOW_ALL_JELLYFIN_USERS", false),
		AllowCancel:      envBool("BFF_ALLOW_CANCEL", false),
		SessionTTL:       envDuration("BFF_SESSION_TTL", 12*time.Hour),
		SelectionTTL:     envDuration("BFF_SELECTION_TTL", 10*time.Minute),
		ReconcileEvery:   envDuration("BFF_RECONCILE_INTERVAL", 10*time.Second),
		ProgressEvery:    envDuration("BFF_PROGRESS_INTERVAL", 2*time.Second),
		ReconcileWorkers: envPositiveInt("BFF_RECONCILE_WORKERS", 4),
		SearchTimeout:    envDuration("BFF_SEARCH_TIMEOUT", 2*time.Minute),
		UpstreamTimeout:  envDuration("BFF_UPSTREAM_TIMEOUT", 30*time.Second),
		WebhookSecret:    webhookSecret,
		JellyfinAPIKey:   jellyfinKey,
		QBittorrentUser:  qbUser,
		QBittorrentPass:  qbPass,
	}

	key, err := base64.StdEncoding.DecodeString(dataKeyText)
	if err != nil || len(key) != 32 {
		return Config{}, errors.New("BFF_DATA_KEY must be base64 for exactly 32 bytes")
	}
	c.DataKey = key
	c.AllowedUsers = csvSet(os.Getenv("BFF_ALLOWED_JELLYFIN_USER_IDS"))
	if !c.AllowAllUsers && len(c.AllowedUsers) == 0 {
		return Config{}, errors.New("set BFF_ALLOWED_JELLYFIN_USER_IDS or explicitly enable BFF_ALLOW_ALL_JELLYFIN_USERS")
	}

	if c.Radarr, err = loadArr("RADARR"); err != nil {
		return Config{}, err
	}
	if c.Sonarr, err = loadArr("SONARR"); err != nil {
		return Config{}, err
	}
	if c.JellyfinURL, err = requiredURL("JELLYFIN_URL"); err != nil {
		return Config{}, err
	}
	if c.JellyfinAPIKey == "" {
		return Config{}, errors.New("JELLYFIN_API_KEY is required")
	}
	if raw := strings.TrimSpace(os.Getenv("QBITTORRENT_URL")); raw != "" {
		if c.QBittorrentURL, err = parseURL("QBITTORRENT_URL", raw); err != nil {
			return Config{}, err
		}
		if (c.QBittorrentUser == "") != (c.QBittorrentPass == "") {
			return Config{}, errors.New("qBittorrent username and password must either both be set or both be empty")
		}
	}
	return c, nil
}

func loadArr(prefix string) (Arr, error) {
	u, err := requiredURL(prefix + "_URL")
	if err != nil {
		return Arr{}, err
	}
	key, err := secret(prefix + "_API_KEY")
	if err != nil {
		return Arr{}, err
	}
	root := strings.TrimSpace(os.Getenv(prefix + "_ROOT_FOLDER"))
	profile, err := strconv.Atoi(os.Getenv(prefix + "_QUALITY_PROFILE_ID"))
	if key == "" || root == "" || err != nil || profile <= 0 {
		return Arr{}, fmt.Errorf("%s requires API key, root folder and positive quality profile ID", prefix)
	}
	return Arr{URL: u, APIKey: key, RootFolder: root, QualityProfileID: profile}, nil
}

func secret(name string) (string, error) {
	if path := strings.TrimSpace(os.Getenv(name + "_FILE")); path != "" {
		value, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read %s_FILE: %w", name, err)
		}
		return strings.TrimSpace(string(value)), nil
	}
	return strings.TrimSpace(os.Getenv(name)), nil
}

func requiredURL(name string) (*url.URL, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return nil, fmt.Errorf("%s is required", name)
	}
	return parseURL(name, raw)
}

func parseURL(name, raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("%s must be an absolute http(s) URL", name)
	}
	u.RawQuery, u.Fragment = "", ""
	u.Path = strings.TrimRight(u.Path, "/")
	return u, nil
}

func csvSet(raw string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, value := range strings.Split(raw, ",") {
		if value = strings.TrimSpace(value); value != "" {
			out[value] = struct{}{}
		}
	}
	return out
}

func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envBool(name string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func envPositiveInt(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type SubjectKind string

const (
	Movie   SubjectKind = "movie"
	Season  SubjectKind = "season"
	Episode SubjectKind = "episode"
)

type Subject struct {
	Kind          SubjectKind `json:"kind"`
	TMDBID        int         `json:"tmdbId"`
	TVDBID        int         `json:"tvdbId,omitempty"`
	SeasonNumber  *int        `json:"seasonNumber,omitempty"`
	EpisodeNumber *int        `json:"episodeNumber,omitempty"`
}

func (s Subject) Validate() error {
	if s.TMDBID <= 0 {
		return errors.New("tmdbId must be positive")
	}
	switch s.Kind {
	case Movie:
		if s.SeasonNumber != nil || s.EpisodeNumber != nil {
			return errors.New("movie must not contain season or episode numbers")
		}
	case Season:
		if s.SeasonNumber == nil || *s.SeasonNumber < 0 || s.EpisodeNumber != nil {
			return errors.New("season requires a non-negative seasonNumber")
		}
	case Episode:
		if s.SeasonNumber == nil || *s.SeasonNumber < 0 || s.EpisodeNumber == nil || *s.EpisodeNumber <= 0 {
			return errors.New("episode requires seasonNumber and a positive episodeNumber")
		}
	default:
		return fmt.Errorf("unsupported subject kind %q", s.Kind)
	}
	return nil
}

type User struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CanMovie  bool   `json:"canRequestMovies"`
	CanSeries bool   `json:"canRequestSeries"`
	CanCancel bool   `json:"canCancel"`
	IsAdmin   bool   `json:"-"`
}

type ResolvedSubject struct {
	Subject
	Backend          string
	ArrItemID        int
	ArrEpisodeID     int
	ExpectedEpisodes []int
}

type Release struct {
	Token                 string    `json:"token"`
	Title                 string    `json:"title"`
	SizeBytes             int64     `json:"sizeBytes"`
	Seeders               *int      `json:"seeders"`
	Quality               string    `json:"quality"`
	Indexer               string    `json:"indexer"`
	Protocol              string    `json:"protocol"`
	Approved              bool      `json:"approved"`
	Rejected              bool      `json:"rejected"`
	PolicyOverrideAllowed bool      `json:"policyOverrideAllowed"`
	RejectionReasons      []string  `json:"rejections"`
	FullSeason            bool      `json:"fullSeason,omitempty"`
	SeasonNumber          *int      `json:"seasonNumber,omitempty"`
	EpisodeNumbers        []int     `json:"episodeNumbers,omitempty"`
	ExpiresAt             time.Time `json:"expiresAt"`
}

type Candidate struct {
	Release
	Payload          []byte
	ExpectedEpisodes []int
}

type SearchState string

const (
	SearchPending   SearchState = "pending"
	SearchRunning   SearchState = "running"
	SearchCompleted SearchState = "completed"
	SearchFailed    SearchState = "failed"
	SearchExpired   SearchState = "expired"
)

type Search struct {
	ID               string      `json:"id"`
	UserID           string      `json:"-"`
	Subject          Subject     `json:"subject"`
	Backend          string      `json:"backend,omitempty"`
	ArrItemID        int         `json:"-"`
	ArrEpisodeID     int         `json:"-"`
	ExpectedEpisodes []int       `json:"-"`
	State            SearchState `json:"state"`
	Results          []Release   `json:"results,omitempty"`
	ErrorCode        string      `json:"errorCode,omitempty"`
	ErrorMessage     string      `json:"errorMessage,omitempty"`
	CreatedAt        time.Time   `json:"createdAt"`
	UpdatedAt        time.Time   `json:"updatedAt"`
	ExpiresAt        time.Time   `json:"expiresAt"`
}

type JobState string

const (
	Dispatching       JobState = "dispatching"
	DispatchUncertain JobState = "dispatch_uncertain"
	Queued            JobState = "queued"
	Downloading       JobState = "downloading"
	DownloadBlocked   JobState = "download_blocked"
	Verifying         JobState = "verifying"
	Importing         JobState = "importing"
	ImportFailed      JobState = "import_failed"
	Imported          JobState = "imported"
	WaitingJellyfin   JobState = "waiting_jellyfin"
	Available         JobState = "available"
	Cancelled         JobState = "cancelled"
	Failed            JobState = "error"
)

type Progress struct {
	Percent         *float64 `json:"percent,omitempty"`
	DownloadedBytes *int64   `json:"downloadedBytes,omitempty"`
	TotalBytes      *int64   `json:"totalBytes,omitempty"`
	BytesPerSecond  *int64   `json:"bytesPerSecond,omitempty"`
	ETASeconds      *int64   `json:"etaSeconds,omitempty"`
	Text            string   `json:"text,omitempty"`
	Source          string   `json:"source,omitempty"`
}

type Job struct {
	ID                 string    `json:"id"`
	UserID             string    `json:"-"`
	Subject            Subject   `json:"subject"`
	Backend            string    `json:"backend"`
	ArrItemID          int       `json:"-"`
	ArrEpisodeID       int       `json:"-"`
	ArrQueueID         int       `json:"-"`
	ExpectedEpisodes   []int     `json:"-"`
	ReleaseGUIDHash    []byte    `json:"-"`
	ReleaseIndexerID   int       `json:"-"`
	ReleaseIndexerHash []byte    `json:"-"`
	ReleaseURLHash     []byte    `json:"-"`
	ReleaseInfoHash    []byte    `json:"-"`
	Title              string    `json:"title"`
	State              JobState  `json:"state"`
	Version            int64     `json:"-"`
	PollAttempt        int       `json:"-"`
	NextPollAt         time.Time `json:"-"`
	Progress           Progress  `json:"progress"`
	DownloadID         string    `json:"-"`
	JellyfinItemID     string    `json:"jellyfinItemId,omitempty"`
	ErrorCode          string    `json:"errorCode,omitempty"`
	ErrorMessage       string    `json:"errorMessage,omitempty"`
	CreatedAt          time.Time `json:"createdAt"`
	UpdatedAt          time.Time `json:"updatedAt"`
}

func (j Job) Terminal() bool {
	return j.State == Available || j.State == Cancelled || j.State == Failed
}

type Observation struct {
	State      JobState
	Progress   Progress
	QueueID    int
	DownloadID string
	ErrorCode  string
	Error      string
}

type TorrentStats struct {
	Progress        float64
	DownloadedBytes int64
	TotalBytes      int64
	BytesPerSecond  int64
	ETASeconds      int64
	State           string
}

type Availability struct {
	Available bool
	ItemID    string
}

type UpstreamError struct {
	Service   string
	Status    int
	Code      string
	Retryable bool
	Ambiguous bool
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("%s upstream error (%s, status %d)", e.Service, e.Code, e.Status)
}

type Arr interface {
	Resolve(context.Context, Subject) (ResolvedSubject, error)
	Search(context.Context, ResolvedSubject) ([]Candidate, error)
	Grab(context.Context, json.RawMessage) error
	Observe(context.Context, Job) (Observation, error)
	Cancel(context.Context, Job) error
}

type Jellyfin interface {
	Authenticate(context.Context, string) (User, error)
	Availability(context.Context, string, Subject, []int) (Availability, error)
}

type QBittorrent interface {
	Enabled() bool
	Stats(context.Context, string) (TorrentStats, error)
}

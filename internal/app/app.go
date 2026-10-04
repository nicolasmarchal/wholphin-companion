package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nicolasmarchal/wholphin-companion/internal/domain"
	"github.com/nicolasmarchal/wholphin-companion/internal/secure"
	"github.com/nicolasmarchal/wholphin-companion/internal/store"
)

var (
	ErrForbidden               = errors.New("forbidden")
	ErrDispatchUncertain       = errors.New("uncertain dispatch cannot be cancelled safely")
	ErrCancellationUnavailable = errors.New("acquisition can no longer be cancelled")
)

type Options struct {
	SessionTTL       time.Duration
	SelectionTTL     time.Duration
	SearchTimeout    time.Duration
	UpstreamTimeout  time.Duration
	ReconcileEvery   time.Duration
	ProgressEvery    time.Duration
	ReconcileWorkers int
	AllowedUsers     map[string]struct{}
	AllowAllUsers    bool
	AllowCancel      bool
}

type App struct {
	store         *store.Store
	radarr        domain.Arr
	sonarr        domain.Arr
	jellyfin      domain.Jellyfin
	qbittorrent   domain.QBittorrent
	options       Options
	log           *slog.Logger
	now           func() time.Time
	searchWake    chan struct{}
	reconcileWake chan struct{}
	startOnce     sync.Once
	cancelMu      sync.Mutex
}

type Session struct {
	Token     string      `json:"token"`
	ExpiresAt time.Time   `json:"expiresAt"`
	User      domain.User `json:"user"`
}

func New(st *store.Store, radarr, sonarr domain.Arr, jellyfin domain.Jellyfin, qb domain.QBittorrent, options Options, logger *slog.Logger) *App {
	if options.ReconcileEvery <= 0 {
		options.ReconcileEvery = 10 * time.Second
	}
	if options.ProgressEvery <= 0 || options.ProgressEvery > options.ReconcileEvery {
		options.ProgressEvery = options.ReconcileEvery
	}
	if options.ReconcileWorkers <= 0 {
		options.ReconcileWorkers = 4
	} else if options.ReconcileWorkers > 32 {
		options.ReconcileWorkers = 32
	}
	options.AllowedUsers = canonicalUserSet(options.AllowedUsers)
	return &App{
		store: st, radarr: radarr, sonarr: sonarr, jellyfin: jellyfin, qbittorrent: qb,
		options: options, log: logger, now: func() time.Time { return time.Now().UTC() },
		searchWake: make(chan struct{}, 1), reconcileWake: make(chan struct{}, 1),
	}
}

func (a *App) Start(ctx context.Context) {
	a.startOnce.Do(func() {
		go a.searchLoop(ctx)
		go a.reconcileLoop(ctx)
		a.wakeSearch()
		a.WakeReconcile()
	})
}

func (a *App) ExchangeSession(ctx context.Context, jellyfinToken string) (Session, error) {
	if jellyfinToken == "" {
		return Session{}, store.ErrNotFound
	}
	upstreamCtx, cancel := context.WithTimeout(ctx, a.options.UpstreamTimeout)
	defer cancel()
	user, err := a.jellyfin.Authenticate(upstreamCtx, jellyfinToken)
	if err != nil {
		return Session{}, err
	}
	if !a.options.AllowAllUsers {
		if _, ok := a.options.AllowedUsers[canonicalUserID(user.ID)]; !ok {
			return Session{}, ErrForbidden
		}
	}
	// Jellyfin has no native permission for Arr acquisition. The BFF allow-list
	// is therefore the authority for these capabilities.
	user.CanMovie, user.CanSeries, user.CanCancel = true, true, a.options.AllowCancel
	now, expires := a.now(), a.now().Add(a.options.SessionTTL)
	if err := a.store.UpsertUser(ctx, user, now); err != nil {
		return Session{}, err
	}
	token, err := secure.Token(32)
	if err != nil {
		return Session{}, err
	}
	if err := a.store.CreateSession(ctx, secure.Hash(token), user.ID, now, expires); err != nil {
		return Session{}, err
	}
	return Session{Token: token, ExpiresAt: expires, User: user}, nil
}

func (a *App) Authenticate(ctx context.Context, token string) (domain.User, error) {
	if token == "" {
		return domain.User{}, store.ErrNotFound
	}
	hash := secure.Hash(token)
	user, err := a.store.Session(ctx, hash, a.now())
	if err != nil {
		return domain.User{}, err
	}
	if !a.options.AllowAllUsers {
		if _, allowed := a.options.AllowedUsers[canonicalUserID(user.ID)]; !allowed {
			_ = a.store.RevokeSession(ctx, hash)
			return domain.User{}, ErrForbidden
		}
	}
	return user, nil
}

func canonicalUserSet(users map[string]struct{}) map[string]struct{} {
	canonical := make(map[string]struct{}, len(users))
	for userID := range users {
		if userID = canonicalUserID(userID); userID != "" {
			canonical[userID] = struct{}{}
		}
	}
	return canonical
}

func canonicalUserID(userID string) string {
	// Jellyfin may expose the same UUID in compact or hyphenated form depending
	// on whether it came from the HTTP API, SDK, or database.
	userID = strings.ToLower(strings.TrimSpace(userID))
	return strings.ReplaceAll(userID, "-", "")
}

func (a *App) Revoke(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	return a.store.RevokeSession(ctx, secure.Hash(token))
}

func (a *App) StartSearch(ctx context.Context, user domain.User, subject domain.Subject) (domain.Search, error) {
	if err := subject.Validate(); err != nil {
		return domain.Search{}, err
	}
	if subject.Kind == domain.Movie && !user.CanMovie {
		return domain.Search{}, ErrForbidden
	}
	if subject.Kind != domain.Movie && !user.CanSeries {
		return domain.Search{}, ErrForbidden
	}
	id, err := secure.Token(18)
	if err != nil {
		return domain.Search{}, err
	}
	now := a.now()
	search := domain.Search{
		ID: id, UserID: user.ID, Subject: subject, State: domain.SearchPending,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(a.options.SearchTimeout + a.options.SelectionTTL),
	}
	if err := a.store.CreateSearch(ctx, search); err != nil {
		return domain.Search{}, err
	}
	a.wakeSearch()
	return search, nil
}

func (a *App) Search(ctx context.Context, user domain.User, id string) (domain.Search, error) {
	return a.store.Search(ctx, id, user.ID, a.now())
}

func (a *App) Acquire(ctx context.Context, user domain.User, selectionToken, idempotencyKey string) (domain.Job, bool, error) {
	if selectionToken == "" || idempotencyKey == "" {
		return domain.Job{}, false, errors.New("token and idempotency key are required")
	}
	requestHash := sha256.Sum256([]byte(selectionToken))
	reserved, err := a.store.ReserveJob(ctx, user.ID, selectionToken, idempotencyKey, requestHash[:], a.now())
	if err != nil {
		return domain.Job{}, false, err
	}
	if !reserved.Created {
		return reserved.Job, false, nil
	}
	arr := a.arr(reserved.Job.Backend)
	if arr == nil {
		return a.failDispatch(ctx, reserved.Job, "backend_unavailable", "Configured Arr backend is unavailable", false)
	}
	dispatchCtx, cancel := context.WithTimeout(context.Background(), a.options.UpstreamTimeout)
	defer cancel()
	err = arr.Grab(dispatchCtx, reserved.Payload)
	persistCtx, persistCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer persistCancel()
	if err != nil {
		var upstream *domain.UpstreamError
		if errors.As(err, &upstream) && upstream.Ambiguous {
			reserved.Job.State = domain.DispatchUncertain
			reserved.Job.Progress = domain.Progress{Text: "Dispatch outcome is being reconciled", Source: "arr"}
			if updateErr := a.store.UpdateJob(persistCtx, reserved.Job, "acquisition.dispatch_uncertain", a.now()); updateErr != nil {
				return domain.Job{}, true, updateErr
			}
			a.WakeReconcile()
			return reserved.Job, true, nil
		}
		code := "grab_failed"
		if errors.As(err, &upstream) && upstream.Code != "" {
			code = upstream.Code
		}
		return a.failDispatch(persistCtx, reserved.Job, code, "Arr rejected the selected release", true)
	}
	reserved.Job.State = domain.Queued
	reserved.Job.Progress = domain.Progress{Text: "Queued", Source: "arr"}
	if err := a.store.UpdateJob(persistCtx, reserved.Job, "acquisition.queued", a.now()); err != nil {
		return domain.Job{}, true, err
	}
	a.WakeReconcile()
	return reserved.Job, true, nil
}

func (a *App) failDispatch(ctx context.Context, job domain.Job, code, message string, created bool) (domain.Job, bool, error) {
	job.State, job.ErrorCode, job.ErrorMessage = domain.Failed, code, message
	if err := a.store.UpdateJob(ctx, job, "acquisition.error", a.now()); err != nil {
		return domain.Job{}, created, err
	}
	return job, created, nil
}

func (a *App) Job(ctx context.Context, user domain.User, id string) (domain.Job, error) {
	return a.store.Job(ctx, id, user.ID)
}

func (a *App) JobChangeSignal() <-chan struct{} { return a.store.JobChangeSignal() }

func (a *App) Jobs(ctx context.Context, user domain.User, subject *domain.Subject, mediaType string, active bool) ([]domain.Job, error) {
	return a.store.Jobs(ctx, user.ID, subject, mediaType, active)
}

func (a *App) Cancel(ctx context.Context, user domain.User, id string) (domain.Job, error) {
	a.cancelMu.Lock()
	defer a.cancelMu.Unlock()
	if !a.options.AllowCancel || !user.CanCancel {
		return domain.Job{}, ErrForbidden
	}
	job, err := a.store.Job(ctx, id, user.ID)
	if err != nil {
		return domain.Job{}, err
	}
	if job.State == domain.Cancelled {
		return job, nil
	}
	if job.Terminal() {
		return domain.Job{}, errors.New("terminal acquisition cannot be cancelled")
	}
	if job.State == domain.DispatchUncertain && job.ArrQueueID == 0 && job.DownloadID == "" {
		// The POST may have reached Arr. Releasing the subject lock here would
		// allow a second exact release to be grabbed before the first appears.
		return domain.Job{}, ErrDispatchUncertain
	}
	if job.State == domain.Imported || job.State == domain.WaitingJellyfin || job.State == domain.ImportFailed {
		return domain.Job{}, ErrCancellationUnavailable
	}
	arr := a.arr(job.Backend)
	if arr == nil {
		return domain.Job{}, errors.New("backend unavailable")
	}
	upstreamCtx, cancel := context.WithTimeout(ctx, a.options.UpstreamTimeout)
	defer cancel()
	if err := arr.Cancel(upstreamCtx, job); err != nil {
		return domain.Job{}, err
	}
	job.State, job.Progress.Text = domain.Cancelled, "Cancelled"
	for attempt := 0; attempt < 3; attempt++ {
		err = a.store.UpdateJob(ctx, job, "acquisition.cancelled", a.now())
		if err == nil {
			return job, nil
		}
		if !errors.Is(err, store.ErrConcurrentUpdate) {
			return domain.Job{}, err
		}
		job, err = a.store.Job(ctx, id, user.ID)
		if err != nil {
			return domain.Job{}, err
		}
		if job.State == domain.Cancelled {
			return job, nil
		}
		if job.Terminal() {
			return domain.Job{}, store.ErrTerminalState
		}
		job.State, job.Progress.Text = domain.Cancelled, "Cancelled"
	}
	return domain.Job{}, store.ErrConcurrentUpdate
}

func (a *App) HasQBittorrentMetrics() bool     { return a.qbittorrent != nil && a.qbittorrent.Enabled() }
func (a *App) SupportsCancellation() bool      { return a.options.AllowCancel }
func (a *App) Ready(ctx context.Context) error { return a.store.Ping(ctx) }

func (a *App) WakeReconcile() {
	select {
	case a.reconcileWake <- struct{}{}:
	default:
	}
}

func (a *App) wakeSearch() {
	select {
	case a.searchWake <- struct{}{}:
	default:
	}
}

func (a *App) searchLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-a.searchWake:
		}
		for a.runOneSearch(ctx) {
		}
	}
}

func (a *App) runOneSearch(parent context.Context) bool {
	search, err := a.store.ClaimSearch(parent, a.now())
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, store.ErrNotFound) {
		return false
	}
	if err != nil {
		a.log.Error("claim release search", "error", err)
		return false
	}
	ctx, cancel := context.WithTimeout(parent, a.options.SearchTimeout)
	defer cancel()
	backend := "sonarr"
	arr := a.sonarr
	if search.Subject.Kind == domain.Movie {
		backend, arr = "radarr", a.radarr
	}
	if arr == nil {
		_ = a.store.FailSearch(parent, search.ID, "backend_unavailable", backend+" is not configured", a.now())
		return true
	}
	resolved, err := arr.Resolve(ctx, search.Subject)
	if err == nil {
		var candidates []domain.Candidate
		candidates, err = arr.Search(ctx, resolved)
		if err == nil {
			expires := a.now().Add(a.options.SelectionTTL)
			for index := range candidates {
				candidates[index].ExpiresAt = expires
			}
			if err = a.store.CompleteSearch(parent, search.ID, resolved, candidates, a.now(), expires); err == nil {
				return true
			}
		}
	}
	code, message := "search_failed", "Interactive search failed"
	var upstream *domain.UpstreamError
	if errors.As(err, &upstream) {
		if upstream.Code != "" {
			code = upstream.Code
		}
		if code == "series_mapping_required" {
			message = "A TVDb ID is required by this Sonarr version"
		}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		code, message = "search_timeout", "Interactive search timed out"
	}
	_ = a.store.FailSearch(parent, search.ID, code, message, a.now())
	return true
}

func (a *App) reconcileLoop(ctx context.Context) {
	// The fast ticker only makes due progress jobs responsive. Other phases keep
	// their independently persisted reconciliation/backoff schedule.
	ticker := time.NewTicker(a.options.ProgressEvery)
	defer ticker.Stop()
	cleanup := time.NewTicker(time.Hour)
	defer cleanup.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.reconcile(ctx, false)
		case <-a.reconcileWake:
			a.reconcile(ctx, true)
		case <-cleanup.C:
			_ = a.store.Cleanup(ctx, a.now())
		}
	}
}

func (a *App) reconcile(ctx context.Context, force bool) {
	jobs, err := a.store.ActiveJobs(ctx, a.now(), force)
	if err != nil {
		a.log.Error("list active acquisitions", "error", err)
		return
	}
	workerCount := a.options.ReconcileWorkers
	if workerCount > len(jobs) {
		workerCount = len(jobs)
	}
	if workerCount == 0 {
		return
	}
	queue := make(chan domain.Job)
	var workers sync.WaitGroup
	workers.Add(workerCount)
	for range workerCount {
		go func() {
			defer workers.Done()
			for job := range queue {
				a.reconcileJob(ctx, job)
			}
		}()
	}
	for _, job := range jobs {
		select {
		case queue <- job:
		case <-ctx.Done():
			close(queue)
			workers.Wait()
			return
		}
	}
	close(queue)
	workers.Wait()
}

func (a *App) reconcileJob(parent context.Context, job domain.Job) {
	previous := job
	ctx, cancel := context.WithTimeout(parent, a.options.UpstreamTimeout)
	defer cancel()
	arr := a.arr(job.Backend)
	if arr == nil {
		job.ErrorCode, job.ErrorMessage = "backend_unavailable", "Arr status could not be refreshed"
		a.scheduleNextPoll(&job, previous)
		_ = a.store.UpdateJob(parent, job, "acquisition.backend_unavailable", a.now())
		return
	}

	if job.State == domain.Imported || job.State == domain.WaitingJellyfin {
		availability, err := a.jellyfin.Availability(ctx, job.UserID, job.Subject, job.ExpectedEpisodes)
		if err != nil {
			job.State, job.Progress.Text = domain.WaitingJellyfin, "Waiting for Jellyfin"
			job.ErrorCode, job.ErrorMessage = "jellyfin_unavailable", "Jellyfin availability could not be verified"
		} else if availability.Available {
			job.State, job.JellyfinItemID = domain.Available, availability.ItemID
			job.Progress.Text = "Available in Jellyfin"
			job.ErrorCode, job.ErrorMessage = "", ""
		} else {
			job.State, job.Progress.Text = domain.WaitingJellyfin, "Waiting for Jellyfin"
			job.ErrorCode, job.ErrorMessage = "", ""
		}
		a.scheduleNextPoll(&job, previous)
		if err := a.store.UpdateJob(parent, job, "acquisition."+string(job.State), a.now()); err != nil && !errors.Is(err, store.ErrTerminalState) && !errors.Is(err, store.ErrConcurrentUpdate) {
			a.log.Error("update Jellyfin acquisition state", "job_id", job.ID, "error", err)
		}
		return
	}

	observation, err := arr.Observe(ctx, job)
	if err != nil {
		// A transient observation failure does not erase the last canonical state.
		if job.ErrorCode == "" || job.ErrorCode == "arr_unavailable" {
			job.ErrorCode, job.ErrorMessage = "arr_unavailable", "Arr status could not be refreshed"
		}
		a.scheduleNextPoll(&job, previous)
		_ = a.store.UpdateJob(parent, job, "acquisition.arr_unavailable", a.now())
		return
	}
	job.State, job.Progress = observation.State, observation.Progress
	if observation.QueueID > 0 {
		job.ArrQueueID = observation.QueueID
	}
	if observation.DownloadID != "" {
		job.DownloadID = observation.DownloadID
	}
	job.ErrorCode, job.ErrorMessage = observation.ErrorCode, observation.Error

	if a.HasQBittorrentMetrics() && job.DownloadID != "" && (job.State == domain.Queued || job.State == domain.Downloading || job.State == domain.DownloadBlocked || job.State == domain.Verifying) {
		if stats, statErr := a.qbittorrent.Stats(ctx, job.DownloadID); statErr == nil {
			percent := stats.Progress * 100
			if percent < 0 {
				percent = 0
			} else if percent > 100 {
				percent = 100
			}
			if stats.TotalBytes < 0 {
				stats.TotalBytes = 0
			}
			if stats.DownloadedBytes < 0 {
				stats.DownloadedBytes = 0
			} else if stats.TotalBytes > 0 && stats.DownloadedBytes > stats.TotalBytes {
				stats.DownloadedBytes = stats.TotalBytes
			}
			if stats.BytesPerSecond < 0 {
				stats.BytesPerSecond = 0
			}
			job.Progress.Percent = &percent
			job.Progress.DownloadedBytes = &stats.DownloadedBytes
			job.Progress.TotalBytes = &stats.TotalBytes
			job.Progress.BytesPerSecond = &stats.BytesPerSecond
			if stats.ETASeconds >= 0 && stats.ETASeconds < 8640000 {
				job.Progress.ETASeconds = &stats.ETASeconds
			}
			job.Progress.Source, job.Progress.Text = "qbittorrent", publicTorrentState(stats.State)
		}
	}
	if job.State == domain.Imported {
		job.Progress.Text = "Import completed"
	}
	a.scheduleNextPoll(&job, previous)
	if err := a.store.UpdateJob(parent, job, "acquisition."+string(job.State), a.now()); err != nil && !errors.Is(err, store.ErrTerminalState) && !errors.Is(err, store.ErrConcurrentUpdate) {
		a.log.Error("update acquisition state", "job_id", job.ID, "error", err)
	}
}

func (a *App) scheduleNextPoll(job *domain.Job, previous domain.Job) {
	materialBefore, materialAfter := previous, *job
	materialBefore.CreatedAt, materialBefore.UpdatedAt = time.Time{}, time.Time{}
	materialAfter.CreatedAt, materialAfter.UpdatedAt = time.Time{}, time.Time{}
	materialBefore.PollAttempt, materialAfter.PollAttempt = 0, 0
	materialBefore.NextPollAt, materialAfter.NextPollAt = time.Time{}, time.Time{}
	materialBefore.Version, materialAfter.Version = 0, 0
	if continuousProgressJob(*job) {
		// Even an identical sample can mean the transfer is between torrent
		// updates. Leave the job due so the next reconciliation tick samples it;
		// scheduling it one full interval from the end of this pass would skip the
		// next tick and effectively double the configured progress cadence.
		job.PollAttempt = 0
		job.NextPollAt = a.now()
		return
	} else if reflect.DeepEqual(materialBefore, materialAfter) {
		job.PollAttempt = previous.PollAttempt + 1
	} else {
		job.PollAttempt = 0
	}
	exponent := job.PollAttempt
	if exponent > 8 {
		exponent = 8
	}
	delay := a.options.ReconcileEvery * time.Duration(1<<exponent)
	maximum := 5 * time.Minute
	if a.options.ReconcileEvery > maximum {
		maximum = a.options.ReconcileEvery
	}
	if delay > maximum {
		delay = maximum
	}
	// Stable per-job jitter avoids synchronized polling after a host restart.
	digest := sha256.Sum256([]byte(job.ID + ":" + strconv.Itoa(job.PollAttempt)))
	jitterWindow := delay / 5
	if jitterWindow > 0 {
		delay = delay - jitterWindow/2 + time.Duration(digest[0])*jitterWindow/255
	}
	job.NextPollAt = a.now().Add(delay)
}

func continuousProgressJob(job domain.Job) bool {
	if job.ErrorCode != "" {
		return false
	}
	return job.State == domain.Queued || job.State == domain.Downloading || job.State == domain.Verifying
}

func publicTorrentState(state string) string {
	switch state {
	case "downloading", "stalledDL", "metaDL", "forcedDL", "allocating":
		return "Downloading"
	case "checkingDL", "checkingUP", "checkingResumeData", "moving":
		return "Verifying download"
	case "pausedDL", "stoppedDL", "error", "missingFiles", "unknown":
		return "Download blocked"
	case "queuedDL":
		return "Queued"
	case "uploading", "stalledUP", "pausedUP", "stoppedUP", "queuedUP", "forcedUP":
		return "Download completed; waiting for import"
	default:
		return "Downloading"
	}
}

func (a *App) arr(name string) domain.Arr {
	switch name {
	case "radarr":
		return a.radarr
	case "sonarr":
		return a.sonarr
	default:
		return nil
	}
}

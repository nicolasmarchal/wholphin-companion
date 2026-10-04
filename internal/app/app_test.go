package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/nicolasmarchal/wholphin-companion/internal/domain"
	"github.com/nicolasmarchal/wholphin-companion/internal/secure"
	"github.com/nicolasmarchal/wholphin-companion/internal/store"
)

type arrFake struct {
	mu             sync.Mutex
	resolved       domain.ResolvedSubject
	candidates     []domain.Candidate
	grabErr        error
	grabbed        [][]byte
	observe        domain.Observation
	observeErr     error
	observeStart   chan struct{}
	observeWait    chan struct{}
	observeActive  int
	observeMaximum int
	grabStart      chan struct{}
	grabWait       chan struct{}
	cancelHits     int
	cancelWait     bool
}

func (f *arrFake) Resolve(_ context.Context, subject domain.Subject) (domain.ResolvedSubject, error) {
	value := f.resolved
	value.Subject = subject
	return value, nil
}
func (f *arrFake) Search(context.Context, domain.ResolvedSubject) ([]domain.Candidate, error) {
	return append([]domain.Candidate(nil), f.candidates...), nil
}
func (f *arrFake) Grab(_ context.Context, payload json.RawMessage) error {
	if f.grabStart != nil {
		select {
		case f.grabStart <- struct{}{}:
		default:
		}
	}
	if f.grabWait != nil {
		<-f.grabWait
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.grabbed = append(f.grabbed, append([]byte(nil), payload...))
	return f.grabErr
}

func TestAcquirePersistsAfterHTTPContextIsCancelledDuringGrab(t *testing.T) {
	application, st, arr, _, user, _ := appFixture(t)
	arr.grabStart = make(chan struct{}, 1)
	arr.grabWait = make(chan struct{})
	release := runSearch(t, application, user)
	requestCtx, cancel := context.WithCancel(context.Background())
	type result struct {
		job domain.Job
		err error
	}
	done := make(chan result, 1)
	go func() {
		job, _, err := application.Acquire(requestCtx, user, release.Token, "cancelled-http-key")
		done <- result{job: job, err: err}
	}()
	<-arr.grabStart
	cancel()
	close(arr.grabWait)
	got := <-done
	if got.err != nil || got.job.State != domain.Queued {
		t.Fatalf("acquire result=%#v err=%v", got.job, got.err)
	}
	persisted, err := st.Job(context.Background(), got.job.ID, user.ID)
	if err != nil || persisted.State != domain.Queued {
		t.Fatalf("post-dispatch state was not durable: %#v err=%v", persisted, err)
	}
	if len(arr.grabs()) != 1 {
		t.Fatalf("grab count=%d", len(arr.grabs()))
	}
}
func (f *arrFake) Observe(ctx context.Context, _ domain.Job) (domain.Observation, error) {
	f.mu.Lock()
	f.observeActive++
	if f.observeActive > f.observeMaximum {
		f.observeMaximum = f.observeActive
	}
	observation, observeErr := f.observe, f.observeErr
	started, wait := f.observeStart, f.observeWait
	f.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	if wait != nil {
		select {
		case <-wait:
		case <-ctx.Done():
			observeErr = ctx.Err()
		}
	}
	f.mu.Lock()
	f.observeActive--
	f.mu.Unlock()
	return observation, observeErr
}
func (f *arrFake) Cancel(ctx context.Context, _ domain.Job) error {
	f.mu.Lock()
	f.cancelHits++
	wait := f.cancelWait
	f.mu.Unlock()
	if wait {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}
func (f *arrFake) grabs() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.grabbed...)
}

func (f *arrFake) maximumConcurrentObservations() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.observeMaximum
}

type jellyfinFake struct {
	availability domain.Availability
	user         domain.User
	userSeen     string
	err          error
	authWait     bool
}

func (f *jellyfinFake) Authenticate(ctx context.Context, _ string) (domain.User, error) {
	if f.authWait {
		<-ctx.Done()
		return domain.User{}, ctx.Err()
	}
	return f.user, nil
}
func (f *jellyfinFake) Availability(_ context.Context, userID string, _ domain.Subject, _ []int) (domain.Availability, error) {
	f.userSeen = userID
	return f.availability, f.err
}

type qbFake struct{}

func (qbFake) Enabled() bool { return false }
func (qbFake) Stats(context.Context, string) (domain.TorrentStats, error) {
	return domain.TorrentStats{}, nil
}

type qbMetricsFake struct{ stats domain.TorrentStats }

func (qbMetricsFake) Enabled() bool { return true }
func (f qbMetricsFake) Stats(context.Context, string) (domain.TorrentStats, error) {
	return f.stats, nil
}

func TestSearchPersistsResolvedResultsAndSelectionTTL(t *testing.T) {
	application, st, arr, _, user, now := appFixture(t)
	search, err := application.StartSearch(context.Background(), user, domain.Subject{Kind: domain.Movie, TMDBID: 42})
	if err != nil {
		t.Fatal(err)
	}
	if !application.runOneSearch(context.Background()) {
		t.Fatal("pending search was not run")
	}
	result, err := application.Search(context.Background(), user, search.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != domain.SearchCompleted || len(result.Results) != 1 {
		t.Fatalf("search=%#v", result)
	}
	if result.Results[0].Token == "" {
		t.Fatal("missing opaque token")
	}
	wantExpiry := now.Add(10 * time.Minute)
	if !result.ExpiresAt.Equal(wantExpiry) || !result.Results[0].ExpiresAt.Equal(wantExpiry) {
		t.Fatalf("TTL got search=%s release=%s want=%s", result.ExpiresAt, result.Results[0].ExpiresAt, wantExpiry)
	}
	if arr.resolved.ArrItemID != 7 {
		t.Fatal("fixture invalid")
	}
	if _, err := st.Search(context.Background(), search.ID, user.ID, wantExpiry.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestSessionExchangeUsesBoundedUpstreamContext(t *testing.T) {
	application, _, _, jellyfin, _, _ := appFixture(t)
	application.options.UpstreamTimeout = 5 * time.Millisecond
	jellyfin.authWait = true
	started := time.Now()
	_, err := application.ExchangeSession(context.Background(), "token")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("exchange error=%v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("session exchange was not bounded")
	}
}

func TestSessionExchangeMatchesCanonicalJellyfinID(t *testing.T) {
	_, st, arr, jellyfin, user, _ := appFixture(t)
	user.ID = "e9d6114ac51e40ab960f588839fffac4"
	jellyfin.user = user
	application := New(st, arr, arr, jellyfin, qbFake{}, Options{
		SessionTTL: time.Hour, SelectionTTL: 10 * time.Minute, SearchTimeout: time.Minute,
		UpstreamTimeout: time.Second, ReconcileEvery: time.Minute,
		AllowedUsers: map[string]struct{}{"E9D6114A-C51E-40AB-960F-588839FFFAC4": {}},
	}, slog.New(slog.NewTextHandler(discardWriter{}, nil)))

	session, err := application.ExchangeSession(context.Background(), "jellyfin-token")
	if err != nil {
		t.Fatal(err)
	}
	if session.User.ID != user.ID {
		t.Fatalf("session user=%q want=%q", session.User.ID, user.ID)
	}
}

func TestAuthenticateReappliesCurrentAllowListAndRevokesSession(t *testing.T) {
	application, st, _, _, user, now := appFixture(t)
	token := "existing-bff-session"
	if err := st.CreateSession(context.Background(), secure.Hash(token), user.ID, now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	application.options.AllowAllUsers = false
	application.options.AllowedUsers = map[string]struct{}{"different-user": {}}
	if _, err := application.Authenticate(context.Background(), token); !errors.Is(err, ErrForbidden) {
		t.Fatalf("removed user retained access: %v", err)
	}
	if _, err := application.Authenticate(context.Background(), token); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("disallowed session was not revoked: %v", err)
	}
}

func TestAcquirePostsExactPayloadOnceAndQueues(t *testing.T) {
	application, _, arr, _, user, _ := appFixture(t)
	release := runSearch(t, application, user)
	job, created, err := application.Acquire(context.Background(), user, release.Token, "exact-key-00000001")
	if err != nil {
		t.Fatal(err)
	}
	if !created || job.State != domain.Queued {
		t.Fatalf("created=%v job=%#v", created, job)
	}
	grabs := arr.grabs()
	if len(grabs) != 1 || !bytes.Equal(grabs[0], []byte(`{"guid":"chosen","indexerId":4,"downloadUrl":"server-only"}`)) {
		t.Fatalf("grab did not preserve exact payload: %q", grabs)
	}
	replayed, created, err := application.Acquire(context.Background(), user, release.Token, "exact-key-00000001")
	if err != nil {
		t.Fatal(err)
	}
	if created || replayed.ID != job.ID || len(arr.grabs()) != 1 {
		t.Fatal("idempotent replay dispatched again")
	}
}

func TestAmbiguousDispatchIsNeverBlindlyRetried(t *testing.T) {
	application, _, arr, _, user, _ := appFixture(t)
	arr.grabErr = &domain.UpstreamError{Service: "radarr", Code: "unreachable", Retryable: true, Ambiguous: true}
	release := runSearch(t, application, user)
	job, created, err := application.Acquire(context.Background(), user, release.Token, "ambiguous-key-0001")
	if err != nil {
		t.Fatal(err)
	}
	if !created || job.State != domain.DispatchUncertain {
		t.Fatalf("job=%#v", job)
	}
	if _, _, err = application.Acquire(context.Background(), user, release.Token, "ambiguous-key-0001"); err != nil {
		t.Fatal(err)
	}
	if len(arr.grabs()) != 1 {
		t.Fatalf("ambiguous dispatch was re-grabbed %d times", len(arr.grabs()))
	}
}

func TestUncertainDispatchCannotBeCancelledOrReacquired(t *testing.T) {
	application, _, arr, _, user, _ := appFixture(t)
	application.options.AllowCancel = true
	user.CanCancel = true
	arr.grabErr = &domain.UpstreamError{Service: "radarr", Code: "upstream_timeout", Retryable: true, Ambiguous: true}
	first := runSearch(t, application, user)
	job, _, err := application.Acquire(context.Background(), user, first.Token, "uncertain-key-0001")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = application.Cancel(context.Background(), user, job.ID); !errors.Is(err, ErrDispatchUncertain) {
		t.Fatalf("unsafe cancellation was allowed: %v", err)
	}
	second := runSearch(t, application, user)
	if _, _, err = application.Acquire(context.Background(), user, second.Token, "uncertain-key-0002"); !errors.Is(err, store.ErrSubjectActive) {
		t.Fatalf("second overlapping acquisition was allowed: %v", err)
	}
	if len(arr.grabs()) != 1 {
		t.Fatalf("uncertain dispatch caused %d grabs", len(arr.grabs()))
	}
}

func TestConcurrentCancellationCallsArrOnce(t *testing.T) {
	application, _, arr, _, user, _ := appFixture(t)
	application.options.AllowCancel = true
	user.CanCancel = true
	release := runSearch(t, application, user)
	job, _, err := application.Acquire(context.Background(), user, release.Token, "cancel-once-key-01")
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			_, cancelErr := application.Cancel(context.Background(), user, job.ID)
			errs <- cancelErr
		}()
	}
	close(start)
	for i := 0; i < 2; i++ {
		if err = <-errs; err != nil {
			t.Fatal(err)
		}
	}
	arr.mu.Lock()
	hits := arr.cancelHits
	arr.mu.Unlock()
	if hits != 1 {
		t.Fatalf("Arr cancel called %d times", hits)
	}
}

func TestCancellationUsesBoundedUpstreamContext(t *testing.T) {
	application, _, arr, _, user, _ := appFixture(t)
	application.options.AllowCancel = true
	application.options.UpstreamTimeout = 5 * time.Millisecond
	user.CanCancel = true
	arr.cancelWait = true
	release := runSearch(t, application, user)
	job, _, err := application.Acquire(context.Background(), user, release.Token, "cancel-timeout-key")
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = application.Cancel(context.Background(), user, job.ID)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancel error=%v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("cancel was not bounded")
	}
}

func TestReconcileImportThenWaitThenAvailable(t *testing.T) {
	application, st, arr, jellyfin, user, _ := appFixture(t)
	release := runSearch(t, application, user)
	job, _, err := application.Acquire(context.Background(), user, release.Token, "reconcile-key-0001")
	if err != nil {
		t.Fatal(err)
	}
	job, _ = st.Job(context.Background(), job.ID, user.ID)
	arr.observe = domain.Observation{State: domain.Imported, DownloadID: "hash", Progress: domain.Progress{Text: "Import completed", Source: "arr"}}
	application.reconcileJob(context.Background(), job)
	job, _ = st.Job(context.Background(), job.ID, user.ID)
	if job.State != domain.Imported {
		t.Fatalf("state=%s", job.State)
	}

	jellyfin.availability = domain.Availability{}
	application.reconcileJob(context.Background(), job)
	job, _ = st.Job(context.Background(), job.ID, user.ID)
	if job.State != domain.WaitingJellyfin {
		t.Fatalf("state=%s", job.State)
	}

	jellyfin.availability = domain.Availability{Available: true, ItemID: "jellyfin-movie"}
	application.reconcileJob(context.Background(), job)
	job, _ = st.Job(context.Background(), job.ID, user.ID)
	if job.State != domain.Available || job.JellyfinItemID != "jellyfin-movie" {
		t.Fatalf("job=%#v", job)
	}
	if jellyfin.userSeen != user.ID {
		t.Fatalf("Jellyfin was checked for %q, want %q", jellyfin.userSeen, user.ID)
	}
}

func TestJellyfinFailureRemainsDistinctWhileWaiting(t *testing.T) {
	application, st, arr, jellyfin, user, _ := appFixture(t)
	release := runSearch(t, application, user)
	job, _, err := application.Acquire(context.Background(), user, release.Token, "jellyfin-error-key")
	if err != nil {
		t.Fatal(err)
	}
	job, _ = st.Job(context.Background(), job.ID, user.ID)
	arr.observe = domain.Observation{State: domain.Imported, Progress: domain.Progress{Text: "Import completed", Source: "arr"}}
	application.reconcileJob(context.Background(), job)
	job, _ = st.Job(context.Background(), job.ID, user.ID)
	jellyfin.err = &domain.UpstreamError{Service: "jellyfin", Code: "unreachable", Retryable: true}
	application.reconcileJob(context.Background(), job)
	job, _ = st.Job(context.Background(), job.ID, user.ID)
	if job.State != domain.WaitingJellyfin || job.ErrorCode != "jellyfin_unavailable" {
		t.Fatalf("Jellyfin failure was not preserved distinctly: %#v", job)
	}
}

func TestImportFailureRemainsDistinct(t *testing.T) {
	application, st, arr, _, user, _ := appFixture(t)
	release := runSearch(t, application, user)
	job, _, err := application.Acquire(context.Background(), user, release.Token, "import-error-key1")
	if err != nil {
		t.Fatal(err)
	}
	job, _ = st.Job(context.Background(), job.ID, user.ID)
	arr.observe = domain.Observation{State: domain.ImportFailed, ErrorCode: "arr_queue_error", Error: "permission denied", Progress: domain.Progress{Text: "Import failed", Source: "arr"}}
	application.reconcileJob(context.Background(), job)
	job, _ = st.Job(context.Background(), job.ID, user.ID)
	if job.State != domain.ImportFailed || job.ErrorCode != "arr_queue_error" {
		t.Fatalf("job=%#v", job)
	}
}

func TestArrObservationFailureIsVisibleWithoutErasingState(t *testing.T) {
	application, st, arr, _, user, _ := appFixture(t)
	release := runSearch(t, application, user)
	job, _, err := application.Acquire(context.Background(), user, release.Token, "arr-error-key-0001")
	if err != nil {
		t.Fatal(err)
	}
	job, _ = st.Job(context.Background(), job.ID, user.ID)
	arr.observeErr = &domain.UpstreamError{Service: "radarr", Code: "unreachable", Retryable: true}
	application.reconcileJob(context.Background(), job)
	job, _ = st.Job(context.Background(), job.ID, user.ID)
	if job.State != domain.Queued || job.ErrorCode != "arr_unavailable" {
		t.Fatalf("stale Arr status was hidden: %#v", job)
	}
	arr.observeErr = nil
	application.reconcileJob(context.Background(), job)
	job, _ = st.Job(context.Background(), job.ID, user.ID)
	if job.ErrorCode != "" {
		t.Fatalf("Arr recovery did not clear stale error: %#v", job)
	}
}

func TestQBittorrentMetricsAreClamped(t *testing.T) {
	application, st, arr, _, user, _ := appFixture(t)
	application.qbittorrent = qbMetricsFake{stats: domain.TorrentStats{Progress: 1.5, DownloadedBytes: 200, TotalBytes: 100, BytesPerSecond: -1, ETASeconds: 10, State: "downloading"}}
	release := runSearch(t, application, user)
	job, _, err := application.Acquire(context.Background(), user, release.Token, "qbit-clamp-key-01")
	if err != nil {
		t.Fatal(err)
	}
	job, _ = st.Job(context.Background(), job.ID, user.ID)
	arr.observe = domain.Observation{State: domain.Downloading, DownloadID: "hash", Progress: domain.Progress{Text: "Downloading", Source: "arr"}}
	application.reconcileJob(context.Background(), job)
	job, _ = st.Job(context.Background(), job.ID, user.ID)
	if job.Progress.Percent == nil || *job.Progress.Percent != 100 || job.Progress.DownloadedBytes == nil || *job.Progress.DownloadedBytes != 100 || job.Progress.BytesPerSecond == nil || *job.Progress.BytesPerSecond != 0 {
		t.Fatalf("qBittorrent metrics not clamped: %#v", job.Progress)
	}
}

func TestReconciliationBacksOffWithoutAppendingNoopEvents(t *testing.T) {
	application, st, arr, _, user, now := appFixture(t)
	release := runSearch(t, application, user)
	job, _, err := application.Acquire(context.Background(), user, release.Token, "backoff-key-00001")
	if err != nil {
		t.Fatal(err)
	}
	arr.observe = domain.Observation{State: domain.Importing, Progress: domain.Progress{Text: "Importing", Source: "arr"}}
	job, _ = st.Job(context.Background(), job.ID, user.ID)
	application.reconcileJob(context.Background(), job)
	baseline, _ := st.Job(context.Background(), job.ID, user.ID)
	application.reconcileJob(context.Background(), baseline)
	first, _ := st.Job(context.Background(), job.ID, user.ID)
	if first.PollAttempt != 1 || !first.NextPollAt.After(now) {
		t.Fatalf("first backoff=%#v", first)
	}
	application.reconcileJob(context.Background(), first)
	second, _ := st.Job(context.Background(), job.ID, user.ID)
	if second.PollAttempt != 2 || !second.NextPollAt.After(first.NextPollAt) {
		t.Fatalf("second backoff=%#v first=%s", second, first.NextPollAt)
	}
	events, err := st.Events(context.Background(), user.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("unchanged polls appended events: %d", len(events))
	}
	arr.observe = domain.Observation{State: domain.Downloading, Progress: domain.Progress{Text: "Downloading", Source: "arr"}}
	application.reconcileJob(context.Background(), second)
	changed, _ := st.Job(context.Background(), job.ID, user.ID)
	if changed.PollAttempt != 0 || changed.State != domain.Downloading {
		t.Fatalf("state change did not reset backoff: %#v", changed)
	}
}

func TestActiveDownloadReconciliationKeepsBaseCadence(t *testing.T) {
	application, st, arr, _, user, now := appFixture(t)
	release := runSearch(t, application, user)
	job, _, err := application.Acquire(context.Background(), user, release.Token, "progress-key-00001")
	if err != nil {
		t.Fatal(err)
	}
	arr.observe = domain.Observation{State: domain.Downloading, Progress: domain.Progress{Text: "Downloading", Source: "arr"}}
	job, _ = st.Job(context.Background(), job.ID, user.ID)
	application.reconcileJob(context.Background(), job)
	first, _ := st.Job(context.Background(), job.ID, user.ID)
	application.reconcileJob(context.Background(), first)
	second, _ := st.Job(context.Background(), job.ID, user.ID)
	if second.PollAttempt != 0 {
		t.Fatalf("active download backed off: %#v", second)
	}
	if second.NextPollAt.After(now) {
		t.Fatalf("active download was not left due for the next tick: %#v", second)
	}
	if application.options.ProgressEvery != 2*time.Second {
		t.Fatalf("progress cadence=%s", application.options.ProgressEvery)
	}
}

func TestFastProgressCadenceExcludesSlowAndFailingPhases(t *testing.T) {
	application, _, _, _, _, now := appFixture(t)
	for _, state := range []domain.JobState{domain.Queued, domain.Downloading, domain.Verifying} {
		previous := domain.Job{ID: "healthy", State: state, PollAttempt: 4, NextPollAt: now.Add(time.Hour)}
		job := previous
		application.scheduleNextPoll(&job, previous)
		if job.PollAttempt != 0 || job.NextPollAt.After(now) {
			t.Fatalf("healthy %s job did not stay on fast cadence: %#v", state, job)
		}
	}
	for _, job := range []domain.Job{
		{ID: "error", State: domain.Downloading, ErrorCode: "arr_unavailable", PollAttempt: 2},
		{ID: "import", State: domain.Importing, PollAttempt: 2},
		{ID: "jellyfin", State: domain.WaitingJellyfin, PollAttempt: 2},
	} {
		previous := job
		application.scheduleNextPoll(&job, previous)
		if job.PollAttempt != previous.PollAttempt+1 || !job.NextPollAt.After(now) {
			t.Fatalf("slow/error state did not back off: %#v", job)
		}
	}
}

func TestReconcileUsesBoundedPerJobConcurrency(t *testing.T) {
	application, _, arr, _, user, _ := appFixture(t)
	application.options.ReconcileWorkers = 2
	for index := range 3 {
		release := runSearchForTMDB(t, application, user, 500+index)
		if _, _, err := application.Acquire(context.Background(), user, release.Token, "worker-key-0000"+strconv.Itoa(index)); err != nil {
			t.Fatal(err)
		}
	}
	arr.observeStart = make(chan struct{}, 3)
	arr.observeWait = make(chan struct{}, 3)
	done := make(chan struct{})
	go func() {
		application.reconcile(context.Background(), false)
		close(done)
	}()

	for range 2 {
		select {
		case <-arr.observeStart:
		case <-time.After(time.Second):
			t.Fatal("parallel reconciliation did not start")
		}
	}
	select {
	case <-arr.observeStart:
		t.Fatal("reconciliation exceeded the worker bound")
	case <-time.After(50 * time.Millisecond):
	}
	arr.observeWait <- struct{}{}
	select {
	case <-arr.observeStart:
	case <-time.After(time.Second):
		t.Fatal("a blocked job prevented the next job from starting")
	}
	arr.observeWait <- struct{}{}
	arr.observeWait <- struct{}{}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("bounded reconciliation did not finish")
	}
	if maximum := arr.maximumConcurrentObservations(); maximum != 2 {
		t.Fatalf("maximum concurrent observations=%d", maximum)
	}
}

func runSearch(t *testing.T, application *App, user domain.User) domain.Release {
	return runSearchForTMDB(t, application, user, 42)
}

func runSearchForTMDB(t *testing.T, application *App, user domain.User, tmdbID int) domain.Release {
	t.Helper()
	search, err := application.StartSearch(context.Background(), user, domain.Subject{Kind: domain.Movie, TMDBID: tmdbID})
	if err != nil {
		t.Fatal(err)
	}
	if !application.runOneSearch(context.Background()) {
		t.Fatal("search not run")
	}
	result, err := application.Search(context.Background(), user, search.ID)
	if err != nil || len(result.Results) != 1 {
		t.Fatalf("search result=%#v err=%v", result, err)
	}
	return result.Results[0]
}

func appFixture(t *testing.T) (*App, *store.Store, *arrFake, *jellyfinFake, domain.User, time.Time) {
	t.Helper()
	sealer, err := secure.NewSealer(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "app.db"), sealer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Unix(1_700_000_000, 0).UTC()
	user := domain.User{ID: "jellyfin-user", Name: "Viewer", CanMovie: true, CanSeries: true}
	if err := st.UpsertUser(context.Background(), user, now); err != nil {
		t.Fatal(err)
	}
	arr := &arrFake{
		resolved: domain.ResolvedSubject{Backend: "radarr", ArrItemID: 7},
		candidates: []domain.Candidate{{
			Release: domain.Release{Title: "Chosen.Release", SizeBytes: 123, Approved: true, Protocol: "torrent"},
			Payload: []byte(`{"guid":"chosen","indexerId":4,"downloadUrl":"server-only"}`),
		}},
		observe: domain.Observation{State: domain.Queued, Progress: domain.Progress{Text: "Queued", Source: "arr"}},
	}
	jellyfin := &jellyfinFake{user: user}
	application := New(st, arr, arr, jellyfin, qbFake{}, Options{
		SessionTTL: time.Hour, SelectionTTL: 10 * time.Minute, SearchTimeout: time.Minute,
		UpstreamTimeout: time.Second, ReconcileEvery: time.Minute, ProgressEvery: 2 * time.Second,
		AllowAllUsers: true,
	}, slog.New(slog.NewTextHandler(discardWriter{}, nil)))
	application.now = func() time.Time { return now }
	return application, st, arr, jellyfin, user, now
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

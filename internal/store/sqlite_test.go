package store

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/nicolasmarchal/wholphin-companion/internal/domain"
	"github.com/nicolasmarchal/wholphin-companion/internal/secure"
)

func TestCandidateTokensAndStrictIdempotence(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	user := domain.User{ID: "user-a", Name: "A", CanMovie: true, CanSeries: true}
	if err := st.UpsertUser(ctx, user, now); err != nil {
		t.Fatal(err)
	}
	season := 2
	search := domain.Search{ID: "search-a", UserID: user.ID, Subject: domain.Subject{Kind: domain.Season, TMDBID: 100, TVDBID: 200, SeasonNumber: &season}, State: domain.SearchPending, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := st.CreateSearch(ctx, search); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimSearch(ctx, now); err != nil {
		t.Fatal(err)
	}
	candidates := []domain.Candidate{
		{Release: domain.Release{Title: "Show.S02.Pack.A", Indexer: "Indexer A", Approved: true, FullSeason: true, EpisodeNumbers: []int{1, 2}}, Payload: []byte(`{"guid":"a","indexerId":1,"downloadUrl":"secret-a"}`), ExpectedEpisodes: []int{1, 2}},
		{Release: domain.Release{Title: "Show.S02.Pack.B", Indexer: "Indexer B", Approved: true, FullSeason: true, EpisodeNumbers: []int{1, 2}}, Payload: []byte(`{"guid":"b","indexerId":2,"magnetUrl":"secret-b"}`), ExpectedEpisodes: []int{1, 2}},
	}
	resolved := domain.ResolvedSubject{Subject: search.Subject, Backend: "sonarr", ArrItemID: 9, ExpectedEpisodes: []int{1, 2}}
	if err := st.CompleteSearch(ctx, search.ID, resolved, candidates, now, now.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	result, err := st.Search(ctx, search.ID, user.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 2 {
		t.Fatalf("got %d results", len(result.Results))
	}
	if result.Results[0].Token == "" || result.Results[1].Token == "" || result.Results[0].Token == result.Results[1].Token {
		t.Fatalf("selection tokens must be non-empty and distinct: %#v", result.Results)
	}

	requestHash := secure.Hash(result.Results[0].Token)
	reserved, err := st.ReserveJob(ctx, user.ID, result.Results[0].Token, "idempotency-key-0001", requestHash, now)
	if err != nil {
		t.Fatal(err)
	}
	if !reserved.Created || !bytes.Contains(reserved.Payload, []byte(`"guid":"a"`)) {
		t.Fatalf("wrong candidate payload: created=%v payload=%s", reserved.Created, reserved.Payload)
	}
	remaining, err := st.Search(ctx, search.ID, user.ID, now)
	if err != nil {
		t.Fatalf("search after token consumption failed: %v", err)
	}
	if len(remaining.Results) != 1 || remaining.Results[0].Title != "Show.S02.Pack.B" {
		t.Fatalf("consumed candidate should be omitted without hiding others: %#v", remaining.Results)
	}
	replay, err := st.ReserveJob(ctx, user.ID, result.Results[0].Token, "idempotency-key-0001", requestHash, now)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Created || replay.Job.ID != reserved.Job.ID {
		t.Fatal("same key did not replay the same job")
	}
	if _, err = st.ReserveJob(ctx, user.ID, result.Results[1].Token, "idempotency-key-0001", secure.Hash(result.Results[1].Token), now); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("same key with a different token must conflict: %v", err)
	}
	if _, err = st.ReserveJob(ctx, user.ID, result.Results[0].Token, "idempotency-key-0002", requestHash, now); !errors.Is(err, ErrConsumed) {
		t.Fatalf("new key for consumed token: %v", err)
	}
}

func TestRestartTurnsInFlightDispatchIntoNonReplayableUncertainty(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0).UTC()
	path := filepath.Join(t.TempDir(), "restart.db")
	sealer, err := secure.NewSealer(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	st, err := Open(path, sealer)
	if err != nil {
		t.Fatal(err)
	}
	user := domain.User{ID: "user", Name: "User", CanMovie: true}
	if err = st.UpsertUser(ctx, user, now); err != nil {
		t.Fatal(err)
	}
	token := addSelectable(t, st, user.ID, "restart-search", domain.Subject{Kind: domain.Movie, TMDBID: 777}, now)
	reserved, err := st.ReserveJob(ctx, user.ID, token, "restart-key-00001", secure.Hash(token), now)
	if err != nil {
		t.Fatal(err)
	}
	if reserved.Job.State != domain.Dispatching {
		t.Fatalf("state before crash=%s", reserved.Job.State)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path, sealer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	var schemaVersion int
	if err = reopened.db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&schemaVersion); err != nil || schemaVersion != 1 {
		t.Fatalf("schema version=%d err=%v", schemaVersion, err)
	}
	job, err := reopened.Job(ctx, reserved.Job.ID, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != domain.DispatchUncertain || job.ErrorCode != "dispatch_interrupted" {
		t.Fatalf("restart did not preserve uncertainty: %#v", job)
	}
}

func TestStaleUpdateCannotResurrectCancelledJobAndNoopCreatesNoEvent(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0).UTC()
	st := testStore(t)
	user := domain.User{ID: "user", Name: "User", CanMovie: true}
	if err := st.UpsertUser(ctx, user, now); err != nil {
		t.Fatal(err)
	}
	token := addSelectable(t, st, user.ID, "cas-search", domain.Subject{Kind: domain.Movie, TMDBID: 778}, now)
	reserved, err := st.ReserveJob(ctx, user.ID, token, "cas-key-000000001", secure.Hash(token), now)
	if err != nil {
		t.Fatal(err)
	}
	queued := reserved.Job
	queued.State, queued.Progress.Text = domain.Queued, "Queued"
	if err = st.UpdateJob(ctx, queued, "queued", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var tokenBytes, payloadBytes int
	if err = st.db.QueryRowContext(ctx, `SELECT length(token_cipher),length(payload_cipher) FROM release_candidates WHERE consumed_job_id=?`, queued.ID).Scan(&tokenBytes, &payloadBytes); err != nil {
		t.Fatal(err)
	}
	if tokenBytes != 0 || payloadBytes != 0 {
		t.Fatalf("consumed sensitive locators were retained: token=%d payload=%d", tokenBytes, payloadBytes)
	}
	current, _ := st.Job(ctx, queued.ID, user.ID)
	current.State, current.Progress.Text = domain.Cancelled, "Cancelled"
	if err = st.UpdateJob(ctx, current, "cancelled", now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	stale := queued
	stale.State, stale.Progress.Text = domain.Downloading, "Downloading"
	if err = st.UpdateJob(ctx, stale, "downloading", now.Add(3*time.Second)); !errors.Is(err, ErrTerminalState) {
		t.Fatalf("stale update was not rejected: %v", err)
	}
	after, _ := st.Job(ctx, queued.ID, user.ID)
	if after.State != domain.Cancelled {
		t.Fatalf("terminal state resurrected: %s", after.State)
	}
	if err = st.UpdateJob(ctx, after, "cancelled", now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = st.db.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE job_id=?`, after.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("no-op update appended an event: count=%d", count)
	}
}

func TestSelectionTTLIsEnforcedByServer(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	user := domain.User{ID: "user", Name: "User", CanMovie: true}
	if err := st.UpsertUser(ctx, user, now); err != nil {
		t.Fatal(err)
	}
	token := addSelectable(t, st, user.ID, "ttl-search", domain.Subject{Kind: domain.Movie, TMDBID: 88}, now)
	if _, err := st.ReserveJob(ctx, user.ID, token, "ttl-key-000000001", secure.Hash(token), now.Add(2*time.Minute)); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired selection token was accepted: %v", err)
	}
}

func TestRejectedAndUnapprovedCannotBeReserved(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	user := domain.User{ID: "user", Name: "User", CanMovie: true}
	if err := st.UpsertUser(ctx, user, now); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []domain.Candidate{
		{Release: domain.Release{Title: "Rejected", Approved: true, Rejected: true}, Payload: []byte(`{"guid":"rejected"}`)},
		{Release: domain.Release{Title: "Not approved", Approved: false}, Payload: []byte(`{"guid":"unapproved"}`)},
	} {
		id := "search-" + candidate.Title
		search := domain.Search{ID: id, UserID: user.ID, Subject: domain.Subject{Kind: domain.Movie, TMDBID: len(id) + 100}, State: domain.SearchPending, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour)}
		if err := st.CreateSearch(ctx, search); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ClaimSearch(ctx, now); err != nil {
			t.Fatal(err)
		}
		resolved := domain.ResolvedSubject{Subject: search.Subject, Backend: "radarr", ArrItemID: len(id)}
		if err := st.CompleteSearch(ctx, id, resolved, []domain.Candidate{candidate}, now, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		got, err := st.Search(ctx, id, user.ID, now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = st.ReserveJob(ctx, user.ID, got.Results[0].Token, "idempotency-0000001", secure.Hash(got.Results[0].Token), now); !errors.Is(err, ErrRejected) {
			t.Fatalf("candidate %q was selectable: %v", candidate.Title, err)
		}
	}
}

func TestSeasonAndEpisodeOverlapAcrossUsers(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	season, episode := 1, 3
	for _, userID := range []string{"owner", "other"} {
		if err := st.UpsertUser(ctx, domain.User{ID: userID, Name: userID, CanSeries: true}, now); err != nil {
			t.Fatal(err)
		}
	}
	seasonToken := addSelectable(t, st, "owner", "season-search", domain.Subject{Kind: domain.Season, TMDBID: 50, SeasonNumber: &season}, now)
	if _, err := st.ReserveJob(ctx, "owner", seasonToken, "owner-key-0000001", secure.Hash(seasonToken), now); err != nil {
		t.Fatal(err)
	}
	episodeToken := addSelectable(t, st, "other", "episode-search", domain.Subject{Kind: domain.Episode, TMDBID: 50, SeasonNumber: &season, EpisodeNumber: &episode}, now)
	if _, err := st.ReserveJob(ctx, "other", episodeToken, "other-key-0000001", secure.Hash(episodeToken), now); !errors.Is(err, ErrSubjectActive) {
		t.Fatalf("overlapping episode should be blocked without leaking owner: %v", err)
	}
}

func TestEpisodeThenSeasonAndDuplicateMovieAreBlocked(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0).UTC()
	season, episode := 3, 4

	t.Run("episode blocks containing season", func(t *testing.T) {
		st := testStore(t)
		for _, id := range []string{"first", "second"} {
			if err := st.UpsertUser(ctx, domain.User{ID: id, Name: id, CanSeries: true}, now); err != nil {
				t.Fatal(err)
			}
		}
		episodeToken := addSelectable(t, st, "first", "episode-first", domain.Subject{Kind: domain.Episode, TMDBID: 70, SeasonNumber: &season, EpisodeNumber: &episode}, now)
		if _, err := st.ReserveJob(ctx, "first", episodeToken, "episode-key-00001", secure.Hash(episodeToken), now); err != nil {
			t.Fatal(err)
		}
		seasonToken := addSelectable(t, st, "second", "season-second", domain.Subject{Kind: domain.Season, TMDBID: 70, SeasonNumber: &season}, now)
		if _, err := st.ReserveJob(ctx, "second", seasonToken, "season-key-0000001", secure.Hash(seasonToken), now); !errors.Is(err, ErrSubjectActive) {
			t.Fatalf("containing season should be blocked: %v", err)
		}
	})

	t.Run("same movie blocks across users", func(t *testing.T) {
		st := testStore(t)
		for _, id := range []string{"first", "second"} {
			if err := st.UpsertUser(ctx, domain.User{ID: id, Name: id, CanMovie: true}, now); err != nil {
				t.Fatal(err)
			}
		}
		first := addSelectable(t, st, "first", "movie-first", domain.Subject{Kind: domain.Movie, TMDBID: 99}, now)
		if _, err := st.ReserveJob(ctx, "first", first, "movie-key-0000001", secure.Hash(first), now); err != nil {
			t.Fatal(err)
		}
		second := addSelectable(t, st, "second", "movie-second", domain.Subject{Kind: domain.Movie, TMDBID: 99}, now)
		if _, err := st.ReserveJob(ctx, "second", second, "movie-key-0000002", secure.Hash(second), now); !errors.Is(err, ErrSubjectActive) {
			t.Fatalf("duplicate movie should be blocked: %v", err)
		}
	})
}

func addSelectable(t *testing.T, st *Store, user, id string, subject domain.Subject, now time.Time) string {
	t.Helper()
	ctx := context.Background()
	search := domain.Search{ID: id, UserID: user, Subject: subject, State: domain.SearchPending, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := st.CreateSearch(ctx, search); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimSearch(ctx, now); err != nil {
		t.Fatal(err)
	}
	candidate := domain.Candidate{Release: domain.Release{Title: id, Indexer: "Test Indexer", Approved: true, FullSeason: subject.Kind == domain.Season}, Payload: []byte(`{"guid":"x","indexerId":1}`)}
	if err := st.CompleteSearch(ctx, id, domain.ResolvedSubject{Subject: subject, Backend: "sonarr", ArrItemID: 1}, []domain.Candidate{candidate}, now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	result, err := st.Search(ctx, id, user, now)
	if err != nil {
		t.Fatal(err)
	}
	return result.Results[0].Token
}

func testStore(t *testing.T) *Store {
	t.Helper()
	sealer, err := secure.NewSealer(bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	st, err := Open(filepath.Join(t.TempDir(), "companion.db"), sealer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

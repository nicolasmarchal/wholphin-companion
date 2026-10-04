package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/nicolasmarchal/wholphin-companion/internal/domain"
	"github.com/nicolasmarchal/wholphin-companion/internal/secure"
)

var (
	ErrNotFound            = errors.New("not found")
	ErrExpired             = errors.New("expired")
	ErrRejected            = errors.New("release is rejected")
	ErrConsumed            = errors.New("selection token was already consumed")
	ErrSubjectActive       = errors.New("subject already has an active acquisition")
	ErrIdempotencyConflict = errors.New("idempotency key was reused with another request")
	ErrTerminalState       = errors.New("acquisition is already terminal")
	ErrConcurrentUpdate    = errors.New("acquisition changed concurrently")
)

type Store struct {
	db           *sql.DB
	sealer       *secure.Sealer
	reserveMu    sync.Mutex
	changeMu     sync.Mutex
	changeSignal chan struct{}
}

type ReservedJob struct {
	Job     domain.Job
	Payload json.RawMessage
	Created bool
}

type Event struct {
	Sequence int64
	Type     string
	Payload  []byte
}

func Open(path string, sealer *secure.Sealer) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=FULL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
	} {
		if _, err = db.ExecContext(ctx, pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("sqlite setup: %w", err)
		}
	}
	if _, err = db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite migration: %w", err)
	}
	const currentSchemaVersion = 2
	var schemaVersion int
	if err = db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&schemaVersion); err != nil {
		db.Close()
		return nil, fmt.Errorf("read sqlite schema version: %w", err)
	}
	if schemaVersion > currentSchemaVersion {
		db.Close()
		return nil, fmt.Errorf("database schema version %d is newer than supported version %d", schemaVersion, currentSchemaVersion)
	}
	if schemaVersion < 2 {
		tx, migrationErr := db.BeginTx(ctx, nil)
		if migrationErr == nil {
			_, migrationErr = tx.ExecContext(ctx, `ALTER TABLE release_candidates ADD COLUMN policy_override_allowed INTEGER NOT NULL DEFAULT 0`)
		}
		if migrationErr == nil {
			_, migrationErr = tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,applied_at) VALUES(2,?)`, time.Now().UTC().Unix())
		}
		if migrationErr == nil {
			migrationErr = tx.Commit()
		} else if tx != nil {
			_ = tx.Rollback()
		}
		if migrationErr != nil {
			db.Close()
			return nil, fmt.Errorf("sqlite migration v2: %w", migrationErr)
		}
	}
	if _, err = db.ExecContext(ctx, `INSERT OR IGNORE INTO schema_migrations(version,applied_at) VALUES(?,?)`, currentSchemaVersion, time.Now().UTC().Unix()); err != nil {
		db.Close()
		return nil, fmt.Errorf("record sqlite schema version: %w", err)
	}
	// A process may have stopped between claiming and completing a search.
	if _, err = db.ExecContext(ctx, `UPDATE release_searches SET state='pending' WHERE state='running'`); err != nil {
		db.Close()
		return nil, err
	}
	// There is no atomic transaction spanning SQLite and Arr's HTTP endpoint.
	// A persisted dispatching row can therefore mean either "not sent" or
	// "sent, response not persisted" after a crash. Never replay it blindly:
	// make the uncertainty explicit and let queue/history reconciliation require
	// the server-side release fingerprint plus a corroborating Arr identity.
	if _, err = db.ExecContext(ctx, `
UPDATE jobs SET state='dispatch_uncertain',status_text='Dispatch interrupted; reconciling with Arr',
 progress_source='arr',error_code='dispatch_interrupted',updated_at=?
WHERE state='dispatching'`, time.Now().UTC().Unix()); err != nil {
		db.Close()
		return nil, err
	}
	if _, err = db.ExecContext(ctx, `UPDATE release_candidates SET token_cipher=X'',payload_cipher=X'' WHERE consumed_job_id IS NOT NULL`); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, sealer: sealer, changeSignal: make(chan struct{})}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) UpsertUser(ctx context.Context, user domain.User, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO users(jellyfin_user_id,display_name,can_movie,can_series,can_cancel,updated_at)
VALUES(?,?,?,?,?,?)
ON CONFLICT(jellyfin_user_id) DO UPDATE SET display_name=excluded.display_name,
 can_movie=excluded.can_movie,can_series=excluded.can_series,
 can_cancel=excluded.can_cancel,updated_at=excluded.updated_at`,
		user.ID, user.Name, user.CanMovie, user.CanSeries, user.CanCancel, now.Unix())
	return err
}

func (s *Store) CreateSession(ctx context.Context, tokenHash []byte, userID string, now, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO auth_sessions(token_hash,user_id,created_at,last_seen_at,expires_at) VALUES(?,?,?,?,?)`,
		tokenHash, userID, now.Unix(), now.Unix(), expires.Unix())
	return err
}

func (s *Store) Session(ctx context.Context, tokenHash []byte, now time.Time) (domain.User, error) {
	var user domain.User
	var movie, series, cancel int
	err := s.db.QueryRowContext(ctx, `
SELECT u.jellyfin_user_id,u.display_name,u.can_movie,u.can_series,u.can_cancel
FROM auth_sessions a JOIN users u ON u.jellyfin_user_id=a.user_id
WHERE a.token_hash=? AND a.expires_at>?`, tokenHash, now.Unix()).
		Scan(&user.ID, &user.Name, &movie, &series, &cancel)
	if errors.Is(err, sql.ErrNoRows) {
		return user, ErrNotFound
	}
	if err != nil {
		return user, err
	}
	user.CanMovie, user.CanSeries, user.CanCancel = movie != 0, series != 0, cancel != 0
	_, _ = s.db.ExecContext(ctx, `UPDATE auth_sessions SET last_seen_at=? WHERE token_hash=?`, now.Unix(), tokenHash)
	return user, nil
}

func (s *Store) RevokeSession(ctx context.Context, tokenHash []byte) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM auth_sessions WHERE token_hash=?`, tokenHash)
	return err
}

func (s *Store) CreateSearch(ctx context.Context, search domain.Search) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO release_searches(id,user_id,kind,tmdb_id,tvdb_id,season_number,episode_number,state,created_at,updated_at,expires_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`, search.ID, search.UserID, search.Subject.Kind, search.Subject.TMDBID,
		search.Subject.TVDBID, search.Subject.SeasonNumber, search.Subject.EpisodeNumber, search.State,
		search.CreatedAt.Unix(), search.UpdatedAt.Unix(), search.ExpiresAt.Unix())
	return err
}

func (s *Store) ClaimSearch(ctx context.Context, now time.Time) (domain.Search, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Search{}, err
	}
	defer tx.Rollback()
	row := tx.QueryRowContext(ctx, `
SELECT id,user_id,kind,tmdb_id,tvdb_id,season_number,episode_number,state,
 created_at,updated_at,expires_at FROM release_searches
WHERE state='pending' AND expires_at>? ORDER BY created_at LIMIT 1`, now.Unix())
	search, err := scanSearch(row)
	if err != nil {
		return domain.Search{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE release_searches SET state='running',updated_at=? WHERE id=? AND state='pending'`, now.Unix(), search.ID)
	if err != nil {
		return domain.Search{}, err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return domain.Search{}, ErrNotFound
	}
	if err = tx.Commit(); err != nil {
		return domain.Search{}, err
	}
	search.State, search.UpdatedAt = domain.SearchRunning, now
	return search, nil
}

func (s *Store) CompleteSearch(ctx context.Context, searchID string, resolved domain.ResolvedSubject, candidates []domain.Candidate, now, expires time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	expected, _ := json.Marshal(resolved.ExpectedEpisodes)
	for _, candidate := range candidates {
		id, err := secure.Token(18)
		if err != nil {
			return err
		}
		if candidate.Token == "" {
			candidate.Token, err = secure.Token(32)
			if err != nil {
				return err
			}
		}
		tokenCipher, err := s.sealer.Seal([]byte(candidate.Token), []byte(id+":token"))
		if err != nil {
			return err
		}
		payloadCipher, err := s.sealer.Seal(candidate.Payload, []byte(id+":payload"))
		if err != nil {
			return err
		}
		rejections, _ := json.Marshal(candidate.RejectionReasons)
		episodes, _ := json.Marshal(candidate.EpisodeNumbers)
		candidateExpected, _ := json.Marshal(candidate.ExpectedEpisodes)
		_, err = tx.ExecContext(ctx, `
INSERT INTO release_candidates(id,search_id,token_hash,token_cipher,payload_cipher,title,size_bytes,seeders,
 quality,indexer_name,protocol,approved,rejected,policy_override_allowed,rejections_json,full_season,release_season_number,
 episode_numbers_json,expected_episodes_json,expires_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, searchID, secure.Hash(candidate.Token), tokenCipher,
			payloadCipher, candidate.Title, candidate.SizeBytes, candidate.Seeders, candidate.Quality,
			candidate.Indexer, candidate.Protocol, candidate.Approved, candidate.Rejected, candidate.PolicyOverrideAllowed, string(rejections),
			candidate.FullSeason, candidate.SeasonNumber, string(episodes), string(candidateExpected), expires.Unix())
		if err != nil {
			return err
		}
	}
	result, err := tx.ExecContext(ctx, `
UPDATE release_searches SET state='completed',backend=?,arr_item_id=?,arr_episode_id=?,tvdb_id=?,
 expected_episodes_json=?,updated_at=?,expires_at=? WHERE id=? AND state='running'`, resolved.Backend,
		resolved.ArrItemID, resolved.ArrEpisodeID, resolved.TVDBID, string(expected), now.Unix(), expires.Unix(), searchID)
	if err != nil {
		return err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return ErrNotFound
	}
	return tx.Commit()
}

func (s *Store) FailSearch(ctx context.Context, id, code, message string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE release_searches SET state='failed',error_code=?,error_message=?,updated_at=? WHERE id=?`, code, message, now.Unix(), id)
	return err
}

func (s *Store) Search(ctx context.Context, id, userID string, now time.Time) (domain.Search, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id,user_id,kind,tmdb_id,tvdb_id,season_number,episode_number,state,backend,arr_item_id,
 arr_episode_id,expected_episodes_json,error_code,error_message,created_at,updated_at,expires_at
FROM release_searches WHERE id=? AND user_id=?`, id, userID)
	search, err := scanFullSearch(row)
	if errors.Is(err, sql.ErrNoRows) {
		return search, ErrNotFound
	}
	if err != nil {
		return search, err
	}
	if !search.ExpiresAt.After(now) {
		search.State = domain.SearchExpired
		return search, nil
	}
	if search.State != domain.SearchCompleted {
		return search, nil
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id,token_cipher,title,size_bytes,seeders,quality,indexer_name,protocol,approved,rejected,
 policy_override_allowed,rejections_json,full_season,release_season_number,episode_numbers_json,expires_at
FROM release_candidates WHERE search_id=? AND consumed_job_id IS NULL ORDER BY rejected,title`, id)
	if err != nil {
		return search, err
	}
	defer rows.Close()
	for rows.Next() {
		var release domain.Release
		var candidateID string
		var tokenCipher []byte
		var seeders sql.NullInt64
		var season sql.NullInt64
		var approved, rejected, policyOverrideAllowed, full int
		var rejections, episodes string
		var expires int64
		if err = rows.Scan(&candidateID, &tokenCipher, &release.Title, &release.SizeBytes, &seeders,
			&release.Quality, &release.Indexer, &release.Protocol, &approved, &rejected, &policyOverrideAllowed, &rejections,
			&full, &season, &episodes, &expires); err != nil {
			return search, err
		}
		token, err := s.sealer.Open(tokenCipher, []byte(candidateID+":token"))
		if err != nil {
			return search, err
		}
		release.Token = string(token)
		release.Approved, release.Rejected, release.PolicyOverrideAllowed, release.FullSeason = approved != 0, rejected != 0, policyOverrideAllowed != 0, full != 0
		if seeders.Valid {
			v := int(seeders.Int64)
			release.Seeders = &v
		}
		if season.Valid {
			v := int(season.Int64)
			release.SeasonNumber = &v
		}
		_ = json.Unmarshal([]byte(rejections), &release.RejectionReasons)
		_ = json.Unmarshal([]byte(episodes), &release.EpisodeNumbers)
		release.ExpiresAt = time.Unix(expires, 0).UTC()
		search.Results = append(search.Results, release)
	}
	return search, rows.Err()
}

func (s *Store) ReserveJob(ctx context.Context, userID, selectionToken, idemKey string, requestHash []byte, now time.Time) (ReservedJob, error) {
	s.reserveMu.Lock()
	defer s.reserveMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReservedJob{}, err
	}
	defer tx.Rollback()

	var existingHash []byte
	var existingJob string
	err = tx.QueryRowContext(ctx, `SELECT request_hash,job_id FROM idempotency_keys WHERE user_id=? AND operation='acquire' AND key=? AND expires_at>?`, userID, idemKey, now.Unix()).Scan(&existingHash, &existingJob)
	if err == nil {
		if !bytes.Equal(existingHash, requestHash) {
			return ReservedJob{}, ErrIdempotencyConflict
		}
		job, err := getJob(ctx, tx, existingJob, userID)
		return ReservedJob{Job: job}, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ReservedJob{}, err
	}

	var candidateID, title, candidateIndexer, backend, kind, expectedJSON string
	var payloadCipher []byte
	var consumed sql.NullString
	var tmdbID, tvdbID, arrItemID, arrEpisodeID int
	var season, episode sql.NullInt64
	var approved, rejected, policyOverrideAllowed int
	var expires int64
	err = tx.QueryRowContext(ctx, `
SELECT c.id,c.payload_cipher,c.title,c.indexer_name,c.approved,c.rejected,c.expires_at,c.consumed_job_id,s.backend,s.kind,s.tmdb_id,
 s.tvdb_id,s.season_number,s.episode_number,s.arr_item_id,s.arr_episode_id,c.expected_episodes_json,c.policy_override_allowed
FROM release_candidates c JOIN release_searches s ON s.id=c.search_id
WHERE c.token_hash=? AND s.user_id=?`, secure.Hash(selectionToken), userID).
		Scan(&candidateID, &payloadCipher, &title, &candidateIndexer, &approved, &rejected, &expires, &consumed, &backend, &kind,
			&tmdbID, &tvdbID, &season, &episode, &arrItemID, &arrEpisodeID, &expectedJSON, &policyOverrideAllowed)
	if errors.Is(err, sql.ErrNoRows) {
		return ReservedJob{}, ErrNotFound
	}
	if err != nil {
		return ReservedJob{}, err
	}
	if expires <= now.Unix() {
		return ReservedJob{}, ErrExpired
	}
	if (rejected != 0 || approved == 0) && policyOverrideAllowed == 0 {
		return ReservedJob{}, ErrRejected
	}
	if consumed.Valid {
		return ReservedJob{}, ErrConsumed
	}
	var activeID string
	activeQuery := `SELECT id FROM jobs WHERE tmdb_id=? AND state NOT IN ('available','cancelled','error')`
	activeArgs := []any{tmdbID}
	switch domain.SubjectKind(kind) {
	case domain.Movie:
		activeQuery += ` AND kind='movie'`
	case domain.Season:
		// A season pack overlaps every episode acquisition in that season.
		activeQuery += ` AND kind IN ('season','episode') AND season_number=?`
		activeArgs = append(activeArgs, season.Int64)
	case domain.Episode:
		// The containing season pack or the same episode both overlap.
		activeQuery += ` AND season_number=? AND (kind='season' OR (kind='episode' AND episode_number=?))`
		activeArgs = append(activeArgs, season.Int64, episode.Int64)
	}
	activeQuery += ` LIMIT 1`
	err = tx.QueryRowContext(ctx, activeQuery, activeArgs...).Scan(&activeID)
	if err == nil {
		return ReservedJob{}, ErrSubjectActive
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ReservedJob{}, err
	}

	jobID, err := secure.Token(18)
	if err != nil {
		return ReservedJob{}, err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO jobs(id,user_id,candidate_id,kind,tmdb_id,tvdb_id,season_number,episode_number,backend,
 arr_item_id,arr_episode_id,expected_episodes_json,title,state,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, jobID, userID, candidateID, kind, tmdbID, tvdbID,
		nullableInt(season), nullableInt(episode), backend, arrItemID, arrEpisodeID, expectedJSON, title,
		domain.Dispatching, now.Unix(), now.Unix())
	if err != nil {
		return ReservedJob{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE release_candidates SET consumed_job_id=?,token_cipher=X'' WHERE id=? AND consumed_job_id IS NULL`, jobID, candidateID)
	if err != nil {
		return ReservedJob{}, err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return ReservedJob{}, errors.New("candidate consumption race")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO idempotency_keys(user_id,operation,key,request_hash,job_id,created_at,expires_at) VALUES(?,'acquire',?,?,?,?,?)`, userID, idemKey, requestHash, jobID, now.Unix(), now.Add(30*24*time.Hour).Unix())
	if err != nil {
		return ReservedJob{}, err
	}
	payload, err := s.sealer.Open(payloadCipher, []byte(candidateID+":payload"))
	if err != nil {
		return ReservedJob{}, err
	}
	var locator struct {
		GUID            string `json:"guid"`
		IndexerID       int    `json:"indexerId"`
		DownloadURL     string `json:"downloadUrl"`
		InfoHash        string `json:"infoHash"`
		TorrentInfoHash string `json:"torrentInfoHash"`
	}
	_ = json.Unmarshal(payload, &locator)
	if locator.GUID == "" || locator.IndexerID <= 0 {
		return ReservedJob{}, ErrRejected
	}
	infoHash := locator.TorrentInfoHash
	if infoHash == "" {
		infoHash = locator.InfoHash
	}
	guidHash := secure.Hash(locator.GUID)
	indexerHash := hashNormalized(candidateIndexer)
	urlHash := hashOptional(locator.DownloadURL)
	infoHashDigest := hashNormalized(infoHash)
	if len(indexerHash) == 0 && len(urlHash) == 0 && len(infoHashDigest) == 0 {
		return ReservedJob{}, ErrRejected
	}
	if _, err = tx.ExecContext(ctx, `UPDATE jobs SET release_guid_hash=?,release_indexer_id=?,release_indexer_hash=?,release_url_hash=?,release_info_hash=? WHERE id=?`, guidHash, locator.IndexerID, indexerHash, urlHash, infoHashDigest, jobID); err != nil {
		return ReservedJob{}, err
	}
	job, err := getJob(ctx, tx, jobID, userID)
	if err != nil {
		return ReservedJob{}, err
	}
	if err = tx.Commit(); err != nil {
		return ReservedJob{}, err
	}
	return ReservedJob{Job: job, Payload: payload, Created: true}, nil
}

func (s *Store) Job(ctx context.Context, id, userID string) (domain.Job, error) {
	job, err := getJob(ctx, s.db, id, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return job, ErrNotFound
	}
	return job, err
}

func (s *Store) Jobs(ctx context.Context, userID string, subject *domain.Subject, mediaType string, active bool) ([]domain.Job, error) {
	query := `SELECT ` + jobColumns + ` FROM jobs WHERE user_id=?`
	args := []any{userID}
	if subject != nil {
		query += ` AND tmdb_id=?`
		args = append(args, subject.TMDBID)
		if subject.Kind != "" {
			query += ` AND kind=?`
			args = append(args, subject.Kind)
		} else if mediaType == "movie" {
			query += ` AND kind='movie'`
		} else if mediaType == "tv" {
			query += ` AND kind IN ('season','episode')`
		}
		if subject.SeasonNumber != nil {
			query += ` AND season_number=?`
			args = append(args, *subject.SeasonNumber)
		}
		if subject.EpisodeNumber != nil {
			query += ` AND episode_number=?`
			args = append(args, *subject.EpisodeNumber)
		}
	}
	if active {
		query += ` AND state NOT IN ('available','cancelled','error')`
	}
	query += ` ORDER BY updated_at DESC`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []domain.Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, job)
	}
	return result, rows.Err()
}

func (s *Store) ActiveJobs(ctx context.Context, now time.Time, force bool) ([]domain.Job, error) {
	query := `SELECT ` + jobColumns + ` FROM jobs WHERE state NOT IN ('available','cancelled','error')`
	args := []any{}
	if !force {
		query += ` AND next_poll_at<=?`
		args = append(args, now.Unix())
	}
	query += ` ORDER BY next_poll_at,updated_at`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []domain.Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, job)
	}
	return result, rows.Err()
}

func (s *Store) UpdateJob(ctx context.Context, job domain.Job, eventType string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id=?`, job.ID))
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if current.Terminal() {
		if sameStoredJob(current, job) {
			return nil
		}
		return ErrTerminalState
	}
	if current.Version != job.Version {
		return ErrConcurrentUpdate
	}
	if sameStoredJob(current, job) {
		return nil
	}
	materialChanged := !sameObservableJob(current, job)
	auditChanged := !sameAuditJob(current, job)
	storedUpdatedAt := current.UpdatedAt
	if materialChanged {
		storedUpdatedAt = now
	}
	expected, _ := json.Marshal(job.ExpectedEpisodes)
	result, err := tx.ExecContext(ctx, `
UPDATE jobs SET state=?,version=version+1,poll_attempt=?,next_poll_at=?,arr_queue_id=?,expected_episodes_json=?,percent=?,downloaded_bytes=?,total_bytes=?,
 bytes_per_second=?,eta_seconds=?,status_text=?,progress_source=?,download_id=?,jellyfin_item_id=?,
 error_code=?,error_message=?,updated_at=? WHERE id=? AND state=? AND version=?`, job.State, job.PollAttempt, job.NextPollAt.Unix(), job.ArrQueueID, string(expected),
		job.Progress.Percent, job.Progress.DownloadedBytes, job.Progress.TotalBytes, job.Progress.BytesPerSecond,
		job.Progress.ETASeconds, job.Progress.Text, job.Progress.Source, job.DownloadID, job.JellyfinItemID,
		job.ErrorCode, job.ErrorMessage, storedUpdatedAt.Unix(), job.ID, current.State, job.Version)
	if err != nil {
		return err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return ErrConcurrentUpdate
	}
	if job.State != domain.Dispatching {
		if _, err = tx.ExecContext(ctx, `UPDATE release_candidates SET payload_cipher=X'',token_cipher=X'' WHERE id=(SELECT candidate_id FROM jobs WHERE id=?)`, job.ID); err != nil {
			return err
		}
	}
	if auditChanged {
		job.UpdatedAt = now
		payload, _ := json.Marshal(job)
		if _, err = tx.ExecContext(ctx, `INSERT INTO events(user_id,job_id,event_type,payload_json,created_at) VALUES(?,?,?,?,?)`, job.UserID, job.ID, eventType, string(payload), now.Unix()); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if materialChanged {
		s.notifyJobChange()
	}
	return nil
}

func (s *Store) Events(ctx context.Context, userID string, after int64, limit int) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sequence,event_type,payload_json FROM events WHERE user_id=? AND sequence>? ORDER BY sequence LIMIT ?`, userID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var event Event
		if err := rows.Scan(&event.Sequence, &event.Type, &event.Payload); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

// JobChangeSignal is an in-memory broadcast edge. Callers capture the channel
// before reading canonical jobs; every material commit closes the current
// channel and replaces it so all live streams wake without creating an audit
// event for every speed or ETA sample.
func (s *Store) JobChangeSignal() <-chan struct{} {
	s.changeMu.Lock()
	defer s.changeMu.Unlock()
	return s.changeSignal
}

func (s *Store) notifyJobChange() {
	s.changeMu.Lock()
	close(s.changeSignal)
	s.changeSignal = make(chan struct{})
	s.changeMu.Unlock()
}

func (s *Store) Cleanup(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM auth_sessions WHERE expires_at<=?`, now.Unix())
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM idempotency_keys WHERE expires_at<=?`, now.Unix())
	if err != nil {
		return err
	}
	// Expired tracker locators and recoverable tokens are no longer needed.
	// Consumed candidate rows remain as referential audit records, but their
	// encrypted sensitive blobs are logically destroyed.
	_, err = s.db.ExecContext(ctx, `UPDATE release_candidates SET token_cipher=X'',payload_cipher=X'' WHERE expires_at<=? OR consumed_job_id IS NOT NULL`, now.Unix())
	if err != nil {
		return err
	}
	// Audit events are deliberately coalesced; live SSE telemetry uses the
	// in-memory change broadcast and reconnects begin with a canonical snapshot.
	_, err = s.db.ExecContext(ctx, `DELETE FROM events WHERE created_at<=?`, now.Add(-30*24*time.Hour).Unix())
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM jobs WHERE state IN ('available','cancelled','error') AND updated_at<=?`, now.Add(-90*24*time.Hour).Unix())
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM release_candidates WHERE expires_at<=? AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.candidate_id=release_candidates.id)`, now.Unix())
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM release_searches WHERE expires_at<=? AND NOT EXISTS (SELECT 1 FROM release_candidates c WHERE c.search_id=release_searches.id)`, now.Unix())
	return err
}

func sameStoredJob(left, right domain.Job) bool {
	left.CreatedAt, left.UpdatedAt = time.Time{}, time.Time{}
	right.CreatedAt, right.UpdatedAt = time.Time{}, time.Time{}
	return reflect.DeepEqual(left, right)
}

func sameObservableJob(left, right domain.Job) bool {
	left.PollAttempt, right.PollAttempt = 0, 0
	left.NextPollAt, right.NextPollAt = time.Time{}, time.Time{}
	left.Version, right.Version = 0, 0
	return sameStoredJob(left, right)
}

func sameAuditJob(left, right domain.Job) bool {
	return left.State == right.State &&
		left.Progress.Text == right.Progress.Text &&
		left.Progress.Source == right.Progress.Source &&
		progressAuditBucket(left.Progress.Percent) == progressAuditBucket(right.Progress.Percent) &&
		left.JellyfinItemID == right.JellyfinItemID &&
		left.ErrorCode == right.ErrorCode &&
		left.ErrorMessage == right.ErrorMessage
}

func progressAuditBucket(percent *float64) int {
	if percent == nil {
		return -1
	}
	value := *percent
	if value < 0 {
		value = 0
	} else if value > 100 {
		value = 100
	}
	return int(value / 5)
}

const jobColumns = `id,user_id,kind,tmdb_id,tvdb_id,season_number,episode_number,backend,arr_item_id,
arr_episode_id,arr_queue_id,expected_episodes_json,release_guid_hash,release_indexer_id,release_indexer_hash,release_url_hash,release_info_hash,title,state,version,poll_attempt,next_poll_at,percent,downloaded_bytes,total_bytes,
bytes_per_second,eta_seconds,status_text,progress_source,download_id,jellyfin_item_id,error_code,error_message,
created_at,updated_at`

type rowScanner interface{ Scan(...any) error }

func getJob(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id, userID string) (domain.Job, error) {
	return scanJob(q.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id=? AND user_id=?`, id, userID))
}

func scanJob(row rowScanner) (domain.Job, error) {
	var job domain.Job
	var kind string
	var season, episode sql.NullInt64
	var expected string
	var percent sql.NullFloat64
	var downloaded, total, speed, eta sql.NullInt64
	var nextPoll, created, updated int64
	err := row.Scan(&job.ID, &job.UserID, &kind, &job.Subject.TMDBID, &job.Subject.TVDBID, &season,
		&episode, &job.Backend, &job.ArrItemID, &job.ArrEpisodeID, &job.ArrQueueID, &expected,
		&job.ReleaseGUIDHash, &job.ReleaseIndexerID, &job.ReleaseIndexerHash, &job.ReleaseURLHash, &job.ReleaseInfoHash, &job.Title, &job.State, &job.Version, &job.PollAttempt, &nextPoll, &percent, &downloaded, &total, &speed, &eta, &job.Progress.Text, &job.Progress.Source,
		&job.DownloadID, &job.JellyfinItemID, &job.ErrorCode, &job.ErrorMessage, &created, &updated)
	if err != nil {
		return job, err
	}
	job.Subject.Kind = domain.SubjectKind(kind)
	if season.Valid {
		v := int(season.Int64)
		job.Subject.SeasonNumber = &v
	}
	if episode.Valid {
		v := int(episode.Int64)
		job.Subject.EpisodeNumber = &v
	}
	if percent.Valid {
		job.Progress.Percent = &percent.Float64
	}
	if downloaded.Valid {
		v := downloaded.Int64
		job.Progress.DownloadedBytes = &v
	}
	if total.Valid {
		v := total.Int64
		job.Progress.TotalBytes = &v
	}
	if speed.Valid {
		v := speed.Int64
		job.Progress.BytesPerSecond = &v
	}
	if eta.Valid {
		v := eta.Int64
		job.Progress.ETASeconds = &v
	}
	_ = json.Unmarshal([]byte(expected), &job.ExpectedEpisodes)
	job.CreatedAt, job.UpdatedAt = time.Unix(created, 0).UTC(), time.Unix(updated, 0).UTC()
	job.NextPollAt = time.Unix(nextPoll, 0).UTC()
	return job, nil
}

func hashOptional(value string) []byte {
	if value == "" {
		return []byte{}
	}
	return secure.Hash(value)
}

func hashNormalized(value string) []byte {
	return hashOptional(strings.ToLower(strings.TrimSpace(value)))
}

func scanSearch(row rowScanner) (domain.Search, error) {
	var search domain.Search
	var kind string
	var season, episode sql.NullInt64
	var created, updated, expires int64
	err := row.Scan(&search.ID, &search.UserID, &kind, &search.Subject.TMDBID, &search.Subject.TVDBID,
		&season, &episode, &search.State, &created, &updated, &expires)
	if err != nil {
		return search, err
	}
	fillSearch(&search, kind, season, episode, created, updated, expires)
	return search, nil
}

func scanFullSearch(row rowScanner) (domain.Search, error) {
	var search domain.Search
	var kind, expected string
	var season, episode sql.NullInt64
	var created, updated, expires int64
	err := row.Scan(&search.ID, &search.UserID, &kind, &search.Subject.TMDBID, &search.Subject.TVDBID,
		&season, &episode, &search.State, &search.Backend, &search.ArrItemID, &search.ArrEpisodeID,
		&expected, &search.ErrorCode, &search.ErrorMessage, &created, &updated, &expires)
	if err != nil {
		return search, err
	}
	fillSearch(&search, kind, season, episode, created, updated, expires)
	_ = json.Unmarshal([]byte(expected), &search.ExpectedEpisodes)
	return search, nil
}

func fillSearch(search *domain.Search, kind string, season, episode sql.NullInt64, created, updated, expires int64) {
	search.Subject.Kind = domain.SubjectKind(kind)
	if season.Valid {
		v := int(season.Int64)
		search.Subject.SeasonNumber = &v
	}
	if episode.Valid {
		v := int(episode.Int64)
		search.Subject.EpisodeNumber = &v
	}
	search.CreatedAt = time.Unix(created, 0).UTC()
	search.UpdatedAt = time.Unix(updated, 0).UTC()
	search.ExpiresAt = time.Unix(expires, 0).UTC()
}

func nullableInt(value sql.NullInt64) any {
	if value.Valid {
		return value.Int64
	}
	return nil
}

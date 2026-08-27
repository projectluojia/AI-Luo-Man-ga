package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/tools/weather"
)

var _ weather.Cache = (*Store)(nil)

func init() {
	registerMigration(25, `
CREATE TABLE weather_cache (
  app_id TEXT NOT NULL CHECK(length(app_id) BETWEEN 1 AND 128),
  cache_key TEXT NOT NULL CHECK(length(cache_key) BETWEEN 1 AND 128),
  provider TEXT NOT NULL CHECK(length(provider) BETWEEN 1 AND 32),
  payload TEXT NOT NULL CHECK(length(payload) BETWEEN 1 AND 262144 AND json_valid(payload)),
  source_revision TEXT NOT NULL CHECK(length(source_revision) BETWEEN 1 AND 256),
  fetched_at TEXT NOT NULL,
  valid_until TEXT NOT NULL,
  PRIMARY KEY (app_id, cache_key)
);
CREATE INDEX weather_cache_expiry_idx ON weather_cache(app_id, valid_until);
`)
}

func (s *Store) GetWeather(ctx context.Context, appID, cacheKey string, now time.Time) (_ weather.CacheEntry, hit bool, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "weather_cache_get", started, resultErr) }()
	if err := identity.ValidateAppID(appID); err != nil {
		return weather.CacheEntry{}, false, err
	}
	if cacheKey == "" || len(cacheKey) > 128 {
		return weather.CacheEntry{}, false, fmt.Errorf("%w: weather cache key is invalid", weather.ErrInvalidRequest)
	}
	var entry weather.CacheEntry
	var payload, fetchedAt, validUntil string
	err := s.db.QueryRowContext(ctx, `
SELECT cache_key,provider,payload,source_revision,fetched_at,valid_until
FROM weather_cache WHERE app_id=? AND cache_key=?`, appID, cacheKey,
	).Scan(&entry.Key, &entry.Provider, &payload, &entry.SourceRevision, &fetchedAt, &validUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return weather.CacheEntry{}, false, nil
	}
	if err != nil {
		return weather.CacheEntry{}, false, fmt.Errorf("read weather cache: %w", err)
	}
	parsedFetched, err := time.Parse(time.RFC3339Nano, fetchedAt)
	if err != nil {
		return weather.CacheEntry{}, false, fmt.Errorf("parse weather cache fetched_at: %w", err)
	}
	parsedUntil, err := time.Parse(time.RFC3339Nano, validUntil)
	if err != nil {
		return weather.CacheEntry{}, false, fmt.Errorf("parse weather cache valid_until: %w", err)
	}
	if !now.Before(parsedUntil) {
		return weather.CacheEntry{}, false, nil
	}
	entry.Payload = []byte(payload)
	entry.FetchedAt = parsedFetched
	entry.ValidUntil = parsedUntil
	return entry, true, nil
}

func (s *Store) PutWeather(ctx context.Context, appID string, entry weather.CacheEntry) (resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "weather_cache_put", started, resultErr) }()
	if err := identity.ValidateAppID(appID); err != nil {
		return err
	}
	if entry.Key == "" || len(entry.Key) > 128 || entry.Provider == "" || len(entry.Provider) > 32 ||
		len(entry.Payload) == 0 || len(entry.Payload) > 256<<10 || entry.SourceRevision == "" ||
		entry.FetchedAt.IsZero() || entry.ValidUntil.IsZero() || !entry.ValidUntil.After(entry.FetchedAt) {
		return fmt.Errorf("%w: weather cache entry is invalid", weather.ErrInvalidRequest)
	}
	tx, err := s.beginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin weather cache write: %w", err)
	}
	defer s.finishTx(tx, &resultErr, "put weather cache")
	if _, err := tx.ExecContext(ctx, `DELETE FROM weather_cache WHERE app_id=? AND valid_until<=?`,
		appID, nowUTC(entry.FetchedAt)); err != nil {
		return fmt.Errorf("expire weather cache: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO weather_cache(app_id,cache_key,provider,payload,source_revision,fetched_at,valid_until)
VALUES(?,?,?,?,?,?,?)
ON CONFLICT(app_id,cache_key) DO UPDATE SET
  provider=excluded.provider,
  payload=excluded.payload,
  source_revision=excluded.source_revision,
  fetched_at=excluded.fetched_at,
  valid_until=excluded.valid_until`,
		appID, entry.Key, entry.Provider, string(entry.Payload), entry.SourceRevision,
		entry.FetchedAt.UTC().Format(time.RFC3339Nano), entry.ValidUntil.UTC().Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("save weather cache: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit weather cache: %w", err)
	}
	return nil
}

func nowUTC(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

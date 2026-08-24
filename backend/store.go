package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Link is one shortened URL record. Owner is the Google "sub" of the
// user who created it — nil (NULL in SQL) for anonymous links.
type Link struct {
	Code      string    `json:"code"`
	URL       string    `json:"url"`
	Title     *string   `json:"title,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	Clicks    int64     `json:"clicks"`
	Owner     *string   `json:"owner,omitempty"`
}

// Sentinel errors: handlers compare against these instead of parsing
// error strings. This is the idiomatic Go way to classify failures.
var (
	ErrNotFound  = errors.New("link not found")
	ErrDuplicate = errors.New("short code already exists")
)

// Store is the interface the HTTP handlers need from the data layer.
//
// Why an interface? So tests can use a fake in-memory store and run
// without a real database. Production code uses PostgresStore.
type Store interface {
	Save(ctx context.Context, code, url, title string, owner *string) error
	Get(ctx context.Context, code string) (Link, error)
	IncrementClicks(ctx context.Context, code string) error
	UpsertUser(ctx context.Context, u UserInfo) error
	ListLinksByOwner(ctx context.Context, sub string) ([]Link, error)
	CountLinksByOwner(ctx context.Context, sub string) (int, error)
	DeleteLink(ctx context.Context, code, owner string) error
	UpdateLink(ctx context.Context, code, owner, title, url string) error
	Ping(ctx context.Context) error
}

// PostgresConfig holds connection settings, read from environment
// variables. In Kubernetes these come from a ConfigMap (non-secret)
// and a Secret (password).
type PostgresConfig struct {
	Host     string
	Port     string
	User     string
	Password string
	Database string
}

func PostgresConfigFromEnv() PostgresConfig {
	return PostgresConfig{
		Host:     envOr("POSTGRES_HOST", "localhost"),
		Port:     envOr("POSTGRES_PORT", "5432"),
		User:     envOr("POSTGRES_USER", "shortener"),
		Password: os.Getenv("POSTGRES_PASSWORD"),
		Database: envOr("POSTGRES_DB", "shortener"),
	}
}

// DSN builds the connection string. Keyword format is used because it
// tolerates special characters in the password better than URL format.
func (c PostgresConfig) DSN() string {
	return fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		c.Host, c.Port, c.User, c.Password, c.Database)
}

// PostgresStore implements Store using PostgreSQL.
type PostgresStore struct {
	pool *pgxpool.Pool
}

// migrateSQL runs on every startup. IF NOT EXISTS makes it idempotent:
// safe to run again and again — the first replica creates the table,
// later replicas (or restarts) are no-ops. This is the simplest
// "schema on boot" pattern, fine for small services.
//
// Order matters: the users table must exist BEFORE links, because
// links.owner_sub is a foreign key pointing at users.google_sub.
const migrateSQL = `
CREATE TABLE IF NOT EXISTS users (
    google_sub TEXT PRIMARY KEY,
    email      TEXT NOT NULL,
    name       TEXT,
    picture    TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS links (
    code       TEXT PRIMARY KEY,
    url        TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    clicks     BIGINT NOT NULL DEFAULT 0
);

ALTER TABLE links
    ADD COLUMN IF NOT EXISTS owner_sub TEXT
    REFERENCES users(google_sub) ON DELETE SET NULL;

ALTER TABLE links
    ADD COLUMN IF NOT EXISTS title TEXT;`

// NewPostgresStore connects (with retries) and applies the migration.
//
// Retries matter in Kubernetes: the API pod and the Postgres pod start
// at roughly the same time, and Postgres is slower. Without retries the
// API would crash and rely on restarts; with retries it just waits.
func NewPostgresStore(ctx context.Context, cfg PostgresConfig, logger *slog.Logger) (*PostgresStore, error) {
	pool, err := connectWithRetry(ctx, cfg, 15, 2*time.Second, logger)
	if err != nil {
		return nil, err
	}

	if _, err := pool.Exec(ctx, migrateSQL); err != nil {
		pool.Close()
		return nil, fmt.Errorf("apply migration: %w", err)
	}
	logger.Info("database ready, schema ensured")
	return &PostgresStore{pool: pool}, nil
}

func connectWithRetry(ctx context.Context, cfg PostgresConfig, attempts int, delay time.Duration, logger *slog.Logger) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("parse postgres config: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	var lastErr error
	for i := 1; i <= attempts; i++ {
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		lastErr = pool.Ping(pingCtx)
		cancel()
		if lastErr == nil {
			return pool, nil
		}
		logger.Warn("postgres not ready yet", "attempt", i, "of", attempts, "error", lastErr)

		select {
		case <-ctx.Done():
			pool.Close()
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	pool.Close()
	return nil, fmt.Errorf("postgres unreachable after %d attempts: %w", attempts, lastErr)
}

// Save inserts a new short link. owner is nil for anonymous links.
// A collision on the code returns ErrDuplicate so the handler can
// retry with a fresh code.
func (s *PostgresStore) Save(ctx context.Context, code, url, title string, owner *string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO links (code, url, title, owner_sub) VALUES ($1, $2, $3, $4)`, code, url, title, owner)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
			return ErrDuplicate
		}
		return err
	}
	return nil
}

// Get looks up a link by its short code.
func (s *PostgresStore) Get(ctx context.Context, code string) (Link, error) {
	var l Link
	err := s.pool.QueryRow(ctx,
		`SELECT code, url, title, created_at, clicks, owner_sub FROM links WHERE code = $1`, code).
		Scan(&l.Code, &l.URL, &l.Title, &l.CreatedAt, &l.Clicks, &l.Owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return Link{}, ErrNotFound
	}
	if err != nil {
		return Link{}, err
	}
	return l, nil
}

// UpsertUser inserts a user on first sign-in, or refreshes their
// profile on later sign-ins. ON CONFLICT = "if the PRIMARY KEY already
// exists, UPDATE instead of failing" — that is the upsert pattern.
func (s *PostgresStore) UpsertUser(ctx context.Context, u UserInfo) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO users (google_sub, email, name, picture)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (google_sub)
		DO UPDATE SET email = EXCLUDED.email,
		              name = EXCLUDED.name,
		              picture = EXCLUDED.picture`,
		u.Sub, u.Email, u.Name, u.Picture)
	return err
}

// ListLinksByOwner returns every link created by one user, newest
// first. This powers the "My links" section in the frontend.
func (s *PostgresStore) ListLinksByOwner(ctx context.Context, sub string) ([]Link, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT code, url, title, created_at, clicks, owner_sub
		 FROM links WHERE owner_sub = $1
		 ORDER BY created_at DESC LIMIT 100`, sub)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	links := make([]Link, 0, 16)
	for rows.Next() {
		var l Link
		if err := rows.Scan(&l.Code, &l.URL, &l.Title, &l.CreatedAt, &l.Clicks, &l.Owner); err != nil {
			return nil, err
		}
		links = append(links, l)
	}
	return links, rows.Err()
}

// IncrementClicks bumps the click counter. Kept separate from Get so a
// redirect can still succeed even if counting fails.
func (s *PostgresStore) IncrementClicks(ctx context.Context, code string) error {
	_, err := s.pool.Exec(ctx, `UPDATE links SET clicks = clicks + 1 WHERE code = $1`, code)
	return err
}

// CountLinksByOwner returns the number of links owned by one user —
// used to enforce the per-user quota.
func (s *PostgresStore) CountLinksByOwner(ctx context.Context, sub string) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM links WHERE owner_sub = $1`, sub).Scan(&count)
	return count, err
}

// DeleteLink removes a link ONLY if it belongs to the specified owner.
// If the code doesn't exist or belongs to someone else, returns ErrNotFound.
// This prevents one user from deleting another user's links.
func (s *PostgresStore) DeleteLink(ctx context.Context, code, owner string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM links WHERE code = $1 AND owner_sub = $2`, code, owner)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateLink changes the title of a link. Only the owner can update
// their own links. Returns ErrNotFound if the code is missing or
// belongs to another user.
func (s *PostgresStore) UpdateLink(ctx context.Context, code, owner, title, url string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE links SET title = $1, url = $2 WHERE code = $3 AND owner_sub = $4`, title, url, code, owner)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Ping verifies the database is reachable RIGHT NOW. This backs the
// readiness probe: if the DB is gone, this pod stops receiving traffic.
func (s *PostgresStore) Ping(ctx context.Context) error {
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return s.pool.Ping(pingCtx)
}

// Close releases the connection pool.
func (s *PostgresStore) Close() {
	s.pool.Close()
}

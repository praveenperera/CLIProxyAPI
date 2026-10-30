package management

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	log "github.com/sirupsen/logrus"
	// registers the "sqlite" driver; pure Go, so builds stay cgo-free
	_ "modernc.org/sqlite"
)

// t3TitleRefresh bounds how often T3's database is read; titles change when T3 names a new
// thread or the user renames one
const t3TitleRefresh = 30 * time.Second

// t3TitlesQuery lists the title of every thread for each Claude Code and Codex session T3 ran.
// Forks and handoffs can put one session in several threads, so rows come oldest first and the
// most recently updated thread wins
const t3TitlesQuery = `SELECT p.driver, json_extract(p.payload_json, '$.nativeThreadRef.nativeId'), t.title
FROM orchestration_v2_projection_provider_threads p
JOIN orchestration_v2_projection_threads t ON t.thread_id = p.thread_id
WHERE t.deleted_at IS NULL AND t.title <> '' AND p.driver IN ('claudeAgent', 'codex')
ORDER BY t.updated_at`

// t3SessionPrefixes maps T3's provider drivers to the proxy's session ID prefixes
var t3SessionPrefixes = map[string]string{"claudeAgent": "claude:", "codex": "codex:"}

// t3Titles caches T3 Code thread titles keyed by the proxy's session ID
type t3Titles struct {
	mu       sync.Mutex
	path     string
	loadedAt time.Time
	titles   map[string]string
}

// t3StateDBPath returns the configured T3 database path with a leading ~ expanded, or "" when
// T3 titles are off
func t3StateDBPath(cfg *config.Config) string {
	if cfg == nil || strings.TrimSpace(cfg.T3StateDB) == "" {
		return ""
	}
	path, errResolve := util.ResolveAuthDir(strings.TrimSpace(cfg.T3StateDB))
	if errResolve != nil {
		return ""
	}
	return path
}

// lookup returns the titles in the database at path, reading it at most every t3TitleRefresh; a
// failed read is logged and keeps the titles from the last good one
func (t *t3Titles) lookup(ctx context.Context, path string, now time.Time) map[string]string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if path != t.path {
		t.path, t.titles, t.loadedAt = path, nil, time.Time{}
	}
	if path == "" || now.Sub(t.loadedAt) < t3TitleRefresh {
		return t.titles
	}
	t.loadedAt = now
	titles, errRead := readT3Titles(ctx, path)
	if errRead != nil {
		log.WithError(errRead).Warn("cache usage: failed to read T3 thread titles")
		return t.titles
	}
	t.titles = titles
	return titles
}

// readT3Titles opens T3's database read-only for one query, so T3 upgrades and restarts never
// meet a held connection
func readT3Titles(ctx context.Context, path string) (titles map[string]string, err error) {
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&_pragma=query_only(1)"}).String()
	db, errOpen := sql.Open("sqlite", dsn)
	if errOpen != nil {
		return nil, fmt.Errorf("open %s: %w", path, errOpen)
	}
	defer func() {
		if errClose := db.Close(); errClose != nil && err == nil {
			err = fmt.Errorf("close %s: %w", path, errClose)
		}
	}()
	rows, errQuery := db.QueryContext(ctx, t3TitlesQuery)
	if errQuery != nil {
		return nil, fmt.Errorf("query %s: %w", path, errQuery)
	}
	defer func() {
		if errClose := rows.Close(); errClose != nil && err == nil {
			err = fmt.Errorf("close rows: %w", errClose)
		}
	}()

	titles = map[string]string{}
	for rows.Next() {
		var driver, title string
		var nativeID sql.NullString
		if errScan := rows.Scan(&driver, &nativeID, &title); errScan != nil {
			return nil, fmt.Errorf("scan %s: %w", path, errScan)
		}
		if id := strings.TrimSpace(nativeID.String); id != "" {
			titles[t3SessionPrefixes[driver]+id] = strings.TrimSpace(title)
		}
	}
	return titles, rows.Err()
}

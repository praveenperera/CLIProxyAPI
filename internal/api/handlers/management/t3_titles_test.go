package management

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestT3TitlesMapSessionsAndFollowRenames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "statev2.sqlite")
	db, errOpen := sql.Open("sqlite", path)
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	t.Cleanup(func() { _ = db.Close() })
	exec := func(query string, args ...any) {
		t.Helper()
		if _, errExec := db.Exec(query, args...); errExec != nil {
			t.Fatal(errExec)
		}
	}
	exec(`CREATE TABLE orchestration_v2_projection_threads (thread_id TEXT, title TEXT, deleted_at TEXT, updated_at TEXT)`)
	exec(`CREATE TABLE orchestration_v2_projection_provider_threads (thread_id TEXT, driver TEXT, payload_json TEXT)`)
	thread := func(id, title, deletedAt, updatedAt, driver, nativeID string) {
		exec(`INSERT INTO orchestration_v2_projection_threads VALUES (?, ?, ?, ?)`, id, title, sql.NullString{String: deletedAt, Valid: deletedAt != ""}, updatedAt)
		exec(`INSERT INTO orchestration_v2_projection_provider_threads VALUES (?, ?, json_object('nativeThreadRef', json_object('nativeId', ?)))`, id, driver, nativeID)
	}
	thread("t1", "Fix routing", "", "2026-10-28T10:00:00Z", "claudeAgent", "aaa")
	thread("t2", "Bazel CI", "", "2026-10-28T10:00:00Z", "codex", "bbb")
	// a fork puts session aaa in a second, newer thread, which names it
	thread("t3", "Fix routing (fork)", "", "2026-10-28T11:00:00Z", "claudeAgent", "aaa")
	thread("t4", "Deleted", "2026-10-28T11:00:00Z", "2026-10-28T11:00:00Z", "claudeAgent", "ccc")
	thread("t5", "Grok thread", "", "2026-10-28T11:00:00Z", "grok", "ddd")

	var titles t3Titles
	now := time.Unix(1_800_000_000, 0)
	got := titles.lookup(context.Background(), path, now)
	want := map[string]string{"claude:aaa": "Fix routing (fork)", "codex:bbb": "Bazel CI"}
	if len(got) != len(want) || got["claude:aaa"] != want["claude:aaa"] || got["codex:bbb"] != want["codex:bbb"] {
		t.Fatalf("titles = %v, want %v", got, want)
	}

	exec(`UPDATE orchestration_v2_projection_threads SET title = 'Renamed', updated_at = '2026-10-28T12:00:00Z' WHERE thread_id = 't2'`)
	if got := titles.lookup(context.Background(), path, now.Add(time.Second)); got["codex:bbb"] != "Bazel CI" {
		t.Fatalf("read again before the refresh interval: %v", got)
	}
	if got := titles.lookup(context.Background(), path, now.Add(t3TitleRefresh)); got["codex:bbb"] != "Renamed" {
		t.Fatalf("rename not picked up: %v", got)
	}

	// a database T3 has migrated away from keeps the last good titles
	exec(`DROP TABLE orchestration_v2_projection_threads`)
	if got := titles.lookup(context.Background(), path, now.Add(2*t3TitleRefresh)); got["codex:bbb"] != "Renamed" {
		t.Fatalf("failed read dropped titles: %v", got)
	}
}

// Command import_chat_history loads Claude Code transcripts and Codex rollouts from
// this machine into the chat history ClickHouse, for conversations from before the
// proxy recorded them. It writes through the same spool and uploader as the proxy,
// and re-running it is safe: ClickHouse collapses rows it already holds. See
// docs/chat-history.md
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chathistory"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/chathistory/transcript"
	log "github.com/sirupsen/logrus"
)

// importers pause while this much waits for upload, so a slow server does not
// fill the disk
const maxSpooled = 2 << 30

type job struct {
	source string
	path   string
}

type result struct {
	job   job
	calls []chathistory.Call
	stats transcript.Stats
	err   error
}

func main() {
	home, _ := os.UserHomeDir()
	claudeDir := flag.String("claude-dir", filepath.Join(home, ".claude", "projects"), "Claude Code projects directory; empty skips Claude Code")
	codexDir := flag.String("codex-dir", filepath.Join(home, ".codex", "sessions"), "Codex sessions directory; empty skips Codex")
	endpoint := flag.String("endpoint", "http://code.local:8123", "ClickHouse HTTP URL")
	database := flag.String("database", "chat", "ClickHouse database")
	user := flag.String("user", "chat", "ClickHouse user")
	passwordFile := flag.String("password-file", filepath.Join(home, ".config", "chat-history", "password"), "file holding the ClickHouse password")
	spoolDir := flag.String("spool-dir", filepath.Join(home, ".local", "share", "chat-history-import"), "spool directory; segments left by an interrupted run upload first")
	until := flag.String("until", "", "skip calls at or after this RFC 3339 time, such as when the proxy started recording")
	limit := flag.Int("limit", 0, "import at most this many files per source; 0 imports all")
	workers := flag.Int("workers", min(runtime.GOMAXPROCS(0), 4), "files parsed at once")
	dryRun := flag.Bool("dry-run", false, "parse and spool to a temporary directory, report the size, and upload nothing")
	flag.Parse()

	var cutoff time.Time
	if *until != "" {
		parsed, errParse := time.Parse(time.RFC3339, *until)
		if errParse != nil {
			log.Fatalf("-until: %v", errParse)
		}

		cutoff = parsed
	}

	opts := chathistory.Options{SpoolDir: *spoolDir, Database: *database, User: *user, FlushInterval: 10 * time.Second}
	if *dryRun {
		temp, errTemp := os.MkdirTemp("", "chat-history-dry-run-")
		if errTemp != nil {
			log.Fatal(errTemp)
		}
		defer func() { _ = os.RemoveAll(temp) }()

		opts.SpoolDir = temp
	} else {
		password, errPassword := os.ReadFile(*passwordFile)
		if errPassword != nil {
			log.Fatalf("read password: %v", errPassword)
		}

		opts.Endpoint = *endpoint
		opts.Password = strings.TrimSpace(string(password))
	}

	recorder, errRecorder := chathistory.NewRecorder(opts)
	if errRecorder != nil {
		log.Fatal(errRecorder)
	}

	jobs := listJobs("claude-code", *claudeDir, *limit)
	jobs = append(jobs, listJobs("codex", *codexDir, *limit)...)
	log.Infof("importing %d files", len(jobs))

	started := time.Now()
	var total transcript.Stats
	var skipped, failedFiles, failedCalls int
	done := 0
	for res := range parseAll(jobs, *workers) {
		done++
		if res.err != nil {
			failedFiles++
			log.Warnf("%s: %v", res.job.path, res.err)
			continue
		}

		total = total.Add(res.stats)
		for _, call := range res.calls {
			if !cutoff.IsZero() && !call.RequestedAt.Before(cutoff) {
				skipped++
				continue
			}

			for !*dryRun && recorder.Spooled() > maxSpooled {
				time.Sleep(time.Second)
			}

			if errImport := recorder.Import(call); errImport != nil {
				failedCalls++
				log.Warnf("%s: %v", res.job.path, errImport)
			}
		}

		if done%200 == 0 || done == len(jobs) {
			log.Infof("%d/%d files, %d calls, %s spooled, %s", done, len(jobs), total.Calls, size(recorder.Spooled()), time.Since(started).Round(time.Second))
		}
	}

	spooled := recorder.Spooled()
	if !*dryRun {
		// a large import leaves gigabytes for the uploader after the last file
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
		if errWait := recorder.WaitUploaded(ctx); errWait != nil {
			log.Warnf("upload did not finish: %v; the rest uploads from %s on the next run", errWait, *spoolDir)
		}
		cancel()
	}

	recorder.Close()
	fmt.Printf("files %d (failed %d), calls %d (skipped by -until %d, failed %d), chained %d, unresolved messages %d, bad lines %d, spooled %s in %s\n",
		len(jobs), failedFiles, total.Calls, skipped, failedCalls, total.Chained, total.Unresolved, total.BadLines, size(spooled), time.Since(started).Round(time.Second))
	if failedFiles > 0 || failedCalls > 0 {
		os.Exit(1)
	}
}

// listJobs finds a source's session files, oldest name first. Claude Code keeps
// main sessions and subagents as project/*.jsonl and project/*/subagents/*.jsonl;
// Codex keeps rollouts under year/month/day
func listJobs(source, dir string, limit int) []job {
	if dir == "" {
		return nil
	}

	var paths []string
	errWalk := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, errEntry error) error {
		if errEntry != nil {
			if errors.Is(errEntry, fs.ErrNotExist) && path == dir {
				return filepath.SkipDir
			}

			return errEntry
		}

		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			return nil
		}

		if source == "codex" && !strings.HasPrefix(name, "rollout-") {
			return nil
		}

		paths = append(paths, path)
		return nil
	})

	if errWalk != nil {
		log.Fatalf("list %s: %v", dir, errWalk)
	}

	sort.Strings(paths)
	if limit > 0 && len(paths) > limit {
		paths = paths[:limit]
	}

	jobs := make([]job, len(paths))
	for i, path := range paths {
		jobs[i] = job{source: source, path: path}
	}

	return jobs
}

// parseAll parses files on several workers. Import runs on one goroutine, since
// the recorder's dedup state is not shared across goroutines
func parseAll(jobs []job, workers int) <-chan result {
	queue := make(chan job)
	results := make(chan result, workers)

	var wg sync.WaitGroup
	for range max(workers, 1) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for next := range queue {
				results <- parse(next)
			}
		}()
	}

	go func() {
		for _, next := range jobs {
			queue <- next
		}

		close(queue)
		wg.Wait()
		close(results)
	}()

	return results
}

func parse(next job) result {
	var calls []chathistory.Call
	var stats transcript.Stats
	var errParse error
	switch next.source {
	case "claude-code":
		calls, stats, errParse = transcript.ClaudeCode(next.path)
	case "codex":
		calls, stats, errParse = transcript.Codex(next.path)
	}

	return result{job: next, calls: calls, stats: stats, err: errParse}
}

func size(bytes int64) string {
	return fmt.Sprintf("%.1f MB", float64(bytes)/1e6)
}

package auth

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/affinity"
	domain "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quota"
	log "github.com/sirupsen/logrus"
)

// QuotaHistoryRetention is how long recorded quota points are kept in memory and on disk
const QuotaHistoryRetention = 30 * 24 * time.Hour

const (
	// quotaHistoryHeartbeat stores an unchanged value often enough to draw a continuous line
	quotaHistoryHeartbeat = 15 * time.Minute
	// quotaHistoryMinDelta is 0.1 percentage points of remaining fraction
	quotaHistoryMinDelta = 0.001
	// relative reset headers drift by a second or two between receipts while a real
	// window rollover moves the reset by at least the window length
	quotaHistoryResetTolerance = time.Minute
	quotaHistoryCompactEvery   = 6 * time.Hour
	// 30 days at the heartbeat is 2880 points, leaving room for frequent value changes
	quotaHistoryMaxPoints = 8192
	quotaHistoryQueueSize = 256
	// records are about 150 bytes; a longer line is corrupt
	quotaHistoryMaxLineBytes = 64 * 1024
)

// QuotaHistory is the recorded remaining-quota time series for live credentials
type QuotaHistory struct {
	// Enabled reports whether the quota-aware chat affinity profile is running
	Enabled bool
	// Retention is the maximum age of recorded points
	Retention time.Duration
	// Credentials is sorted by auth ID and omits credentials without points in range
	Credentials []QuotaHistoryCredential
}

// QuotaHistoryCredential groups recorded windows for one current credential
type QuotaHistoryCredential struct {
	AuthID    string
	AuthIndex string
	Provider  string
	// QuotaWeight is the credential's quota size relative to its provider's other credentials,
	// from the optional quota_weight field in its auth file; it defaults to 1
	QuotaWeight float64
	// Email is the account email from the auth file, or empty when it has none
	Email string
	// PlanType is the credential's plan, such as Codex's prolite, from its auth file or Codex id token,
	// or empty when unknown
	PlanType string
	// Windows is sorted by window ID
	Windows []QuotaHistoryWindow
}

// QuotaHistoryWindow is one provider quota window, such as seven_day or primary
type QuotaHistoryWindow struct {
	ID string
	// Period is the rolling window length, or zero when unknown
	Period time.Duration
	// Points is sorted by observation time
	Points []QuotaHistoryPoint
}

// QuotaHistoryPoint is one recorded observation of a fractional remaining value
type QuotaHistoryPoint struct {
	At time.Time
	// Remaining is a fraction between 0 and 1
	Remaining float64
	// ResetAt is zero when the provider did not report a reset
	ResetAt time.Time
}

// quotaHistoryRecord is one JSONL line; times are unix seconds
type quotaHistoryRecord struct {
	AuthID    string  `json:"auth_id"`
	Provider  string  `json:"provider"`
	Window    string  `json:"window"`
	Period    int64   `json:"period_seconds"`
	T         int64   `json:"t"`
	Remaining float64 `json:"remaining"`
	ResetAt   int64   `json:"reset_at,omitempty"`
}

type quotaHistoryKey struct{ authID, window string }

type quotaHistoryPoint struct {
	t, reset  int64
	remaining float64
}

type quotaHistorySeries struct {
	provider string
	period   int64
	points   []quotaHistoryPoint
}

// quotaHistoryWrite is either an append batch or a full rewrite of the log
type quotaHistoryWrite struct {
	records []quotaHistoryRecord
	rewrite bool
}

// quotaHistory records downsampled quota points in memory and persists them best effort
// Recording only touches memory under mu and hands records to a single writer goroutine
// through a non-blocking send, so dispatch never waits on disk; a dropped or failed append
// is repaired by the next compaction, which rewrites the log from memory
type quotaHistory struct {
	path string
	now  func() time.Time

	mu          sync.Mutex
	series      map[quotaHistoryKey]*quotaHistorySeries
	closed      bool
	lastCompact time.Time
	// keepLog blocks rewrites after a failed load so a partial memory view never replaces the file
	keepLog bool
	writes  chan quotaHistoryWrite
	done    chan struct{}
}

// openQuotaHistory loads retained points and starts the writer
// The directory must already exist and be owned by the affinity store
func openQuotaHistory(dir string, now func() time.Time) *quotaHistory {
	h := &quotaHistory{
		path:   filepath.Join(dir, affinity.QuotaHistoryFileName),
		now:    now,
		series: make(map[quotaHistoryKey]*quotaHistorySeries),
		writes: make(chan quotaHistoryWrite, quotaHistoryQueueSize),
		done:   make(chan struct{}),
	}
	loaded, errLoad := h.load()
	if errLoad != nil {
		log.WithError(errLoad).Warn("quota history: failed to load recorded points; keeping the existing file")
		h.keepLog = true
	}
	go h.run()
	// a failed load keeps the unreadable file instead of replacing it with an empty log
	if errLoad == nil && loaded {
		h.mu.Lock()
		h.compactLocked(h.now())
		h.mu.Unlock()
	}
	return h
}

// load reads retained records and reports whether a log file existed
func (h *quotaHistory) load() (bool, error) {
	file, err := os.Open(h.path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() {
		if errClose := file.Close(); errClose != nil {
			log.WithError(errClose).Warn("quota history: failed to close log after load")
		}
	}()

	cutoff := h.now().Add(-QuotaHistoryRetention).Unix()
	reader := bufio.NewReader(file)
	skipped := 0
	for {
		line, errRead := readQuotaHistoryLine(reader)
		if len(line) > 0 {
			var rec quotaHistoryRecord
			if errDecode := json.Unmarshal(line, &rec); errDecode != nil || !rec.valid() {
				// a crash can leave one truncated final line; compaction drops it
				skipped++
			} else if rec.T >= cutoff {
				s := h.seriesFor(quotaHistoryKey{rec.AuthID, rec.Window})
				s.provider = rec.Provider
				if rec.Period > 0 {
					s.period = rec.Period
				}
				s.points = append(s.points, quotaHistoryPoint{t: rec.T, reset: rec.ResetAt, remaining: rec.Remaining})
			}
		}
		if errors.Is(errRead, io.EOF) {
			break
		}
		if errRead != nil {
			return true, errRead
		}
	}
	if skipped > 0 {
		log.WithField("lines", skipped).Warn("quota history: skipped invalid records")
	}

	for _, s := range h.series {
		sort.SliceStable(s.points, func(i, j int) bool { return s.points[i].t < s.points[j].t })
		points := s.points[:0]
		for _, p := range s.points {
			if n := len(points); n > 0 && points[n-1].t == p.t {
				points[n-1] = p
				continue
			}
			points = append(points, p)
		}
		s.points = capQuotaHistoryPoints(points)
	}
	return true, nil
}

// readQuotaHistoryLine returns one line without its newline, discarding overlong lines
func readQuotaHistoryLine(reader *bufio.Reader) ([]byte, error) {
	var line []byte
	overlong := false
	for {
		chunk, isPrefix, err := reader.ReadLine()
		if !overlong {
			line = append(line, chunk...)
			if len(line) > quotaHistoryMaxLineBytes {
				// keep reading to the newline but return bytes that fail JSON decoding
				overlong = true
				line = []byte("!")
			}
		}
		if err != nil || !isPrefix {
			return line, err
		}
	}
}

func (r quotaHistoryRecord) valid() bool {
	return r.AuthID != "" && r.Window != "" && r.T > 0 && r.Period >= 0 && r.ResetAt >= 0 &&
		!math.IsNaN(r.Remaining) && r.Remaining >= 0 && r.Remaining <= 1
}

func (h *quotaHistory) seriesFor(key quotaHistoryKey) *quotaHistorySeries {
	s := h.series[key]
	if s == nil {
		s = &quotaHistorySeries{}
		h.series[key] = s
	}
	return s
}

// weeklyReset returns the soonest future reset recorded for a credential's weekly windows and
// the remaining fraction at that point; unlike live observations, the log survives restarts
func (h *quotaHistory) weeklyReset(authID string, now time.Time) (time.Time, float64, bool) {
	if h == nil {
		return time.Time{}, 0, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var best quotaHistoryPoint
	for key, s := range h.series {
		if key.authID != authID || s.period != int64(7*24*time.Hour/time.Second) || len(s.points) == 0 {
			continue
		}
		if latest := s.points[len(s.points)-1]; latest.reset > now.Unix() && (best.reset == 0 || latest.reset < best.reset) {
			best = latest
		}
	}
	if best.reset == 0 {
		return time.Time{}, 0, false
	}
	return time.Unix(best.reset, 0), best.remaining, true
}

func capQuotaHistoryPoints(points []quotaHistoryPoint) []quotaHistoryPoint {
	if len(points) > quotaHistoryMaxPoints {
		return points[len(points)-quotaHistoryMaxPoints:]
	}
	return points
}

// record stores accepted fractional windows that pass downsampling and queues them for append
func (h *quotaHistory) record(authID, provider string, observedAt time.Time, windows []domain.Window) {
	if h == nil || len(windows) == 0 {
		return
	}
	t := observedAt.Unix()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	cutoff := h.now().Add(-QuotaHistoryRetention).Unix()
	var records []quotaHistoryRecord
	for _, w := range windows {
		remaining := w.Remaining()
		value, known := remaining.Value()
		if !known || remaining.IsAmount() {
			continue
		}
		var reset int64
		if at, ok := w.ResetAt(); ok {
			reset = at.Unix()
		}
		s := h.seriesFor(quotaHistoryKey{authID, w.ID()})
		s.provider = provider
		if period := int64(w.Period().Duration() / time.Second); period > 0 {
			s.period = period
		}
		if n := len(s.points); n > 0 {
			last := s.points[n-1]
			// a concurrent older receipt can arrive after a newer one; keep the series monotonic
			if t <= last.t {
				continue
			}
			if math.Abs(value-last.remaining) < quotaHistoryMinDelta && !quotaResetChanged(last.reset, reset) && time.Duration(t-last.t)*time.Second < quotaHistoryHeartbeat {
				continue
			}
		}
		points := s.points
		for len(points) > 0 && points[0].t < cutoff {
			points = points[1:]
		}
		s.points = capQuotaHistoryPoints(append(points, quotaHistoryPoint{t: t, reset: reset, remaining: value}))
		records = append(records, quotaHistoryRecord{AuthID: authID, Provider: provider, Window: w.ID(), Period: s.period, T: t, Remaining: value, ResetAt: reset})
	}
	if len(records) > 0 {
		h.enqueueLocked(quotaHistoryWrite{records: records})
	}
}

func quotaResetChanged(previous, current int64) bool {
	if (previous == 0) != (current == 0) {
		return true
	}
	delta := time.Duration(current-previous) * time.Second
	return delta > quotaHistoryResetTolerance || delta < -quotaHistoryResetTolerance
}

// enqueueLocked sends under mu so the writer sees appends and rewrites in memory order
func (h *quotaHistory) enqueueLocked(w quotaHistoryWrite) bool {
	select {
	case h.writes <- w:
		return true
	default:
		log.Debug("quota history: write queue full; the next compaction persists the dropped points")
		return false
	}
}

// maybeCompact prunes expired points and rewrites the log at most once per compaction interval
func (h *quotaHistory) maybeCompact() {
	if h == nil {
		return
	}
	now := h.now()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || h.keepLog || now.Sub(h.lastCompact) < quotaHistoryCompactEvery {
		return
	}
	h.compactLocked(now)
}

func (h *quotaHistory) compactLocked(now time.Time) {
	cutoff := now.Add(-QuotaHistoryRetention).Unix()
	var records []quotaHistoryRecord
	for key, s := range h.series {
		points := s.points
		for len(points) > 0 && points[0].t < cutoff {
			points = points[1:]
		}
		if len(points) == 0 {
			delete(h.series, key)
			continue
		}
		s.points = points
		for _, p := range points {
			records = append(records, quotaHistoryRecord{AuthID: key.authID, Provider: s.provider, Window: key.window, Period: s.period, T: p.t, Remaining: p.remaining, ResetAt: p.reset})
		}
	}
	// a full queue leaves lastCompact unchanged so the next poll retries
	if h.enqueueLocked(quotaHistoryWrite{records: records, rewrite: true}) {
		h.lastCompact = now
	}
}

// run owns the log file; it appends batches and replaces the file on rewrite
func (h *quotaHistory) run() {
	defer close(h.done)
	var file *os.File
	for w := range h.writes {
		if w.rewrite {
			file = h.rewrite(file, w.records)
			continue
		}
		file = h.append(file, w.records)
	}
	if file != nil {
		if errClose := file.Close(); errClose != nil {
			log.WithError(errClose).Warn("quota history: failed to close log")
		}
	}
}

func encodeQuotaHistory(records []quotaHistoryRecord) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	for _, rec := range records {
		if errEncode := encoder.Encode(rec); errEncode != nil {
			return nil, errEncode
		}
	}
	return buf.Bytes(), nil
}

func (h *quotaHistory) append(file *os.File, records []quotaHistoryRecord) *os.File {
	data, errEncode := encodeQuotaHistory(records)
	if errEncode != nil {
		log.WithError(errEncode).Warn("quota history: failed to encode records")
		return file
	}
	if file == nil {
		var errOpen error
		file, errOpen = os.OpenFile(h.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if errOpen != nil {
			log.WithError(errOpen).Warn("quota history: failed to open log for append")
			return nil
		}
	}
	if _, errWrite := file.Write(data); errWrite != nil {
		log.WithError(errWrite).Warn("quota history: failed to append records")
	}
	return file
}

// rewrite replaces the log through a synced temp file so a crash keeps the old or new log
func (h *quotaHistory) rewrite(file *os.File, records []quotaHistoryRecord) *os.File {
	data, errEncode := encodeQuotaHistory(records)
	if errEncode != nil {
		log.WithError(errEncode).Warn("quota history: failed to encode compacted log")
		return file
	}
	tmpPath := filepath.Join(filepath.Dir(h.path), affinity.QuotaHistoryTempFileName)
	if errWrite := writeSyncedFile(tmpPath, data); errWrite != nil {
		log.WithError(errWrite).Warn("quota history: failed to write compacted log")
		if errRemove := os.Remove(tmpPath); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
			log.WithError(errRemove).Warn("quota history: failed to remove compaction temp file")
		}
		return file
	}
	// close the append handle first: Windows cannot rename over a file that is still open,
	// and either way the next append reopens the log lazily
	if file != nil {
		if errClose := file.Close(); errClose != nil {
			log.WithError(errClose).Warn("quota history: failed to close log before replacing it")
		}
	}
	if errRename := os.Rename(tmpPath, h.path); errRename != nil {
		log.WithError(errRename).Warn("quota history: failed to replace log")
	}
	return nil
}

func writeSyncedFile(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, errWrite := file.Write(data)
	var errSync error
	if errWrite == nil {
		errSync = file.Sync()
	}
	return errors.Join(errWrite, errSync, file.Close())
}

// close drains queued writes and stops the writer; later records are ignored
func (h *quotaHistory) close() {
	if h == nil {
		return
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	close(h.writes)
	h.mu.Unlock()
	<-h.done
}

// query copies points in [since, until] grouped by auth ID
func (h *quotaHistory) query(since, until time.Time) map[string]QuotaHistoryCredential {
	from, to := since.Unix(), until.Unix()
	out := make(map[string]QuotaHistoryCredential)
	h.mu.Lock()
	defer h.mu.Unlock()
	for key, s := range h.series {
		start := sort.Search(len(s.points), func(i int) bool { return s.points[i].t >= from })
		end := sort.Search(len(s.points), func(i int) bool { return s.points[i].t > to })
		if start >= end {
			continue
		}
		window := QuotaHistoryWindow{ID: key.window, Period: time.Duration(s.period) * time.Second, Points: make([]QuotaHistoryPoint, 0, end-start)}
		for _, p := range s.points[start:end] {
			point := QuotaHistoryPoint{At: time.Unix(p.t, 0).UTC(), Remaining: p.remaining}
			if p.reset > 0 {
				point.ResetAt = time.Unix(p.reset, 0).UTC()
			}
			window.Points = append(window.Points, point)
		}
		credential := out[key.authID]
		credential.AuthID = key.authID
		credential.Provider = s.provider
		credential.Windows = append(credential.Windows, window)
		out[key.authID] = credential
	}
	return out
}

// QuotaHistory returns recorded remaining-quota points observed in [since, until]
// Credentials that no longer exist are omitted; the result is empty when the profile is off
func (m *Manager) QuotaHistory(since, until time.Time) QuotaHistory {
	result := QuotaHistory{Retention: QuotaHistoryRetention, Credentials: []QuotaHistoryCredential{}}
	if m == nil {
		return result
	}
	m.affinityMu.Lock()
	r := m.affinityRuntime
	m.affinityMu.Unlock()
	if r == nil || r.history == nil {
		return result
	}
	result.Enabled = true
	// the history lock is released before GetByID takes the manager lock
	recorded := r.history.query(since, until)
	for id, credential := range recorded {
		a, ok := m.GetByID(id)
		if !ok {
			continue
		}
		credential.AuthIndex = a.EnsureIndex()
		credential.QuotaWeight = quotaWeight(a.Metadata)
		// read the metadata directly: AccountInfo returns the key itself for API-key credentials
		credential.Email = metadataString(a.Metadata, "email")
		// the file synthesizer derives Codex plans from the id token when the file has no plan_type
		credential.PlanType = strings.TrimSpace(a.Attributes["plan_type"])
		if credential.PlanType == "" {
			credential.PlanType = metadataString(a.Metadata, "plan_type")
		}
		sort.Slice(credential.Windows, func(i, j int) bool { return credential.Windows[i].ID < credential.Windows[j].ID })
		result.Credentials = append(result.Credentials, credential)
	}
	sort.Slice(result.Credentials, func(i, j int) bool { return result.Credentials[i].AuthID < result.Credentials[j].AuthID })
	return result
}

func metadataString(metadata map[string]any, key string) string {
	value, _ := metadata[key].(string)
	return strings.TrimSpace(value)
}

// quotaWeight reads a positive quota_weight from auth metadata; plans of one provider can differ in
// size, such as Claude Max 20x and 5x, so pooled views weight each credential's remaining share
func quotaWeight(metadata map[string]any) float64 {
	var value float64
	switch typed := metadata["quota_weight"].(type) {
	case float64:
		value = typed
	case json.Number:
		// management field patches store numbers undecoded until the file is reloaded
		value, _ = typed.Float64()
	}
	if value > 0 && value <= 1000 {
		return value
	}
	return 1
}

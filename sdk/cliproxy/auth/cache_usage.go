package auth

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/affinity"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

// CacheUsageRetention is how long hourly prompt cache totals are kept
const CacheUsageRetention = 30 * 24 * time.Hour

// cacheUsageSessionIdle drops sessions that have not sent a request for this long
const cacheUsageSessionIdle = 7 * 24 * time.Hour

// cacheUsageMaxSessions caps tracked sessions; the least recently active are dropped first
const cacheUsageMaxSessions = 2000

// cacheUsageMaxLabels caps session labels; a label is read before its request runs, so a
// request that fails leaves a label without a session until cacheUsageLabelPending passes
const cacheUsageMaxLabels = 2 * cacheUsageMaxSessions

// cacheUsageMaxUnpricedModels caps the unpriced model names that get their own row, since the
// upstream names them; later names share cacheUsageOtherModel until a restart rebuilds the set
// from retained hours
const cacheUsageMaxUnpricedModels = 64

// cacheUsageModelMaxBytes bounds a model name kept in the report
const cacheUsageModelMaxBytes = 128

// cacheUsageOtherModel holds unpriced models past cacheUsageMaxUnpricedModels
const cacheUsageOtherModel = "other"

// cacheUsageSaveInterval limits snapshot rewrites; a final save runs when the profile closes
const cacheUsageSaveInterval = time.Minute

// cacheUsagePruneInterval limits retention sweeps on the record path
const cacheUsagePruneInterval = 10 * time.Minute

// cacheUsageLabelPending keeps a session label whose session has no recorded request yet,
// which covers the first request's run time; labels of requests that never finish then go
const cacheUsageLabelPending = time.Hour

// cacheUsageSnapshotVersion 2 added per-model hours and API-equivalent cost; totals loaded from
// an older snapshot count as unpriced
const cacheUsageSnapshotVersion = 2

// CacheUsageTotals counts input tokens by prompt cache outcome; the input buckets are mutually
// exclusive, so InputTokens = CacheReadTokens + CacheWriteTokens + UncachedTokens
type CacheUsageTotals struct {
	Requests         int64 `json:"requests"`
	InputTokens      int64 `json:"input_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	// CacheWrite1hTokens is the part of CacheWriteTokens written to the 1-hour cache
	CacheWrite1hTokens int64 `json:"cache_write_1h_tokens,omitempty"`
	UncachedTokens     int64 `json:"uncached_tokens"`
	OutputTokens       int64 `json:"output_tokens"`
	// CostUSD is the requests' cost at API list prices and NoCacheCostUSD the same requests
	// with every input token at the base input rate; both leave out UnpricedRequests, whose
	// model has no known price or which were recorded before cost tracking
	CostUSD          float64 `json:"cost_usd,omitempty"`
	NoCacheCostUSD   float64 `json:"no_cache_cost_usd,omitempty"`
	UnpricedRequests int64   `json:"unpriced_requests,omitempty"`
}

// HitRatio returns the share of input tokens read from the prompt cache; false when there is no input
func (t CacheUsageTotals) HitRatio() (float64, bool) {
	if t.InputTokens <= 0 {
		return 0, false
	}
	return float64(t.CacheReadTokens) / float64(t.InputTokens), true
}

func (t *CacheUsageTotals) add(other CacheUsageTotals) {
	t.Requests += other.Requests
	t.InputTokens += other.InputTokens
	t.CacheReadTokens += other.CacheReadTokens
	t.CacheWriteTokens += other.CacheWriteTokens
	t.CacheWrite1hTokens += other.CacheWrite1hTokens
	t.UncachedTokens += other.UncachedTokens
	t.OutputTokens += other.OutputTokens
	t.CostUSD += other.CostUSD
	t.NoCacheCostUSD += other.NoCacheCostUSD
	t.UnpricedRequests += other.UnpricedRequests
}

// markUnpriced counts every request as unpriced, for totals kept before cost tracking
func (t *CacheUsageTotals) markUnpriced() {
	t.UnpricedRequests = t.Requests
}

// CacheUsageQuery filters a cache usage report; empty AuthID and SessionID match everything
type CacheUsageQuery struct {
	Since, Until time.Time
	AuthID       string
	// SessionID filters sessions to an exact recorded ID, or to "<provider>:<id>" for a bare ID
	// that no session has exactly
	SessionID string
}

// CacheUsageReport is prompt cache usage for credentials and client sessions
type CacheUsageReport struct {
	// Since and Until are the hour-aligned bounds actually reported: totals are kept per hour,
	// so a query covers every hour it overlaps, ending at the end of Until's hour
	Since, Until time.Time
	// Persisted reports whether totals survive restarts, which needs the quota-aware chat affinity profile
	Persisted   bool
	Retention   time.Duration
	Totals      CacheUsageTotals
	Credentials []CacheUsageCredential
	// Models splits the totals by the model that served each request, busiest first
	Models   []CacheUsageModel
	Sessions []CacheUsageSession
}

// CacheUsageModel is one served model's usage in the queried range; Model is empty for
// requests recorded before usage was kept per model
type CacheUsageModel struct {
	Model    string
	Provider string
	Totals   CacheUsageTotals
}

// CacheUsageCredential is one credential's prompt cache usage in the queried range
type CacheUsageCredential struct {
	AuthID    string
	AuthIndex string
	Provider  string
	Email     string
	PlanType  string
	Totals    CacheUsageTotals
	// Hours holds hourly totals in ascending order, omitting hours without requests
	Hours []CacheUsageHour
}

// CacheUsageHour is the totals for requests started in the hour beginning at Start
type CacheUsageHour struct {
	Start  time.Time
	Totals CacheUsageTotals
}

// CacheUsageSession is one client session's prompt cache usage since it was first seen
type CacheUsageSession struct {
	SessionID       string
	ParentSessionID string
	AuthID          string
	Provider        string
	Model           string
	// Title is the start of the session's first user prompt and Project the last element of
	// the client's working directory; either is empty when the request did not carry it
	Title     string
	Project   string
	FirstSeen time.Time
	LastSeen  time.Time
	Totals    CacheUsageTotals
	// LastRequest is the latest request alone, showing whether the cache is warm right now
	LastRequest CacheUsageTotals
}

type cacheUsageHourKey struct {
	AuthID   string `json:"auth_id"`
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
	Hour     int64  `json:"t"`
}

type cacheUsageSession struct {
	ID       string `json:"id"`
	ParentID string `json:"parent_id,omitempty"`
	AuthID   string `json:"auth_id"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// First and Last are request start times in unix milliseconds, so requests started within
	// one second still order correctly
	First       int64            `json:"first_ms"`
	Last        int64            `json:"last_ms"`
	Totals      CacheUsageTotals `json:"totals"`
	LastRequest CacheUsageTotals `json:"last_request"`
}

// cacheUsageLabel names a session; it is kept apart from the session because it is read from
// the request before the first usage record creates the session
type cacheUsageLabel struct {
	Title   string `json:"title,omitempty"`
	Project string `json:"project,omitempty"`
	// At is when the label was read, in unix milliseconds
	At int64 `json:"at_ms"`
}

type cacheUsageHourRecord struct {
	cacheUsageHourKey
	Totals CacheUsageTotals `json:"totals"`
}

type cacheUsageSnapshot struct {
	Version  int                         `json:"version"`
	Hours    []cacheUsageHourRecord      `json:"hours"`
	Sessions []*cacheUsageSession        `json:"sessions"`
	Labels   map[string]*cacheUsageLabel `json:"labels,omitempty"`
}

// cacheUsage aggregates prompt cache outcomes from usage records by credential hour and session
type cacheUsage struct {
	now func() time.Time
	// saveMu serializes snapshot writes, which share one temp file
	saveMu sync.Mutex

	mu       sync.Mutex
	hours    map[cacheUsageHourKey]*CacheUsageTotals
	sessions map[string]*cacheUsageSession
	labels   map[string]*cacheUsageLabel
	// unpricedModels holds the unpriced model names that have their own row
	unpricedModels map[string]struct{}
	path           string
	lastSave       time.Time
	lastPrune      time.Time
	saving         bool
	dirty          bool
}

func newCacheUsage(now func() time.Time) *cacheUsage {
	return &cacheUsage{now: now, hours: map[cacheUsageHourKey]*CacheUsageTotals{}, sessions: map[string]*cacheUsageSession{}, labels: map[string]*cacheUsageLabel{}, unpricedModels: map[string]struct{}{}}
}

// persistTo loads the snapshot in dir and saves later changes there
func (c *cacheUsage) persistTo(dir string) {
	path := filepath.Join(dir, affinity.CacheUsageFileName)
	var snapshot cacheUsageSnapshot
	data, errRead := os.ReadFile(path)
	if errRead == nil {
		if errDecode := json.Unmarshal(data, &snapshot); errDecode != nil {
			log.WithError(errDecode).Warn("cache usage: ignoring unreadable snapshot")
			snapshot = cacheUsageSnapshot{}
		}
	} else if !os.IsNotExist(errRead) {
		log.WithError(errRead).Warn("cache usage: failed to read snapshot")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.path = path
	legacy := snapshot.Version < cacheUsageSnapshotVersion
	// readings taken before the snapshot loaded are newer and add to it
	for _, h := range snapshot.Hours {
		if legacy {
			h.Totals.markUnpriced()
		}
		key := h.cacheUsageHourKey
		// a snapshot saved before the unpriced row cap can hold more names than it allows
		if _, priced := cacheUsagePrices[key.Model]; !priced && key.Model != cacheUsageOtherModel {
			key.Model = c.unpricedModelLocked(key.Model)
		}
		totals := c.hours[key]
		if totals == nil {
			totals = &CacheUsageTotals{}
			c.hours[key] = totals
		}
		totals.add(h.Totals)
	}
	for _, s := range snapshot.Sessions {
		if s == nil || s.ID == "" {
			continue
		}
		if legacy {
			s.Totals.markUnpriced()
			s.LastRequest.markUnpriced()
		}
		if current := c.sessions[s.ID]; current != nil {
			current.merge(s)
		} else {
			c.sessions[s.ID] = s
		}
	}
	for id, label := range snapshot.Labels {
		// a label read since startup is the session's first and wins
		if label != nil && c.labels[id] == nil {
			c.labels[id] = label
		}
	}
	c.pruneLocked(c.now())
}

// hasLabel reports whether the session is already named, so callers skip reading the payload
func (c *cacheUsage) hasLabel(sessionID string) bool {
	if c == nil {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.labels[sessionID] != nil
}

// noteLabel names a session once; later requests carry the same first prompt, or a summary of
// it after compaction, so the first reading is kept
func (c *cacheUsage) noteLabel(sessionID, title, project string) {
	if c == nil || sessionID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.labels[sessionID] != nil {
		return
	}
	if len(c.labels) >= cacheUsageMaxLabels {
		c.pruneLabelsLocked(c.now())
		// a flood of failing requests leaves new sessions untitled rather than growing the map
		if len(c.labels) >= cacheUsageMaxLabels {
			return
		}
	}
	c.labels[sessionID] = &cacheUsageLabel{Title: title, Project: project, At: c.now().UnixMilli()}
	c.dirty = true
}

// unpricedModelLocked returns the row name for an unpriced model, shortened to
// cacheUsageModelMaxBytes and folded into cacheUsageOtherModel once the cap is reached
func (c *cacheUsage) unpricedModelLocked(model string) string {
	if len(model) > cacheUsageModelMaxBytes {
		model = strings.ToValidUTF8(model[:cacheUsageModelMaxBytes], "")
	}
	if _, ok := c.unpricedModels[model]; ok || model == "" {
		return model
	}
	if len(c.unpricedModels) >= cacheUsageMaxUnpricedModels {
		return cacheUsageOtherModel
	}
	c.unpricedModels[model] = struct{}{}
	return model
}

// merge folds another view of the same session into s
func (s *cacheUsageSession) merge(other *cacheUsageSession) {
	s.Totals.add(other.Totals)
	s.First = min(s.First, other.First)
	if other.Last > s.Last {
		s.ParentID, s.AuthID, s.Provider, s.Model = other.ParentID, other.AuthID, other.Provider, other.Model
		s.Last, s.LastRequest = other.Last, other.LastRequest
	}
}

// record adds one finished request; failed requests and records without complete canonical
// accounting are skipped, since provider-native token fields disagree on what input includes
func (c *cacheUsage) record(r usage.Record) {
	b := r.Detail.TokenBreakdown
	if c == nil || r.Failed || r.AuthID == "" || !b.Valid() || b.Quality != usage.TokenAccountingQualityComplete || b.Input.TotalTokens <= 0 {
		return
	}
	at := r.RequestedAt
	if at.IsZero() {
		at = c.now()
	}
	request := CacheUsageTotals{
		Requests:           1,
		InputTokens:        b.Input.TotalTokens,
		CacheReadTokens:    b.Input.CacheReadTokens,
		CacheWriteTokens:   b.Input.CacheWriteTokens,
		CacheWrite1hTokens: min(max(r.Detail.CacheWrite1hTokens, 0), b.Input.CacheWriteTokens),
		UncachedTokens:     b.Input.UncachedTokens,
		OutputTokens:       b.Output.TotalTokens,
	}
	// price the model that actually served the request, which differs from the requested one
	// after an alias or upstream substitution
	model := strings.TrimSpace(r.ResponseModel)
	if model == "" {
		model = strings.TrimSpace(r.Model)
	}
	tier := r.ResponseServiceTier
	if tier == "" {
		tier = r.Detail.ResponseServiceTier
	}
	if cost, noCache, ok := cacheUsageCost(model, tier, request); ok {
		request.CostUSD, request.NoCacheCostUSD = cost, noCache
		// dated snapshots, cloud IDs, and suffixed aliases of one model share its row
		model = cacheUsagePriceKey(model)
	} else {
		request.UnpricedRequests = 1
	}

	c.mu.Lock()
	if request.UnpricedRequests > 0 {
		model = c.unpricedModelLocked(model)
	}
	key := cacheUsageHourKey{AuthID: r.AuthID, Provider: r.Provider, Model: model, Hour: at.Truncate(time.Hour).Unix()}
	totals := c.hours[key]
	if totals == nil {
		totals = &CacheUsageTotals{}
		c.hours[key] = totals
	}
	totals.add(request)
	if id := strings.TrimSpace(r.SessionID); id != "" {
		s := c.sessions[id]
		if s == nil {
			s = &cacheUsageSession{ID: id, First: at.UnixMilli(), Last: at.UnixMilli()}
			c.sessions[id] = s
			c.capSessionsLocked()
		}
		s.First = min(s.First, at.UnixMilli())
		s.Totals.add(request)
		// requests can finish out of order; the latest started one names the session's credential,
		// which changes on failover, and its cache outcome
		if at.UnixMilli() >= s.Last {
			s.ParentID, s.AuthID, s.Provider, s.Model = strings.TrimSpace(r.ParentSessionID), r.AuthID, r.Provider, r.Model
			s.Last, s.LastRequest = at.UnixMilli(), request
		}
	}
	if now := c.now(); now.Sub(c.lastPrune) >= cacheUsagePruneInterval {
		c.pruneLocked(now)
		c.lastPrune = now
	}
	c.dirty = true
	save := c.path != "" && !c.saving && c.now().Sub(c.lastSave) >= cacheUsageSaveInterval
	if save {
		c.saving = true
	}
	c.mu.Unlock()

	if save {
		go c.save()
	}
}

// save writes the snapshot when it changed since the last write
func (c *cacheUsage) save() {
	c.saveMu.Lock()
	defer c.saveMu.Unlock()
	c.saveLocked(false)
}

// close writes the final snapshot and stops persisting in one step, before the state directory
// is released; records arriving later stay in memory only
func (c *cacheUsage) close() {
	if c == nil {
		return
	}
	c.saveMu.Lock()
	defer c.saveMu.Unlock()
	c.saveLocked(true)
}

// saveLocked runs with saveMu held, since writes share one temp file
func (c *cacheUsage) saveLocked(final bool) {
	c.mu.Lock()
	path := c.path
	if final {
		c.path = ""
	}
	if path == "" || !c.dirty {
		c.saving = false
		c.mu.Unlock()
		return
	}
	now := c.now()
	c.pruneLocked(now)
	snapshot := cacheUsageSnapshot{Version: cacheUsageSnapshotVersion}
	for key, totals := range c.hours {
		snapshot.Hours = append(snapshot.Hours, cacheUsageHourRecord{cacheUsageHourKey: key, Totals: *totals})
	}
	for _, s := range c.sessions {
		copied := *s
		snapshot.Sessions = append(snapshot.Sessions, &copied)
	}
	snapshot.Labels = make(map[string]*cacheUsageLabel, len(c.labels))
	for id, label := range c.labels {
		copied := *label
		snapshot.Labels[id] = &copied
	}
	c.dirty = false
	c.lastSave = now
	c.mu.Unlock()

	errWrite := writeCacheUsageSnapshot(path, snapshot)

	c.mu.Lock()
	if errWrite != nil {
		// keep the changes pending so the next save retries them
		c.dirty = true
		log.WithError(errWrite).Warn("cache usage: failed to save snapshot")
	}
	c.saving = false
	c.mu.Unlock()
}

func writeCacheUsageSnapshot(path string, snapshot cacheUsageSnapshot) error {
	data, errEncode := json.Marshal(snapshot)
	if errEncode != nil {
		return errEncode
	}
	tmp := filepath.Join(filepath.Dir(path), affinity.CacheUsageTempFileName)
	if errWrite := os.WriteFile(tmp, data, 0o600); errWrite != nil {
		return errWrite
	}
	return os.Rename(tmp, path)
}

// capSessionsLocked drops the least recently active session once a new one exceeds the cap
func (c *cacheUsage) capSessionsLocked() {
	if len(c.sessions) <= cacheUsageMaxSessions {
		return
	}
	var oldest *cacheUsageSession
	for _, s := range c.sessions {
		if oldest == nil || s.Last < oldest.Last {
			oldest = s
		}
	}
	delete(c.sessions, oldest.ID)
}

func (c *cacheUsage) pruneLocked(now time.Time) {
	oldestHour := now.Add(-CacheUsageRetention).Truncate(time.Hour).Unix()
	for key := range c.hours {
		if key.Hour < oldestHour {
			delete(c.hours, key)
		}
	}
	idleBefore := now.Add(-cacheUsageSessionIdle).UnixMilli()
	for id, s := range c.sessions {
		if s.Last < idleBefore {
			delete(c.sessions, id)
		}
	}
	defer c.pruneLabelsLocked(now)
	if len(c.sessions) <= cacheUsageMaxSessions {
		return
	}
	ordered := make([]*cacheUsageSession, 0, len(c.sessions))
	for _, s := range c.sessions {
		ordered = append(ordered, s)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Last < ordered[j].Last })
	for _, s := range ordered[:len(ordered)-cacheUsageMaxSessions] {
		delete(c.sessions, s.ID)
	}
}

// pruneLabelsLocked drops labels whose session is gone, once a new session's first request has
// had time to finish and record it
func (c *cacheUsage) pruneLabelsLocked(now time.Time) {
	pendingBefore := now.Add(-cacheUsageLabelPending).UnixMilli()
	for id, label := range c.labels {
		if c.sessions[id] == nil && label.At < pendingBefore {
			delete(c.labels, id)
		}
	}
}

// filterCacheUsageSessions keeps sessions recorded under exactly id; a bare id without a
// provider prefix also matches "<provider>:<id>" when no session has exactly that id, and every
// such match is returned so the caller sees which providers share it
func filterCacheUsageSessions(sessions []CacheUsageSession, id string) []CacheUsageSession {
	if id == "" {
		return sessions
	}
	var exact, prefixed []CacheUsageSession
	for _, s := range sessions {
		if s.SessionID == id {
			exact = append(exact, s)
		} else if _, rest, ok := strings.Cut(s.SessionID, ":"); ok && rest == id && !strings.Contains(id, ":") {
			prefixed = append(prefixed, s)
		}
	}
	if len(exact) > 0 {
		return exact
	}
	if prefixed == nil {
		return []CacheUsageSession{}
	}
	return prefixed
}

// CacheUsage reports prompt cache usage by credential and session for requests in [Since, Until]
func (m *Manager) CacheUsage(q CacheUsageQuery) CacheUsageReport {
	report := CacheUsageReport{Retention: CacheUsageRetention, Credentials: []CacheUsageCredential{}, Models: []CacheUsageModel{}, Sessions: []CacheUsageSession{}}
	if m == nil || m.cacheUsage == nil {
		return report
	}
	c := m.cacheUsage
	report.Since, report.Until = q.Since.Truncate(time.Hour), q.Until.Truncate(time.Hour).Add(time.Hour)
	sinceHour, until := report.Since.Unix(), report.Until.Unix()

	// copy under the cache usage lock, then label credentials under the manager lock
	c.mu.Lock()
	report.Persisted = c.path != ""
	byAuth := map[string]*CacheUsageCredential{}
	// a credential hour holds one entry per model, merged here into one point
	byAuthHour := map[cacheUsageHourKey]*CacheUsageTotals{}
	byModel := map[cacheUsageHourKey]*CacheUsageModel{}
	for key, totals := range c.hours {
		if key.Hour < sinceHour || key.Hour >= until || (q.AuthID != "" && key.AuthID != q.AuthID) {
			continue
		}
		credential := byAuth[key.AuthID]
		if credential == nil {
			credential = &CacheUsageCredential{AuthID: key.AuthID, Provider: key.Provider}
			byAuth[key.AuthID] = credential
		}
		credential.Totals.add(*totals)
		hourKey := cacheUsageHourKey{AuthID: key.AuthID, Hour: key.Hour}
		if byAuthHour[hourKey] == nil {
			byAuthHour[hourKey] = &CacheUsageTotals{}
		}
		byAuthHour[hourKey].add(*totals)
		modelKey := cacheUsageHourKey{Provider: key.Provider, Model: key.Model}
		model := byModel[modelKey]
		if model == nil {
			model = &CacheUsageModel{Model: key.Model, Provider: key.Provider}
			byModel[modelKey] = model
		}
		model.Totals.add(*totals)
	}
	for key, totals := range byAuthHour {
		credential := byAuth[key.AuthID]
		credential.Hours = append(credential.Hours, CacheUsageHour{Start: time.Unix(key.Hour, 0), Totals: *totals})
	}
	for _, model := range byModel {
		report.Models = append(report.Models, *model)
	}
	for _, s := range c.sessions {
		if s.Last < report.Since.UnixMilli() || s.First >= report.Until.UnixMilli() || (q.AuthID != "" && s.AuthID != q.AuthID) {
			continue
		}
		session := CacheUsageSession{
			SessionID: s.ID, ParentSessionID: s.ParentID, AuthID: s.AuthID, Provider: s.Provider, Model: s.Model,
			FirstSeen: time.UnixMilli(s.First), LastSeen: time.UnixMilli(s.Last), Totals: s.Totals, LastRequest: s.LastRequest,
		}
		if label := c.labels[s.ID]; label != nil {
			session.Title, session.Project = label.Title, label.Project
		}
		report.Sessions = append(report.Sessions, session)
	}
	c.mu.Unlock()
	report.Sessions = filterCacheUsageSessions(report.Sessions, q.SessionID)

	for _, credential := range byAuth {
		if a, ok := m.GetByID(credential.AuthID); ok {
			credential.AuthIndex = a.EnsureIndex()
			credential.Email = metadataString(a.Metadata, "email")
			credential.PlanType = strings.TrimSpace(a.Attributes["plan_type"])
			if credential.PlanType == "" {
				credential.PlanType = metadataString(a.Metadata, "plan_type")
			}
		}
		sort.Slice(credential.Hours, func(i, j int) bool { return credential.Hours[i].Start.Before(credential.Hours[j].Start) })
		report.Totals.add(credential.Totals)
		report.Credentials = append(report.Credentials, *credential)
	}
	sort.Slice(report.Credentials, func(i, j int) bool { return report.Credentials[i].AuthID < report.Credentials[j].AuthID })
	sort.Slice(report.Models, func(i, j int) bool {
		if report.Models[i].Totals.Requests != report.Models[j].Totals.Requests {
			return report.Models[i].Totals.Requests > report.Models[j].Totals.Requests
		}
		if report.Models[i].Model != report.Models[j].Model {
			return report.Models[i].Model < report.Models[j].Model
		}
		return report.Models[i].Provider < report.Models[j].Provider
	})
	sort.Slice(report.Sessions, func(i, j int) bool { return report.Sessions[i].LastSeen.After(report.Sessions[j].LastSeen) })
	return report
}

// CacheUsagePlugin returns the usage plugin that feeds CacheUsage
func (m *Manager) CacheUsagePlugin() usage.Plugin { return cacheUsagePlugin{m} }

type cacheUsagePlugin struct{ m *Manager }

func (p cacheUsagePlugin) HandleUsage(_ context.Context, record usage.Record) {
	if p.m != nil {
		p.m.cacheUsage.record(record)
	}
}

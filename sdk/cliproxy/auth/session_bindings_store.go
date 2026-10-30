package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/affinity"
	log "github.com/sirupsen/logrus"
)

// sessionBindingsSaveInterval bounds how many new bindings a crash can lose; a clean shutdown
// saves once more
const sessionBindingsSaveInterval = time.Minute

// sessionBindingsSnapshot is the saved form of the session affinity cache
type sessionBindingsSnapshot struct {
	Version  int                  `json:"version"`
	Sequence uint64               `json:"sequence"`
	Groups   []sessionCacheRecord `json:"groups"`
}

// sessionBindingsStore keeps session-to-credential bindings across restarts, so a restart does
// not move live sessions to another credential and lose their prompt caches; it reads the
// manager's current cache on each save because routing reloads may replace the selector
type sessionBindingsStore struct {
	cache func() *SessionCache

	mu   sync.Mutex
	path string
	// pending holds a loaded snapshot until a session cache exists to restore it into
	pending *sessionBindingsSnapshot
	// savedCache and saved identify the last written state; a replaced cache always saves
	savedCache *SessionCache
	saved      uint64
	stop       chan struct{}
	stopped    chan struct{}
}

func newSessionBindingsStore(cache func() *SessionCache) *sessionBindingsStore {
	return &sessionBindingsStore{cache: cache}
}

// open restores saved bindings from dir and starts periodic saves
func (s *sessionBindingsStore) open(dir string) {
	if s == nil || dir == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path != "" {
		return
	}
	s.path = filepath.Join(dir, affinity.SessionBindingsFileName)

	var snapshot sessionBindingsSnapshot
	if data, errRead := os.ReadFile(s.path); errRead == nil {
		if errDecode := json.Unmarshal(data, &snapshot); errDecode != nil {
			log.WithError(errDecode).Warn("session bindings: ignoring unreadable snapshot")
			snapshot = sessionBindingsSnapshot{}
		}
	} else if !os.IsNotExist(errRead) {
		log.WithError(errRead).Warn("session bindings: failed to read snapshot")
	}
	s.pending = &snapshot
	s.restorePendingLocked(s.cache())

	s.stop, s.stopped = make(chan struct{}), make(chan struct{})
	go s.loop(s.stop, s.stopped)
}

func (s *sessionBindingsStore) loop(stop, stopped chan struct{}) {
	defer close(stopped)
	ticker := time.NewTicker(sessionBindingsSaveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			s.save()
		}
	}
}

// restorePendingLocked restores the loaded snapshot once a session cache exists; an SDK
// manager may install its session selector after the profile opens
func (s *sessionBindingsStore) restorePendingLocked(cache *SessionCache) {
	if s.pending == nil || cache == nil {
		return
	}
	if restored := cache.restore(s.pending.Groups, s.pending.Sequence); restored > 0 {
		log.Infof("session bindings: restored %d session bindings", restored)
	}
	s.pending = nil
}

// close stops periodic saves and writes the final snapshot
func (s *sessionBindingsStore) close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	stop, stopped := s.stop, s.stopped
	s.stop, s.stopped = nil, nil
	s.mu.Unlock()
	if stop != nil {
		close(stop)
		<-stopped
	}
	s.save()
	s.mu.Lock()
	s.path, s.pending = "", nil
	s.mu.Unlock()
}

// save writes the snapshot when the cache changed since the last write
func (s *sessionBindingsStore) save() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == "" {
		return
	}
	cache := s.cache()
	// nothing to save yet, and writing now would replace bindings still waiting to restore
	if cache == nil {
		return
	}
	s.restorePendingLocked(cache)
	groups, sequence, changes := cache.snapshot()
	if cache == s.savedCache && changes == s.saved {
		return
	}
	data, errEncode := json.Marshal(sessionBindingsSnapshot{Version: 1, Sequence: sequence, Groups: groups})
	if errEncode != nil {
		log.WithError(errEncode).Warn("session bindings: failed to encode snapshot")
		return
	}
	tmp := filepath.Join(filepath.Dir(s.path), affinity.SessionBindingsTempFileName)
	if errWrite := os.WriteFile(tmp, data, 0o600); errWrite != nil {
		log.WithError(errWrite).Warn("session bindings: failed to save snapshot")
		return
	}
	if errRename := os.Rename(tmp, s.path); errRename != nil {
		log.WithError(errRename).Warn("session bindings: failed to save snapshot")
		return
	}
	s.savedCache, s.saved = cache, changes
}

// sessionAffinityCache returns the current selector's session cache, or nil without one
func (m *Manager) sessionAffinityCache() *SessionCache {
	if selector, ok := m.Selector().(*SessionAffinitySelector); ok && selector != nil {
		return selector.cache
	}
	return nil
}

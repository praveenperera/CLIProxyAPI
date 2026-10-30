package auth

import "time"

// setTTL changes how long later bindings and refreshes last; existing expiries are kept
func (c *SessionCache) setTTL(ttl time.Duration) {
	if c == nil || ttl <= 0 {
		return
	}
	c.mu.Lock()
	c.ttl = ttl
	c.mu.Unlock()
}

// sessionCacheRecord is one saved alias group
type sessionCacheRecord struct {
	AuthID    string   `json:"auth_id"`
	ExpiresAt int64    `json:"expires_at_ms"`
	Aliases   []string `json:"aliases"`
	SessionID string   `json:"session_id,omitempty"`
	Provider  string   `json:"provider,omitempty"`
	Sequence  uint64   `json:"sequence,omitempty"`
}

// snapshot returns the live alias groups oldest first, the selection sequence, and the change
// count they reflect
func (c *SessionCache) snapshot() ([]sessionCacheRecord, uint64, uint64) {
	if c == nil {
		return nil, 0, 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	now := time.Now()
	records := make([]sessionCacheRecord, 0, len(c.groups))
	for e := c.evictionOrder.Front(); c.evictionOrder != nil && e != nil; e = e.Next() {
		key, _ := e.Value.(string)
		group, ok := c.groups[key]
		if !ok || !now.Before(group.expiresAt) {
			continue
		}
		records = append(records, sessionCacheRecord{
			AuthID:    group.authID,
			ExpiresAt: group.expiresAt.UnixMilli(),
			Aliases:   append([]string(nil), group.aliases...),
			SessionID: group.binding.sessionID,
			Provider:  group.binding.provider,
			Sequence:  group.binding.sequence,
		})
	}
	return records, c.bindingSequence, c.changes
}

// restore adds saved groups that are still unexpired; a binding made since startup is newer
// than the saved one, so any group sharing an alias with a live binding is skipped, live
// selections are renumbered after the saved ones, and saved groups are evicted first
func (c *SessionCache) restore(records []sessionCacheRecord, sequence uint64) int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensureInitializedLocked()
	now := time.Now()

	// latestSessionAuth compares sequences, so live selections must outrank every saved one
	if sequence > 0 {
		for key, group := range c.groups {
			if group.binding.sequence == 0 {
				continue
			}
			group.binding.sequence += sequence
			c.groups[key] = group
			for _, alias := range group.aliases {
				if current, ok := c.entries[alias]; ok && sameSessionEntryGroup(current, group) {
					c.entries[alias] = group
				}
			}
		}
		c.bindingSequence += sequence
	}

	// records arrive oldest first; inserting newest first at the front keeps that order ahead
	// of live groups
	restored := 0
	for i := len(records) - 1; i >= 0; i-- {
		r := records[i]
		expiresAt := time.UnixMilli(r.ExpiresAt)
		aliases := compactSessionAliases(mergeSessionAliases(nil, r.Aliases...))
		if r.AuthID == "" || len(aliases) == 0 || !now.Before(expiresAt) {
			continue
		}
		live := false
		for _, alias := range aliases {
			if entry, ok := c.entries[alias]; ok && now.Before(entry.expiresAt) {
				live = true
				break
			}
		}
		if live {
			continue
		}
		binding := sessionBinding{sessionID: r.SessionID, provider: r.Provider, sequence: min(r.Sequence, sequence)}
		c.replaceAliasGroupsLocked(r.AuthID, expiresAt, aliases, binding)
		if elem, ok := c.evictionElements[aliases[0]]; ok {
			c.evictionOrder.MoveToFront(elem)
		}
		restored++
	}
	return restored
}

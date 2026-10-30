package quota

import "time"

// Windows returns an independent immutable window list
func (s Snapshot) Windows() []Window { return append([]Window(nil), s.windows...) }

// WindowSnapshot retains the original source and receipt for one window
func (s Snapshot) WindowSnapshot(w Window) Snapshot {
	return Snapshot{credential: s.credential, source: s.source, observed: s.observed, windows: []Window{w}}
}

// Combine evaluates separately timed windows without making stale data fresh
// The manager supplies the latest fenced observation for each distinct window
func Combine(snapshots []Snapshot, model string, now time.Time, maxAge time.Duration) View {
	result := View{knowledge: Unknown, freshness: Stale, partial: len(snapshots) == 0}
	seenApplicable, allUnsupported := false, true
	for _, s := range snapshots {
		v := s.View(model, now, maxAge)
		result.partial = result.partial || v.partial
		if v.freshness != Fresh {
			continue
		}
		result.freshness = Fresh
		result.windows = append(result.windows, v.windows...)
		if len(v.windows) > 0 {
			seenApplicable = true
			allUnsupported = allUnsupported && v.knowledge == Unsupported
		}
		result.denied = result.denied || v.denied
		if v.knowledge == Known {
			result.knowledge = Known
		}
		if !v.nextReset.IsZero() && (result.nextReset.IsZero() || v.nextReset.Before(result.nextReset)) {
			result.nextReset = v.nextReset
		}
	}
	if result.knowledge != Known && seenApplicable && allUnsupported {
		result.knowledge = Unsupported
	}
	result.allowance = effectiveAllowance(result.windows, result.nextReset)
	return result
}

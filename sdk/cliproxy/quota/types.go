// Package quota models immutable observations without reserving or refilling quota
package quota

import (
	"errors"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/affinity"
)

// Source identifies the observed acquisition boundary, not a guarantee of provider authority
type Source uint8

const (
	// PassiveHTTP is a response-header observation
	PassiveHTTP Source = iota + 1
	// PassiveWebsocket is one complete meter event
	PassiveWebsocket
	// ExistingUsageGET is a read-only existing provider usage endpoint
	ExistingUsageGET
)

// Knowledge distinguishes absent, unsupported, and measured values
type Knowledge uint8

const (
	// Unknown has no measured remaining value
	Unknown Knowledge = iota
	// Unsupported has no supported acquisition or applicable meter
	Unsupported
	// Known contains a validated amount or fraction
	Known
)

// Freshness is evaluated from receipt time, without rewriting that timestamp
type Freshness uint8

const (
	// Stale observations cannot establish exhaustion or positive allowance
	Stale Freshness = iota
	// Fresh observations are within the supplied maximum age
	Fresh
)

// CalendarKind identifies a provider-established calendar window
type CalendarKind uint8

const (
	// Day is a calendar-day window
	Day CalendarKind = iota + 1
	// Week is a calendar-week window
	Week
	// Month is a calendar-month window
	Month
)

// Period keeps unknown, rolling, and calendar windows distinct
type Period struct {
	rolling  time.Duration
	calendar CalendarKind
}

// Rolling validates an explicit supplied window duration
func Rolling(duration time.Duration) (Period, error) {
	if duration <= 0 || duration > 366*24*time.Hour {
		return Period{}, errors.New("invalid quota period")
	}
	return Period{rolling: duration}, nil
}

// Calendar validates a provider-established calendar kind
func Calendar(kind CalendarKind) (Period, error) {
	if kind < Day || kind > Month {
		return Period{}, errors.New("invalid quota calendar")
	}
	return Period{calendar: kind}, nil
}

// Duration returns a rolling duration, or zero for unknown/calendar periods
func (p Period) Duration() time.Duration { return p.rolling }

// CalendarKind returns the calendar kind, or zero for unknown/rolling periods
func (p Period) CalendarKind() CalendarKind { return p.calendar }

// Scope specifies applicability established by the provider adapter
// The zero scope is invalid, not a wildcard
type Scope struct {
	meter  string
	models []string
}

// SharedCredential constructs a credential-wide meter scope
func SharedCredential(meter string) (Scope, error) {
	if !text(meter, 128) {
		return Scope{}, errors.New("invalid quota meter")
	}
	return Scope{meter: meter}, nil
}

// ModelSet constructs an explicit canonical-model scope, never inferred from labels
func ModelSet(meter string, models []string) (Scope, error) {
	scope, err := SharedCredential(meter)
	if err != nil {
		return Scope{}, err
	}
	if len(models) == 0 || len(models) > 256 {
		return Scope{}, errors.New("invalid quota model set")
	}
	seen := make(map[string]bool, len(models))
	for _, model := range models {
		if !text(model, 256) || seen[model] {
			return Scope{}, errors.New("invalid quota model")
		}
		seen[model] = true
	}
	scope.models = append([]string(nil), models...)
	return scope, nil
}

// Meter returns the provider-owned meter identifier
func (s Scope) Meter() string { return s.meter }

// Models returns a copy of explicit model applicability; nil means credential-wide
func (s Scope) Models() []string { return append([]string(nil), s.models...) }
func (s Scope) applies(model string) bool {
	if len(s.models) == 0 {
		return true
	}
	for _, m := range s.models {
		if m == model {
			return true
		}
	}
	return false
}

// ComparisonKey identifies an adapter-verified equivalent debit unit and entitlement
// An empty key deliberately makes numeric values incomparable
type ComparisonKey struct{ value string }

// NewComparisonKey validates an equivalence key; it does not verify provider facts
func NewComparisonKey(value string) (ComparisonKey, error) {
	if !text(value, 512) {
		return ComparisonKey{}, errors.New("invalid quota comparison key")
	}
	return ComparisonKey{value}, nil
}

// Remaining is a sealed unknown, unsupported, fraction, or amount value
// Its zero value is unknown, never a measured zero
type Remaining struct {
	knowledge Knowledge
	amount    bool
	value     float64
	unit      string
	group     ComparisonKey
}

// UnknownRemaining constructs an unmeasured value
func UnknownRemaining() Remaining { return Remaining{} }

// UnsupportedRemaining constructs a value not supplied by this source
func UnsupportedRemaining() Remaining { return Remaining{knowledge: Unsupported} }

// Fraction validates a finite fraction without claiming absolute capacity
func Fraction(value float64, group ComparisonKey) (Remaining, error) {
	if !finite(value) || value < 0 || value > 1 {
		return Remaining{}, errors.New("invalid quota fraction")
	}
	return Remaining{knowledge: Known, value: value, group: group}, nil
}

// Amount validates an absolute amount in an adapter-established debit unit
func Amount(value float64, unit string, group ComparisonKey) (Remaining, error) {
	if !finite(value) || value < 0 || !text(unit, 64) {
		return Remaining{}, errors.New("invalid quota amount")
	}
	return Remaining{knowledge: Known, amount: true, value: value, unit: unit, group: group}, nil
}

// Knowledge returns whether a measured numeric value exists
func (r Remaining) Knowledge() Knowledge { return r.knowledge }

// Value returns the measured numeric value and its presence
func (r Remaining) Value() (float64, bool) { return r.value, r.knowledge == Known }

// IsAmount distinguishes absolute units from fractions
func (r Remaining) IsAmount() bool { return r.amount }

// Unit returns the absolute debit unit, or empty for a fraction
func (r Remaining) Unit() string { return r.unit }

// Comparison returns the explicit adapter-established equivalence key
func (r Remaining) Comparison() ComparisonKey { return r.group }

// WindowInput is presence-aware parse-layer data validated by NewWindow
type WindowInput struct {
	ID        string
	Scope     Scope
	Period    Period
	ResetAt   *time.Time
	Remaining Remaining
	Allowed   *bool
}

// Window is a validated immutable applicable constraint
type Window struct {
	id                  string
	scope               Scope
	period              Period
	reset               time.Time
	remaining           Remaining
	allowed, hasAllowed bool
}

// NewWindow validates bounded provider fields and keeps absent reset/allowed distinct
func NewWindow(input WindowInput) (Window, error) {
	if !text(input.ID, 128) || input.Scope.meter == "" {
		return Window{}, errors.New("invalid quota window")
	}
	w := Window{id: input.ID, scope: Scope{input.Scope.meter, append([]string(nil), input.Scope.models...)}, period: input.Period, remaining: input.Remaining}
	if input.ResetAt != nil {
		if !validTime(*input.ResetAt) {
			return Window{}, errors.New("invalid quota reset")
		}
		w.reset = input.ResetAt.UTC()
	}
	if input.Allowed != nil {
		w.allowed = *input.Allowed
		w.hasAllowed = true
	}
	return w, nil
}

// ID returns the provider-owned window name
func (w Window) ID() string { return w.id }

// Scope returns a copy of the applicability scope
func (w Window) Scope() Scope { return Scope{w.scope.meter, append([]string(nil), w.scope.models...)} }

// Period returns the observed period without guessing missing durations
func (w Window) Period() Period { return w.period }

// ResetAt returns the known reset and presence
func (w Window) ResetAt() (time.Time, bool) { return w.reset, !w.reset.IsZero() }

// Remaining returns the immutable measured variant
func (w Window) Remaining() Remaining { return w.remaining }

// Allowed returns the explicit allowed flag and presence
func (w Window) Allowed() (bool, bool) { return w.allowed, w.hasAllowed }

// SnapshotInput contains one complete receipt-time observation, not merged partial fields
type SnapshotInput struct {
	Credential affinity.CredentialRef
	Source     Source
	ObservedAt time.Time
	Windows    []WindowInput
}

// Snapshot is immutable and requires manager lineage and generation checks before ingestion
type Snapshot struct {
	credential affinity.CredentialRef
	source     Source
	observed   time.Time
	windows    []Window
}

// NewSnapshot validates a bounded complete observation and unique meter/window pairs
func NewSnapshot(input SnapshotInput) (Snapshot, error) {
	if input.Credential.AuthID() == "" || input.Source < PassiveHTTP || input.Source > ExistingUsageGET || !validTime(input.ObservedAt) || len(input.Windows) > 32 {
		return Snapshot{}, errors.New("invalid quota snapshot")
	}
	s := Snapshot{credential: input.Credential, source: input.Source, observed: input.ObservedAt.UTC(), windows: make([]Window, 0, len(input.Windows))}
	seen := make(map[string]bool, len(input.Windows))
	for _, inputWindow := range input.Windows {
		window, err := NewWindow(inputWindow)
		if err != nil {
			return Snapshot{}, err
		}
		// IDs are unique across the receipt, so distinct scope labels cannot hide duplicates
		if seen[window.id] {
			return Snapshot{}, errors.New("duplicate quota window")
		}
		seen[window.id] = true
		s.windows = append(s.windows, window)
	}
	return s, nil
}

// Credential returns the exact auth and lineage to which this observation belongs
func (s Snapshot) Credential() affinity.CredentialRef { return s.credential }

// Source returns the acquisition boundary
func (s Snapshot) Source() Source { return s.source }

// ObservedAt returns the original receipt time, not the latest fetch attempt
func (s Snapshot) ObservedAt() time.Time { return s.observed }

// View is an immutable model-applicable observation evaluated at a supplied clock
type View struct {
	knowledge       Knowledge
	freshness       Freshness
	windows         []Window
	denied, partial bool
	nextReset       time.Time
	allowance       Remaining
}

// View evaluates freshness and reset expiry without local quota refill
func (s Snapshot) View(model string, now time.Time, maxAge time.Duration) View {
	v := View{knowledge: Unknown, freshness: Stale, partial: true}
	if maxAge <= 0 || s.observed.IsZero() || now.Before(s.observed) || now.Sub(s.observed) > maxAge {
		return v
	}
	v.freshness = Fresh
	v.partial = false
	allUnsupported := true
	for _, w := range s.windows {
		if !w.scope.applies(model) {
			continue
		}
		// a passed reset invalidates that complete window until a new provider observation
		if !w.reset.IsZero() && !w.reset.After(now) {
			w.remaining = UnknownRemaining()
			w.hasAllowed = false
		}
		v.windows = append(v.windows, w)
		if w.remaining.knowledge != Unsupported {
			allUnsupported = false
		}
		value, known := w.remaining.Value()
		if (w.hasAllowed && !w.allowed) || (known && value == 0) {
			v.denied = true
		}
		if known {
			v.knowledge = Known
		} else {
			v.partial = true
		}
		if !known || w.reset.IsZero() {
			v.partial = true
			continue
		}
		if value > 0 && (v.nextReset.IsZero() || w.reset.Before(v.nextReset)) {
			v.nextReset = w.reset
		}
	}
	if len(v.windows) == 0 {
		v.partial = true
	}
	if len(v.windows) > 0 && allUnsupported {
		v.knowledge = Unsupported
	}
	v.allowance = effectiveAllowance(v.windows, v.nextReset)
	return v
}

// Knowledge returns the distinct observed knowledge state
func (v View) Knowledge() Knowledge { return v.knowledge }

// Freshness returns the evaluated receipt-age state
func (v View) Freshness() Freshness { return v.freshness }

// Denied returns a fresh applicable zero or explicit denial
func (v View) Denied() bool { return v.denied }

// Partial reports missing fields or unknown companion constraints
func (v View) Partial() bool { return v.partial }

// Windows returns an independent copy of all applicable immutable windows
func (v View) Windows() []Window { return append([]Window(nil), v.windows...) }

// NextReset returns the earliest fresh positive future reset, if present
func (v View) NextReset() (time.Time, bool) { return v.nextReset, !v.nextReset.IsZero() }

// ComparableAllowance returns the verified effective allowance, if comparable
func (v View) ComparableAllowance() Remaining { return v.allowance }

func effectiveAllowance(windows []Window, next time.Time) Remaining {
	if next.IsZero() {
		return Remaining{}
	}
	var absolute Remaining
	allAbsolute := true
	haveAbsolute := false
	for _, w := range windows {
		r := w.remaining
		if r.knowledge != Known {
			continue
		}
		if !r.amount || r.group.value == "" {
			allAbsolute = false
			break
		}
		if !haveAbsolute {
			absolute = r
			haveAbsolute = true
			continue
		}
		if !sameGroup(absolute, r) {
			allAbsolute = false
			break
		}
		if r.value < absolute.value {
			absolute = r
		}
	}
	if allAbsolute && haveAbsolute {
		return absolute
	}
	var determining Remaining
	have := false
	for _, w := range windows {
		r := w.remaining
		if !w.reset.Equal(next) || r.knowledge != Known || r.value <= 0 {
			continue
		}
		if r.group.value == "" {
			return Remaining{}
		}
		if !have {
			determining = r
			have = true
			continue
		}
		if !sameGroup(determining, r) {
			return Remaining{}
		}
		if r.value < determining.value {
			determining = r
		}
	}
	return determining
}
func sameGroup(a, b Remaining) bool {
	return a.group == b.group && a.amount == b.amount && a.unit == b.unit
}
func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }
func text(value string, max int) bool {
	if value == "" || len(value) > max || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func validTime(t time.Time) bool { return !t.IsZero() && t.Year() >= 1970 && t.Year() <= 9999 }

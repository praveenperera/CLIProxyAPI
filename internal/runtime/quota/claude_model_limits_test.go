package quota

import (
	"strings"
	"testing"
	"time"
)

// a Claude usage reply trimmed to the fields the parser reads, with the Fable-only limit used up
const claudeReplyWithFable = `{
	"five_hour": null,
	"seven_day": {"utilization": 7.0, "resets_at": "2026-10-05T05:00:00.209259+00:00"},
	"seven_day_opus": null,
	"limits": [
		{"kind": "weekly_all", "group": "weekly", "percent": 7, "resets_at": "2026-10-05T05:00:00.209259+00:00", "scope": null},
		{"kind": "weekly_scoped", "group": "weekly", "percent": 100, "resets_at": "2026-10-05T05:00:00.209406+00:00",
			"scope": {"model": {"id": null, "display_name": "Fable"}, "surface": null}}
	]
}`

func TestClaudeFableLimitIsRecordedButNeverRouted(t *testing.T) {
	ref := credential(t, "claude")
	usage, err := ParseClaudeUsage(ref, []byte(claudeReplyWithFable), now)
	if err != nil {
		t.Fatal(err)
	}

	windows := usage.Windows()
	if len(windows) != 2 || windows[0].ID() != "seven_day" || windows[1].ID() != "seven_day_fable" {
		t.Fatalf("windows = %+v", windows)
	}
	fable := windows[1]
	remaining, known := fable.Remaining().Value()
	reset, hasReset := fable.ResetAt()
	if fable.Scope().Label() != "Fable" || !known || remaining != 0 || fable.Period().Duration() != 7*24*time.Hour || !hasReset || reset.Day() != 5 {
		t.Fatalf("fable window = label %q remaining %v/%v period %v reset %v", fable.Scope().Label(), remaining, known, fable.Period().Duration(), reset)
	}
	if windows[0].Scope().Label() != "" {
		t.Fatal("the account-wide weekly limit got a label")
	}

	// a used-up Fable limit must not deny any request, Fable's included, since the reply names
	// no canonical model; routing sees only the account-wide limit
	for _, model := range []string{"claude-fable-5-2", "claude-opus-5-5"} {
		view := usage.View(model, now, 5*time.Minute)
		if view.Denied() || len(view.Windows()) != 1 || view.Windows()[0].ID() != "seven_day" {
			t.Fatalf("%s view = denied %v windows %+v", model, view.Denied(), view.Windows())
		}
	}
}

func TestClaudeUnreadableLimitsKeepTheAccountLimits(t *testing.T) {
	ref := credential(t, "claude")
	for _, limits := range []string{
		`"oops"`,
		`[{"group": "weekly", "percent": "full", "scope": {"model": {"display_name": "Fable"}}}]`,
		// the same model twice would be a duplicate window and fail the whole snapshot
		`[{"group": "weekly", "percent": 1, "scope": {"model": {"display_name": "Fable"}}},
		  {"group": "weekly", "percent": 2, "scope": {"model": {"display_name": "fable"}}}]`,
	} {
		usage, err := ParseClaudeUsage(ref, []byte(`{"seven_day": {"utilization": 40}, "limits": `+limits+`}`), now)
		if err != nil {
			t.Fatalf("limits %s: %v", limits, err)
		}
		value, known := usage.View("claude-opus-5-5", now, 5*time.Minute).Windows()[0].Remaining().Value()
		if !known || value != 0.6 {
			t.Fatalf("limits %s: seven_day remaining %v", limits, value)
		}
	}
}

func TestClaudeModelLimitsSkipOnlyTheEntryThatCantBeRead(t *testing.T) {
	ref := credential(t, "claude")
	long := strings.Repeat("Fable ", 20)
	reply := `{"seven_day": {"utilization": 40}, "limits": [
		{"group": "weekly", "percent": "full", "scope": {"model": {"display_name": "Opus"}}},
		{"group": "weekly", "percent": 0, "scope": {"model": {"display_name": "` + long + `"}}},
		{"group": "weekly", "percent": 100, "scope": {"model": {"display_name": "Fable"}}}
	]}`
	usage, err := ParseClaudeUsage(ref, []byte(reply), now)
	if err != nil {
		t.Fatal(err)
	}

	// the unreadable Opus entry and the name too long for a window ID are dropped, and neither
	// takes the account limit or the readable Fable entry with it
	windows := usage.Windows()
	if len(windows) != 2 || windows[0].ID() != "seven_day" || windows[1].ID() != "seven_day_fable" {
		t.Fatalf("windows = %+v", windows)
	}
	if value, known := windows[1].Remaining().Value(); !known || value != 0 {
		t.Fatalf("fable remaining %v", value)
	}
}

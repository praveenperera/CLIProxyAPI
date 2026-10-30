package affinity

import (
	"strings"
	"testing"
	"time"
)

func testKey(t *testing.T, namespace, caller, id string) ChatKey {
	t.Helper()
	c, err := NewCallerScope(caller)
	if err != nil {
		t.Fatal(err)
	}
	chat, err := NewChatID(id)
	if err != nil {
		t.Fatal(err)
	}
	key, err := NewChatKey(namespace, c, chat)
	if err != nil {
		t.Fatal(err)
	}
	return key
}
func testRef(t *testing.T, id string) CredentialRef {
	t.Helper()
	lineage := [32]byte{1}
	ref, err := NewCredentialRef("claude", id, lineage)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}
func testBinding(t *testing.T, key ChatKey, id string, now time.Time, retention Retention) Binding {
	t.Helper()
	h, err := NewHandle()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewBinding(key, h, testRef(t, id), now, now, retention.Expiration(now, now))
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func TestIdentityValidationAndIsolation(t *testing.T) {
	for _, bad := range []string{"", " leading", "trailing ", "a\nb", "a\x00b", strings.Repeat("a", 257), string([]byte{0xff})} {
		if _, err := NewChatID(bad); err == nil {
			t.Errorf("accepted invalid chat ID")
		}
		if _, err := NewCallerScope(bad); err == nil {
			t.Errorf("accepted invalid scope")
		}
	}
	cases := [][3]string{{"n", "a:b", "c"}, {"n", "a", "b:c"}, {"n2", "a", "b:c"}, {"n", "other", "b:c"}}
	seen := map[[32]byte]bool{}
	for _, parts := range cases {
		key := testKey(t, parts[0], parts[1], parts[2])
		if seen[key.Digest()] {
			t.Fatal("tuple collision")
		}
		seen[key.Digest()] = true
	}
	if err := Start(ChatKey{}).Validate(); err == nil {
		t.Fatal("zero key accepted")
	}
	if err := Continue(testKey(t, "n", "c", "id"), Handle{}).Validate(); err == nil {
		t.Fatal("zero handle accepted")
	}
	if (Request{}).Intent() != Legacy {
		t.Fatal("zero request is not legacy")
	}
}
func TestHandleCanonicalAndBindingValidation(t *testing.T) {
	h, err := NewHandle()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseHandle(h.Encode())
	if err != nil || parsed != h {
		t.Fatal("handle roundtrip")
	}
	for _, bad := range []string{"", h.Encode() + "=", strings.Repeat("A", 43)} {
		if _, err := ParseHandle(bad); err == nil {
			t.Fatal("invalid handle accepted")
		}
	}
	now := time.Date(2026, 10, 3, 17, 0, 0, 0, time.UTC)
	key := testKey(t, "n", "c", "id")
	ref := testRef(t, "a")
	for _, times := range [][3]time.Time{{{}, now, now.Add(time.Hour)}, {now, now.Add(-time.Second), now.Add(time.Hour)}, {now, now, now}} {
		if _, err := NewBinding(key, h, ref, times[0], times[1], times[2]); err == nil {
			t.Fatal("invalid binding accepted")
		}
	}
}
func TestErrorStatusesAndRetention(t *testing.T) {
	for reason := IdentityRequired; reason <= PrefixTransformUnsupported; reason++ {
		e := NewError(reason, time.Now())
		if e.Error() == "" || e.StatusCode() < 400 || e.StatusCode() > 503 {
			t.Fatal("invalid local error")
		}
		if reason != QuotaDenied && reason != CredentialCooldown && !e.RetryAt().IsZero() {
			t.Fatal("unreliable retry time retained")
		}
	}
	for _, c := range []struct {
		idle, hard time.Duration
		capacity   int
	}{{0, time.Hour, 1}, {time.Hour, time.Minute, 1}, {time.Hour, time.Hour, 65537}} {
		if _, err := NewRetention(c.idle, c.hard, c.capacity); err == nil {
			t.Fatal("invalid retention accepted")
		}
	}
}

package auth

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
)

// A credential whose provenance no longer matches its token used to drop out of quota
// polling with no log line; it must warn once, stay quiet on repeats, and report recovery
func TestQuotaPollWarnsOnceWhenCredentialCheckSkipsCredential(t *testing.T) {
	m, e, _, _, ids := affinityFixture(t)
	id := ids[0]
	setMetadata := func(provenanceToken string) {
		t.Helper()
		a, _ := m.GetByID(id)
		a.Attributes = map[string]string{AttributeAuthKind: AuthKindOAuth}
		a.Metadata = map[string]any{"account_id": id, "oauth_provenance": testProvenance(t, id, provenanceToken), "access_token": "current"}
		if _, err := m.Update(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	setMetadata("previous")
	e.usage = func(context.Context, *Auth, *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"rate_limit":{"primary_window":{"used_percent":20,"limit_window_seconds":3600,"reset_after_seconds":3600}}}`))}, nil
	}
	hook := setupTestLoggerHook(t)
	log.SetLevel(log.InfoLevel)
	countFor := func(level log.Level, text string) int {
		n := 0
		for _, entry := range hook.AllEntries() {
			if entry.Level == level && strings.Contains(entry.Message, id) && strings.Contains(entry.Message, text) {
				n++
			}
		}
		return n
	}

	m.pollQuotaCredential(context.Background(), id, m.affinityRuntime)
	m.pollQuotaCredential(context.Background(), id, m.affinityRuntime)
	if got := countFor(log.WarnLevel, "credential check failed: chat_credential_changed"); got != 1 {
		t.Fatalf("credential check warnings = %d, want 1", got)
	}

	setMetadata("current")
	m.pollQuotaCredential(context.Background(), id, m.affinityRuntime)
	if got := countFor(log.InfoLevel, "usage reads recovered"); got != 1 {
		t.Fatalf("recovery logs = %d, want 1", got)
	}
}

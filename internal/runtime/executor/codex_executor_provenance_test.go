package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	codexauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	fileauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/affinity"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// A refresh must leave the on-disk provenance bound to the new access token, because the
// watcher reloads the file after every write and quota polling rejects a mismatched digest
func TestCodexExecutorRefreshPersistsProvenanceForNewAccessToken(t *testing.T) {
	const accountID = "acc-provenance"
	proxyURL, teardown := startCodexMockOAuthServers(t, makeTestCodexRefreshJWT("plus", accountID))
	defer teardown()

	staleProvenance, errProvenance := affinity.AcquiredOAuthProvenance("codex", accountID, "old-access-token")
	if errProvenance != nil {
		t.Fatalf("AcquiredOAuthProvenance: %v", errProvenance)
	}
	tmpDir := t.TempDir()
	authPath := filepath.Join(tmpDir, "codex-provenance.json")
	diskInitial := map[string]any{
		"type":             "codex",
		"access_token":     "old-access-token",
		"refresh_token":    "old-refresh-token",
		"account_id":       accountID,
		"expired":          time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
		"oauth_provenance": staleProvenance,
	}
	if err := codexauth.WriteJSONFileAtomic(authPath, diskInitial); err != nil {
		t.Fatalf("WriteJSONFileAtomic: %v", err)
	}
	diskMetadata := readCodexCredentialFile(t, authPath)
	auth := &cliproxyauth.Auth{
		ID:         "codex-provenance.json",
		FileName:   "codex-provenance.json",
		Provider:   "codex",
		ProxyURL:   proxyURL,
		Attributes: map[string]string{cliproxyauth.AttributePath: authPath},
		Metadata:   diskMetadata,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	refreshed, errRefresh := NewCodexExecutor(&config.Config{AuthDir: tmpDir}).Refresh(ctx, auth)
	if errRefresh != nil {
		t.Fatalf("Refresh: %v", errRefresh)
	}
	assertCodexProvenanceMatchesFile(t, authPath, "new-mock-access-token")

	// the manager persists the refreshed auth again through the scoped refresh patch
	store := fileauth.NewFileTokenStore()
	store.SetBaseDir(tmpDir)
	if _, errSave := store.Save(ctx, refreshed); errSave != nil {
		t.Fatalf("store.Save: %v", errSave)
	}
	assertCodexProvenanceMatchesFile(t, authPath, "new-mock-access-token")
}

// When another process already rotated the file, adopting its tokens must also adopt its
// provenance, or the refresh patch would write the stale in-memory digest back to disk
func TestCodexExecutorRefreshAdoptsRotatedDiskProvenance(t *testing.T) {
	const accountID = "acc-rotated"
	staleProvenance, errStale := affinity.AcquiredOAuthProvenance("codex", accountID, "old-access-token")
	if errStale != nil {
		t.Fatalf("AcquiredOAuthProvenance: %v", errStale)
	}
	rotatedProvenance, errRotated := affinity.AcquiredOAuthProvenance("codex", accountID, "rotated-access-token")
	if errRotated != nil {
		t.Fatalf("AcquiredOAuthProvenance: %v", errRotated)
	}
	tmpDir := t.TempDir()
	authPath := filepath.Join(tmpDir, "codex-rotated.json")
	if err := codexauth.WriteJSONFileAtomic(authPath, map[string]any{
		"type":             "codex",
		"access_token":     "rotated-access-token",
		"refresh_token":    "rotated-refresh-token",
		"account_id":       accountID,
		"expired":          time.Now().Add(5 * 24 * time.Hour).UTC().Format(time.RFC3339),
		"oauth_provenance": rotatedProvenance,
	}); err != nil {
		t.Fatalf("WriteJSONFileAtomic: %v", err)
	}
	auth := &cliproxyauth.Auth{
		ID:         "codex-rotated.json",
		FileName:   "codex-rotated.json",
		Provider:   "codex",
		Attributes: map[string]string{cliproxyauth.AttributePath: authPath},
		Metadata: map[string]any{
			"type":             "codex",
			"access_token":     "old-access-token",
			"refresh_token":    "old-refresh-token",
			"account_id":       accountID,
			"expired":          time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
			"oauth_provenance": staleProvenance,
		},
	}

	ctx := context.Background()
	refreshed, errRefresh := NewCodexExecutor(&config.Config{AuthDir: tmpDir}).Refresh(ctx, auth)
	if errRefresh != nil {
		t.Fatalf("Refresh: %v", errRefresh)
	}
	memoryProvenance, errParse := affinity.ParseOAuthProvenance(refreshed.Metadata["oauth_provenance"])
	if errParse != nil || !memoryProvenance.Matches("codex", accountID, "rotated-access-token") {
		t.Fatalf("in-memory oauth_provenance does not match the adopted disk token (parse error %v)", errParse)
	}

	// the executor leaves this write to the manager, which applies the refresh patch on save
	store := fileauth.NewFileTokenStore()
	store.SetBaseDir(tmpDir)
	if _, errSave := store.Save(ctx, refreshed); errSave != nil {
		t.Fatalf("store.Save: %v", errSave)
	}
	assertCodexProvenanceMatchesFile(t, authPath, "rotated-access-token")
}

func readCodexCredentialFile(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read %s: %v", path, errRead)
	}
	var metadata map[string]any
	if errUnmarshal := json.Unmarshal(raw, &metadata); errUnmarshal != nil {
		t.Fatalf("unmarshal %s: %v", path, errUnmarshal)
	}
	return metadata
}

func assertCodexProvenanceMatchesFile(t *testing.T, path, wantAccessToken string) {
	t.Helper()
	disk := readCodexCredentialFile(t, path)
	token, _ := disk["access_token"].(string)
	if token != wantAccessToken {
		t.Fatalf("disk access_token = %q, want %q", token, wantAccessToken)
	}
	account, _ := disk["account_id"].(string)
	provenance, errParse := affinity.ParseOAuthProvenance(disk["oauth_provenance"])
	if errParse != nil {
		t.Fatalf("parse disk oauth_provenance: %v", errParse)
	}
	if !provenance.Matches("codex", account, token) {
		t.Fatalf("disk oauth_provenance does not match account %q and the refreshed access token", account)
	}
}

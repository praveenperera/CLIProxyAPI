package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MoeForge/moego/json"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/affinity"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// provenanceClaudeOAuthService binds each exchanged token to its account, as the real service does
type provenanceClaudeOAuthService struct {
	targetedTestClaudeOAuthService
}

func (s *provenanceClaudeOAuthService) ExchangeCodeForTokens(ctx context.Context, code, state string, pkce *claude.PKCECodes) (*claude.ClaudeAuthBundle, error) {
	bundle, err := s.targetedTestClaudeOAuthService.ExchangeCodeForTokens(ctx, code, state, pkce)
	if err != nil {
		return nil, err
	}

	bundle.TokenData.OAuthProvenance, err = affinity.AcquiredOAuthProvenance("claude", bundle.TokenData.AccountUUID, bundle.TokenData.AccessToken)
	return bundle, err
}

func (s *provenanceClaudeOAuthService) CreateTokenStorage(bundle *claude.ClaudeAuthBundle) *claude.ClaudeTokenStorage {
	storage := s.targetedTestClaudeOAuthService.CreateTokenStorage(bundle)
	storage.OAuthProvenance = bundle.TokenData.OAuthProvenance
	return storage
}

// The quota poller skips a credential whose provenance does not match its access token, so a
// management re-login must replace the old token's provenance rather than merge it back in
func TestManagementClaudeReloginReplacesStaleOAuthProvenance(t *testing.T) {
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	svc := &provenanceClaudeOAuthService{targetedTestClaudeOAuthService{authDir: authDir, email: "a@x.test", accountUUID: "uuid-a", orgUUID: "org-a", accessToken: "claude-fresh"}}
	origClaude := newClaudeOAuthService
	newClaudeOAuthService = func(*config.Config, string) claudeOAuthService { return svc }
	t.Cleanup(func() { newClaudeOAuthService = origClaude })

	stale, errStale := affinity.AcquiredOAuthProvenance("claude", "uuid-a", "claude-expired")
	if errStale != nil {
		t.Fatalf("stale provenance: %v", errStale)
	}

	// round-trip through JSON so the registered metadata looks like a loaded auth file
	raw, _ := json.Marshal(map[string]any{"type": "claude", "email": "a@x.test", "account_uuid": "uuid-a", "access_token": "claude-expired", "refresh_token": "old", "oauth_provenance": stale})
	path := filepath.Join(authDir, "claude-custom.json")
	if errWrite := os.WriteFile(path, raw, 0o600); errWrite != nil {
		t.Fatalf("write auth file: %v", errWrite)
	}

	var meta map[string]any
	if errDecode := json.Unmarshal(raw, &meta); errDecode != nil {
		t.Fatalf("decode auth file: %v", errDecode)
	}

	target := &coreauth.Auth{ID: "claude-custom.json", FileName: "claude-custom.json", Provider: "claude", RequiresSignIn: true, Metadata: meta, Attributes: map[string]string{coreauth.AttributePath: path}}
	target.EnsureIndex()
	if _, errReg := manager.Register(context.Background(), target); errReg != nil {
		t.Fatalf("register: %v", errReg)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir, Port: 8317}, manager)
	router := gin.New()
	router.POST("/v8/management/oauth/auth-url", h.StartOAuthV8)
	router.GET("/v8/management/oauth/status", h.GetAuthStatus)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v8/management/oauth/auth-url", strings.NewReader(`{"auth_index":"`+target.Index+`"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)
	var start struct {
		State string `json:"state"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &start)
	if st := pollOAuthStatusUntilDone(t, router, start.State); st["status"] != "ok" || st["adopted"] != true {
		t.Fatalf("relogin status = %#v, want ok & adopted", st)
	}

	runtime, ok := manager.GetByID("claude-custom.json")
	if !ok {
		t.Fatal("relogged credential missing from manager")
	}

	provenance, errParse := affinity.ParseOAuthProvenance(runtime.Metadata["oauth_provenance"])
	if errParse != nil || !provenance.Matches("claude", "uuid-a", "claude-fresh") {
		t.Fatalf("runtime provenance = %#v (%v), want one bound to the new access token", runtime.Metadata["oauth_provenance"], errParse)
	}

	saved, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read auth file: %v", errRead)
	}

	var file map[string]any
	if errDecode := json.Unmarshal(saved, &file); errDecode != nil {
		t.Fatalf("decode saved auth file: %v", errDecode)
	}

	onDisk, errDisk := affinity.ParseOAuthProvenance(file["oauth_provenance"])
	if errDisk != nil || !onDisk.Matches("claude", "uuid-a", "claude-fresh") {
		t.Fatalf("saved provenance = %#v (%v), want one bound to the new access token", file["oauth_provenance"], errDisk)
	}
}

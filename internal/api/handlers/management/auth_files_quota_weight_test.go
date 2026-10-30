package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	fileauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestPatchAuthFileFields_QuotaWeightPersistsAndClears(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	authDir := t.TempDir()
	fileName := "claude-20x.json"
	filePath := filepath.Join(authDir, fileName)
	store := fileauth.NewFileTokenStore()
	store.SetBaseDir(authDir)
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:         fileName,
		FileName:   fileName,
		Provider:   "claude",
		Attributes: map[string]string{"path": filePath},
		Metadata:   map[string]any{"type": "claude"},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)

	patch := func(quotaWeight string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		body := `{"name":"claude-20x.json","quota_weight":` + quotaWeight + `}`
		ctx.Request = httptest.NewRequest(http.MethodPatch, "/v8/management/credentials/fields", strings.NewReader(body))
		ctx.Request.Header.Set("Content-Type", "application/json")
		h.PatchAuthFileFields(ctx)
		return rec
	}
	persisted := func() map[string]any {
		t.Helper()
		raw, errRead := os.ReadFile(filePath)
		if errRead != nil {
			t.Fatalf("ReadFile() error = %v", errRead)
		}
		var fields map[string]any
		if errUnmarshal := json.Unmarshal(raw, &fields); errUnmarshal != nil {
			t.Fatalf("Unmarshal() error = %v", errUnmarshal)
		}
		return fields
	}

	if rec := patch("1.7"); rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if updated, _ := manager.GetByID(fileName); updated.Metadata["quota_weight"] != 1.7 {
		t.Fatalf("runtime quota_weight = %#v, want 1.7", updated.Metadata["quota_weight"])
	}
	if got := persisted()["quota_weight"]; got != 1.7 {
		t.Fatalf("persisted quota_weight = %#v, want 1.7", got)
	}

	for _, invalid := range []string{"0", "-1", "1001", `"2"`} {
		if rec := patch(invalid); rec.Code != http.StatusBadRequest {
			t.Fatalf("quota_weight %s status = %d, want 400; body=%s", invalid, rec.Code, rec.Body.String())
		}
	}

	if rec := patch("null"); rec.Code != http.StatusOK {
		t.Fatalf("reset status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if _, exists := persisted()["quota_weight"]; exists {
		t.Fatal("persisted quota_weight remains after reset")
	}
}

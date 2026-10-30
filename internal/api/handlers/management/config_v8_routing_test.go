package management

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestConfigV8UploadKeepsSessionAffinityForced(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := "server: {port: 8317}\nrouting: {strategy: fill-first, session-affinity: false}\n"
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: cfg, configFilePath: path}
	router := gin.New()
	router.PUT("/v8/management/config.yaml", h.ConfigV8)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, "/v8/management/config.yaml", strings.NewReader("server: {port: 8318}\nrouting: {strategy: fill-first, session-affinity: false}\n")))
	if recorder.Code != http.StatusOK {
		t.Fatalf("upload disabling session affinity: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = config.ValidateV8Config(saved); err != nil {
		t.Fatalf("saved config is invalid: %v\n%s", err, saved)
	}
	if loaded, errLoad := config.LoadConfig(path); errLoad != nil || loaded.Port != 8318 || loaded.Routing.Strategy != "fill-first" || !loaded.Routing.SessionAffinity {
		t.Fatalf("upload lost settings: cfg=%+v error=%v", loaded, errLoad)
	}
}

func TestConfigV8UploadValidatesRoutingOrder(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("server: {port: 8317}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: cfg, configFilePath: path}
	router := gin.New()
	router.PUT("/v8/management/config.yaml", h.ConfigV8)
	upload := func(body string) int {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, "/v8/management/config.yaml", strings.NewReader(body)))
		return recorder.Code
	}

	if code := upload("routing: {strategy: ordered, order: [priority, bogus]}\n"); code == http.StatusOK {
		t.Fatal("upload with an unknown criterion was accepted")
	}
	if code := upload("routing: {strategy: ordered, order: [session-remaining, Weekly-Reset]}\n"); code != http.StatusOK {
		t.Fatalf("valid order rejected: status=%d", code)
	}
	loaded, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	order, err := config.NormalizeRoutingOrder(loaded.Routing.Order)
	if err != nil || loaded.Routing.Strategy != config.RoutingStrategyOrdered || strings.Join(order, ",") != "session-remaining,weekly-reset" {
		t.Fatalf("saved routing = %+v (order %v, %v)", loaded.Routing, order, err)
	}
}

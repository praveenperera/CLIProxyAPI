package cliproxy

import (
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chathistory"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	log "github.com/sirupsen/logrus"
)

// startChatHistory starts capture when chat-history is enabled. A misconfigured
// block is logged rather than fatal: losing history must not stop the proxy
func startChatHistory(cfg *config.Config) {
	if cfg == nil || cfg.ChatHistory == nil || !cfg.ChatHistory.Enabled {
		return
	}

	settings := cfg.ChatHistory
	passwordPath, errPath := util.ResolveAuthDir(strings.TrimSpace(settings.PasswordFile))
	spoolDir, errSpool := util.ResolveAuthDir(strings.TrimSpace(settings.SpoolDir))
	if errPath != nil || errSpool != nil {
		log.Errorf("chat-history disabled: resolve paths: %v %v", errPath, errSpool)
		return
	}

	password, errRead := os.ReadFile(passwordPath)
	if errRead != nil {
		log.Errorf("chat-history disabled: read password-file: %v", errRead)
		return
	}

	maxSpoolMB := settings.MaxSpoolMB
	if maxSpoolMB == 0 {
		maxSpoolMB = 5120
	}

	recorder, errRecorder := chathistory.NewRecorder(chathistory.Options{
		SpoolDir:      spoolDir,
		Endpoint:      strings.TrimSpace(settings.Endpoint),
		Database:      settings.Database,
		User:          strings.TrimSpace(settings.User),
		Password:      strings.TrimSpace(string(password)),
		MaxSpoolBytes: maxSpoolMB << 20,
		FlushInterval: time.Duration(settings.FlushSeconds) * time.Second,
	})
	if errRecorder != nil {
		log.Errorf("chat-history disabled: %v", errRecorder)
		return
	}

	if previous := chathistory.SetRecorder(recorder); previous != nil {
		previous.Close()
	}

	log.Infof("chat-history: recording to %s, spooling in %s", settings.Endpoint, spoolDir)
}

// stopChatHistory flushes queued calls to the spool and makes a last upload attempt
func stopChatHistory() {
	if previous := chathistory.SetRecorder(nil); previous != nil {
		previous.Close()
	}
}

// warnChatHistoryReload notes that chat-history changes wait for a restart, since
// swapping the recorder under live streams would split their calls across two
func warnChatHistoryReload(previous, next *config.Config) {
	if previous == nil || next == nil {
		return
	}

	if !reflect.DeepEqual(previous.ChatHistory, next.ChatHistory) {
		log.Warn("chat-history settings changed; restart to apply them")
	}
}

package auth

import (
	"bytes"
	"context"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

// cacheUsageTitleMaxRunes bounds a session title; the full prompt is never kept
const cacheUsageTitleMaxRunes = 120

// claudeCodeWorkingDirPrefix starts the line Claude Code puts in its system prompt
var claudeCodeWorkingDirPrefix = []byte("Primary working directory: ")

// workingDirMaxBytes bounds the line read after a working directory marker; real paths are far
// shorter, and the bound keeps a payload that repeats the marker from rescanning its whole tail
// for every repeat
const workingDirMaxBytes = 1000

var (
	// codexCwdPattern matches the cwd Codex sends in its environment context, up to
	// workingDirMaxBytes long
	codexCwdPattern = regexp.MustCompile(`<cwd>([^<]{1,1000})</cwd>`)
	// inlineReminderPattern matches reminders clients splice into otherwise real prompt text
	inlineReminderPattern = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)
	// wrappedBlockPattern matches a text block that is one client-injected tag, such as a
	// system reminder or Codex environment context, rather than something the user typed
	wrappedBlockPattern = regexp.MustCompile(`(?s)^<([a-zA-Z_][\w-]*)[^>]*>.*</([a-zA-Z_][\w-]*)>$`)
	whitespacePattern   = regexp.MustCompile(`\s+`)
)

// injectedPromptPrefixes start user turns that clients write themselves: Codex's AGENTS.md
// instructions and Claude Code's caveat ahead of local command output
var injectedPromptPrefixes = []string{"# AGENTS.md instructions for ", "Caveat: "}

// noteCacheUsageLabel names the request's session in the cache usage report the first time it
// is seen, from the first user prompt and the client's working directory
func (m *Manager) noteCacheUsageLabel(ctx context.Context, payload []byte) {
	if m == nil || m.cacheUsage == nil || len(payload) == 0 {
		return
	}
	sessionID := strings.TrimSpace(logging.GetClientRequestMetadata(ctx).SessionID)
	if sessionID == "" || m.cacheUsage.hasLabel(sessionID) {
		return
	}
	// an empty label is still stored, so a session with no prompt text is parsed only once
	title, project := extractSessionLabel(payload)
	m.cacheUsage.noteLabel(sessionID, title, project)
}

// requestPayload is the inbound request before translation, which keeps the client's own
// message layout
func requestPayload(req exec.Request, opts exec.Options) []byte {
	if len(opts.OriginalRequest) > 0 {
		return opts.OriginalRequest
	}
	return req.Payload
}

// extractSessionLabel reads a short title from the first user message and the project name
// from the working directory, for Claude Messages, OpenAI Chat, and Responses payloads
func extractSessionLabel(payload []byte) (title, project string) {
	return firstUserPrompt(payload), workingDirName(payload)
}

func firstUserPrompt(payload []byte) string {
	var title string
	for _, field := range []string{"messages", "input"} {
		gjson.GetBytes(payload, field).ForEach(func(_, message gjson.Result) bool {
			if message.Get("role").String() != "user" {
				return true
			}
			title = promptText(message.Get("content"))
			// a user turn that is only injected context does not name the session
			return title == ""
		})
		if title != "" {
			return title
		}
	}
	return ""
}

// promptText returns the first text the user typed in a message's content
func promptText(content gjson.Result) string {
	if content.Type == gjson.String {
		return cleanPrompt(content.String())
	}
	var text string
	content.ForEach(func(_, part gjson.Result) bool {
		switch part.Get("type").String() {
		case "text", "input_text":
			text = cleanPrompt(part.Get("text").String())
		}
		return text == ""
	})
	return text
}

func cleanPrompt(text string) string {
	text = strings.TrimSpace(inlineReminderPattern.ReplaceAllString(text, ""))
	if match := wrappedBlockPattern.FindStringSubmatch(text); match != nil && match[1] == match[2] {
		return ""
	}
	// slash commands arrive as tags only, such as <command-name> through <command-args>
	if strings.HasPrefix(text, "<") && strings.HasSuffix(text, ">") {
		return ""
	}
	for _, prefix := range injectedPromptPrefixes {
		if strings.HasPrefix(text, prefix) {
			return ""
		}
	}
	text = whitespacePattern.ReplaceAllString(text, " ")
	if utf8.RuneCountInString(text) <= cacheUsageTitleMaxRunes {
		return text
	}
	runes := []rune(text)
	return strings.TrimSpace(string(runes[:cacheUsageTitleMaxRunes-1])) + "…"
}

// workingDirName returns the last element of the client's working directory. The system
// prompt is searched first, because conversation text can quote the same markers, as the
// summary of a compacted session does; Codex sends its cwd in a user turn
func workingDirName(payload []byte) string {
	for _, field := range []string{"system", "instructions"} {
		if dir := findWorkingDir([]byte(gjson.GetBytes(payload, field).Raw)); dir != "" {
			return dir
		}
	}
	return findWorkingDir(payload)
}

// findWorkingDir returns the first working directory in raw JSON text that is an absolute path
func findWorkingDir(raw []byte) string {
	for rest := raw; ; {
		i := bytes.Index(rest, claudeCodeWorkingDirPrefix)
		if i < 0 {
			break
		}
		rest = rest[i+len(claudeCodeWorkingDirPrefix):]
		line := rest[:min(len(rest), workingDirMaxBytes)]
		// the prefix sits inside a JSON string, so the line ends at an escaped newline or the quote
		for _, sep := range [][]byte{[]byte(`\n`), []byte(`"`)} {
			if j := bytes.Index(line, sep); j >= 0 {
				line = line[:j]
			}
		}
		if dir := absoluteDirName(line); dir != "" {
			return dir
		}
	}
	for rest := raw; ; {
		match := codexCwdPattern.FindSubmatchIndex(rest)
		if match == nil {
			return ""
		}
		if dir := absoluteDirName(rest[match[2]:match[3]]); dir != "" {
			return dir
		}
		rest = rest[match[1]:]
	}
}

// absoluteDirName returns the last element of an absolute or home-relative path, or "" for
// anything else, such as a marker quoted in conversation text
func absoluteDirName(raw []byte) string {
	dir := strings.TrimRight(strings.TrimSpace(string(raw)), "/")
	if strings.HasPrefix(dir, "/") || strings.HasPrefix(dir, "~/") {
		return path.Base(dir)
	}
	return ""
}

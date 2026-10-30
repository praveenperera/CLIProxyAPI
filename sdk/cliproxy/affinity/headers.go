package affinity

import (
	"net/http"
	"strings"
	"time"
)

const (
	// ChatIDHeader carries an explicit local chat identity
	ChatIDHeader = "X-CLIProxy-Chat-ID"
	// ChatIntentHeader carries start or continue; absence means continue
	ChatIntentHeader = "X-CLIProxy-Chat-Intent"
	// ChatHandleHeader carries the local continuation capability
	ChatHandleHeader = "X-CLIProxy-Chat-Handle"
)

// IsLocalHeader identifies local-only affinity headers, including unknown extensions
func IsLocalHeader(name string) bool {
	return strings.HasPrefix(strings.ToLower(name), "x-cliproxy-chat-")
}

// StripHeaders returns a private header copy without local affinity headers
func StripHeaders(headers http.Header) http.Header {
	out := headers.Clone()
	for name := range out {
		if IsLocalHeader(name) {
			delete(out, name)
		}
	}
	return out
}

// ResponseHeaders attaches only the saved local continuation handle
func ResponseHeaders(headers http.Header, handle Handle) http.Header {
	out := StripHeaders(headers)
	if out == nil {
		out = make(http.Header)
	}
	out.Set(ChatHandleHeader, handle.Encode())
	return out
}

// ParseHeaders validates lifecycle input with the authenticated host caller scope
// It does not read caller identity from a client header or infer a chat from a payload
func ParseHeaders(namespace string, caller CallerScope, headers http.Header) (Request, error) {
	values := make(map[string]string)
	for name, items := range headers {
		if !IsLocalHeader(name) {
			continue
		}
		canonical := ""
		for _, known := range []string{ChatIDHeader, ChatIntentHeader, ChatHandleHeader} {
			if strings.EqualFold(name, known) {
				canonical = known
				break
			}
		}
		if canonical == "" {
			return Request{}, NewError(IdentityInvalid, time.Time{})
		}
		if len(items) != 1 || items[0] == "" || values[canonical] != "" {
			return Request{}, NewError(IdentityInvalid, time.Time{})
		}
		values[canonical] = items[0]
	}
	if len(values) == 0 {
		if namespace != "" {
			return Request{}, NewError(IdentityRequired, time.Time{})
		}
		return Request{}, nil
	}
	if caller.value == "" {
		return Request{}, NewError(IdentityRequired, time.Time{})
	}
	id, err := NewChatID(values[ChatIDHeader])
	if err != nil {
		return Request{}, NewError(IdentityInvalid, time.Time{})
	}
	key, err := NewChatKey(namespace, caller, id)
	if err != nil {
		return Request{}, NewError(RouteUnsupported, time.Time{})
	}
	switch values[ChatIntentHeader] {
	case "start":
		if values[ChatHandleHeader] != "" {
			return Request{}, NewError(BindingConflict, time.Time{})
		}
		return Start(key), nil
	case "", "continue":
		h, err := ParseHandle(values[ChatHandleHeader])
		if err != nil {
			return Request{}, NewError(IdentityInvalid, time.Time{})
		}
		return Continue(key, h), nil
	default:
		return Request{}, NewError(IdentityInvalid, time.Time{})
	}
}

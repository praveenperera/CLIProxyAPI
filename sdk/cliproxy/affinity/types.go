// Package affinity defines validated local chat identities and durable bindings
package affinity

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// CallerScope identifies a caller authenticated by the embedding host
// A client-supplied identity is not a trusted caller scope
type CallerScope struct{ value string }

// ChatID identifies one chat, independently of model and provider
type ChatID struct{ value string }

// ChatKey isolates a chat by deployment namespace and authenticated caller
type ChatKey struct{ digest [32]byte }

// Handle is a random local continuation capability, not a provider credential
type Handle struct{ value [32]byte }

// CredentialRef identifies an auth record and its verified immutable lineage
type CredentialRef struct {
	provider, authID string
	lineage          [32]byte
}

// Intent identifies a sealed request variant
type Intent uint8

const (
	// Legacy leaves routing unchanged
	Legacy Intent = iota
	// StartIntent asserts a new chat if no live binding exists
	StartIntent
	// ContinueIntent requires the saved binding and handle
	ContinueIntent
)

// Request contains immutable host-validated lifecycle input
// Its zero value selects legacy routing
type Request struct {
	intent Intent
	key    ChatKey
	handle Handle
}

// Binding is a validated, versioned durable credential assignment
type Binding struct {
	key                       ChatKey
	handle                    Handle
	ref                       CredentialRef
	created, touched, expires time.Time
}

// SchemaVersion is the only supported binding record version
const SchemaVersion uint32 = 1

func validText(s string, max int) bool {
	if s == "" || len(s) > max || !utf8.ValidString(s) || strings.TrimSpace(s) != s {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// NewCallerScope validates an identity supplied by the trusted host
func NewCallerScope(value string) (CallerScope, error) {
	if !validText(value, 256) {
		return CallerScope{}, errors.New("invalid caller scope")
	}
	return CallerScope{value}, nil
}

// NewChatID validates an explicit chat ID without inferring or rewriting it
func NewChatID(value string) (ChatID, error) {
	if !validText(value, 256) {
		return ChatID{}, errors.New("invalid chat ID")
	}
	return ChatID{value}, nil
}

// NewChatKey hashes the complete versioned, length-delimited identity tuple
func NewChatKey(namespace string, caller CallerScope, id ChatID) (ChatKey, error) {
	if !validText(namespace, 256) || caller.value == "" || id.value == "" {
		return ChatKey{}, errors.New("invalid chat key")
	}
	h := sha256.New()
	h.Write([]byte("cliproxy/chat-key/v1"))
	for _, part := range []string{namespace, caller.value, id.value} {
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(part)))
		h.Write(size[:])
		h.Write([]byte(part))
	}
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	return ChatKey{digest}, nil
}

// Digest returns the namespace-isolated record key
func (k ChatKey) Digest() [32]byte { return k.digest }

// NewHandle generates a cryptographically random continuation handle
func NewHandle() (Handle, error) { var h Handle; _, err := rand.Read(h.value[:]); return h, err }

// ParseHandle validates the canonical base64url representation
func ParseHandle(value string) (Handle, error) {
	var h Handle
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(data) != 32 || base64.RawURLEncoding.EncodeToString(data) != value {
		return h, errors.New("invalid chat handle")
	}
	copy(h.value[:], data)
	if h.value == ([32]byte{}) {
		return Handle{}, errors.New("invalid chat handle")
	}
	return h, nil
}

// Encode returns the handle for a local authenticated response, never an upstream header
func (h Handle) Encode() string { return base64.RawURLEncoding.EncodeToString(h.value[:]) }

// String redacts the continuation capability in ordinary diagnostic formatting
func (h Handle) String() string { return "[chat handle]" }

// GoString redacts the continuation capability in Go-syntax diagnostic formatting
func (h Handle) GoString() string { return "affinity.Handle{redacted}" }

// Start constructs a start request from a validated key
func Start(key ChatKey) Request { return Request{intent: StartIntent, key: key} }

// Continue constructs a continuation from a validated key and saved handle
func Continue(key ChatKey, handle Handle) Request {
	return Request{intent: ContinueIntent, key: key, handle: handle}
}

// Intent returns the request lifecycle
func (r Request) Intent() Intent { return r.intent }

// Key returns the immutable chat key
func (r Request) Key() ChatKey { return r.key }

// Handle returns the saved local continuation capability
func (r Request) Handle() Handle { return r.handle }

// Validate rejects zero identity values embedded in a strict variant
func (r Request) Validate() error {
	if r.intent == Legacy {
		return nil
	}
	if r.intent != StartIntent && r.intent != ContinueIntent || r.key.digest == ([32]byte{}) {
		return errors.New("invalid affinity request")
	}
	if r.intent == ContinueIntent && r.handle.value == ([32]byte{}) {
		return errors.New("invalid affinity handle")
	}
	return nil
}

// NewCredentialRef validates a reference supplied by the credential owner
// The caller must first verify provider identity provenance; this function does not establish it
func NewCredentialRef(provider, authID string, lineage [32]byte) (CredentialRef, error) {
	if provider != "claude" && provider != "codex" || !validText(authID, 512) || lineage == ([32]byte{}) {
		return CredentialRef{}, errors.New("invalid credential reference")
	}
	return CredentialRef{provider, authID, lineage}, nil
}

// Provider returns the execution authority
func (r CredentialRef) Provider() string { return r.provider }

// AuthID returns the token-store record ID
func (r CredentialRef) AuthID() string { return r.authID }

// Lineage returns the verified principal or deployment-key HMAC digest
func (r CredentialRef) Lineage() [32]byte { return r.lineage }

// NewBinding validates a complete durable assignment
func NewBinding(key ChatKey, handle Handle, ref CredentialRef, created, touched, expires time.Time) (Binding, error) {
	if key.digest == ([32]byte{}) || handle.value == ([32]byte{}) || ref.lineage == ([32]byte{}) || ref.authID == "" || (ref.provider != "claude" && ref.provider != "codex") || created.IsZero() || touched.Before(created) || !expires.After(touched) || created.Year() < 1970 || expires.Year() > 9999 {
		return Binding{}, errors.New("invalid binding record")
	}
	return Binding{key, handle, ref, created.UTC(), touched.UTC(), expires.UTC()}, nil
}

// Key returns the durable record key
func (b Binding) Key() ChatKey { return b.key }

// Handle returns the authenticated continuation capability
func (b Binding) Handle() Handle { return b.handle }

// Credential returns the immutable execution assignment
func (b Binding) Credential() CredentialRef { return b.ref }

// CreatedAt returns the hard-retention origin
func (b Binding) CreatedAt() time.Time { return b.created }

// TouchedAt returns the last durable continuation time
func (b Binding) TouchedAt() time.Time { return b.touched }

// ExpiresAt returns the earlier of idle and hard expiration
func (b Binding) ExpiresAt() time.Time { return b.expires }

// String hides the local continuation capability in request diagnostics
func (r Request) String() string { return "[chat affinity request]" }

// GoString hides the local continuation capability in Go diagnostics
func (r Request) GoString() string { return "affinity.Request{redacted}" }

// String hides the local continuation capability in binding diagnostics
func (b Binding) String() string { return "[chat binding]" }

// GoString hides the local continuation capability in Go diagnostics
func (b Binding) GoString() string { return "affinity.Binding{redacted}" }

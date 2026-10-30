package affinity

import "time"

// UnavailableReason is a closed set of local request-terminal failures
type UnavailableReason uint8

const (
	// IdentityRequired reports absent authenticated identity
	IdentityRequired UnavailableReason = iota + 1
	// IdentityInvalid reports malformed lifecycle input
	IdentityInvalid
	// BindingConflict reports a wrong handle or conflicting pin
	BindingConflict
	// BindingMissing reports a continuation with no saved record
	BindingMissing
	// BindingExpired reports a continuation past retention
	BindingExpired
	// Busy reports a turn already in flight
	Busy
	// CredentialMissing reports a deleted credential
	CredentialMissing
	// CredentialDisabled reports a disabled credential
	CredentialDisabled
	// CredentialChanged reports an immutable lineage mismatch
	CredentialChanged
	// CredentialCooldown reports an active availability cooldown
	CredentialCooldown
	// QuotaDenied reports a fresh applicable denial
	QuotaDenied
	// ModelUnsupported reports an unavailable model on the saved credential
	ModelUnsupported
	// StoreUnavailable reports an unreadable or uncommitted durable store
	StoreUnavailable
	// StoreFull reports capacity without live eviction
	StoreFull
	// RouteUnsupported reports an unsupported execution authority
	RouteUnsupported
	// PrefixTransformUnsupported reports an incompatible history transformation
	PrefixTransformUnsupported
)

var reasonNames = [...]string{"", "identity_required", "identity_invalid", "binding_conflict", "binding_missing", "binding_expired", "busy", "credential_missing", "credential_disabled", "credential_changed", "credential_cooldown", "quota_denied", "model_unsupported", "store_unavailable", "store_full", "route_unsupported", "prefix_transform_unsupported"}

// Error is a sanitized request-terminal affinity error
type Error struct {
	reason UnavailableReason
	retry  time.Time
}

// NewError constructs a bounded local error; invalid reasons fail closed as store errors
func NewError(reason UnavailableReason, retry time.Time) *Error {
	if reason < IdentityRequired || reason > PrefixTransformUnsupported {
		reason = StoreUnavailable
	}
	if !retry.IsZero() && (retry.Year() < 1970 || retry.Year() > 9999) {
		reason = StoreUnavailable
	}
	if reason != CredentialCooldown && reason != QuotaDenied {
		retry = time.Time{}
	}
	return &Error{reason, retry.UTC()}
}

// Error returns a bounded reason without caller, handle, or credential contents
func (e *Error) Error() string { return "chat_" + reasonNames[e.reason] }

// Reason returns the closed failure reason
func (e *Error) Reason() UnavailableReason { return e.reason }

// RetryAt returns an optional reliable bound-credential deadline
func (e *Error) RetryAt() time.Time { return e.retry }

// StatusCode supplies the local HTTP status independently of upstream adapters
func (e *Error) StatusCode() int {
	switch e.reason {
	case IdentityRequired, IdentityInvalid, RouteUnsupported, PrefixTransformUnsupported:
		return 400
	case BindingConflict, Busy:
		return 409
	case BindingMissing, BindingExpired:
		return 410
	default:
		return 503
	}
}

// IsRequestScoped prevents local errors from entering credential failover
func (e *Error) IsRequestScoped() bool { return true }

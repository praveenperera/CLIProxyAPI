package affinity

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
)

// OAuthProvenance attests an identity obtained at an authenticated acquisition boundary
// It binds that identity to the exact acquired token, without retaining the token
// It does not validate arbitrary JWT signatures or turn synthesized IDs into principals
type OAuthProvenance struct {
	provider, principal string
	tokenDigest         [32]byte
}

type oauthProvenanceRecord struct {
	Version     uint32 `json:"version"`
	Provider    string `json:"provider"`
	Principal   string `json:"principal"`
	TokenDigest string `json:"token_digest"`
}

// AcquiredOAuthProvenance records an identity from a real profile or OAuth token response
// Only the trusted acquisition owner can establish this source assertion
func AcquiredOAuthProvenance(provider, principal, token string) (OAuthProvenance, error) {
	if (provider != "claude" && provider != "codex") || !validText(principal, 256) || token == "" {
		return OAuthProvenance{}, errors.New("unverified OAuth acquisition")
	}
	return OAuthProvenance{provider, principal, sha256.Sum256([]byte(token))}, nil
}

// Matches verifies that a runtime token and its account still belong to the acquired identity
func (p OAuthProvenance) Matches(provider, principal, token string) bool {
	return token != "" && p.provider == provider && p.principal == principal && p.tokenDigest == sha256.Sum256([]byte(token))
}

// MarshalJSON serializes only the bounded identity assertion and token digest
func (p OAuthProvenance) MarshalJSON() ([]byte, error) {
	if p.provider == "" && p.principal == "" && p.tokenDigest == ([32]byte{}) {
		return []byte("null"), nil
	}
	return json.Marshal(oauthProvenanceRecord{1, p.provider, p.principal, hex.EncodeToString(p.tokenDigest[:])})
}

// UnmarshalJSON validates a persisted acquisition assertion without accepting tokens
func (p *OAuthProvenance) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		*p = OAuthProvenance{}
		return nil
	}
	if len(data) > 1024 {
		return errors.New("invalid OAuth provenance")
	}
	var record oauthProvenanceRecord
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&record) != nil || record.Version != 1 || (record.Provider != "claude" && record.Provider != "codex") || !validText(record.Principal, 256) {
		return errors.New("invalid OAuth provenance")
	}
	digest, err := hex.DecodeString(record.TokenDigest)
	if err != nil || len(digest) != 32 || hex.EncodeToString(digest) != record.TokenDigest {
		return errors.New("invalid OAuth provenance")
	}
	var value [32]byte
	for i := range value {
		value[i] = digest[i]
	}
	if value == ([32]byte{}) {
		return errors.New("invalid OAuth provenance")
	}
	*p = OAuthProvenance{record.Provider, record.Principal, value}
	return nil
}

// ParseOAuthProvenance validates either an immutable acquisition value or a decoded file record
func ParseOAuthProvenance(value any) (OAuthProvenance, error) {
	if p, ok := value.(OAuthProvenance); ok {
		return p, nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return OAuthProvenance{}, errors.New("invalid OAuth provenance")
	}
	var p OAuthProvenance
	if err = json.Unmarshal(data, &p); err != nil {
		return p, errors.New("invalid OAuth provenance")
	}
	return p, nil
}

package management

import (
	"encoding/json"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// applyQuotaWeightPatch applies a quota_weight field patch to the auth metadata
// and reports whether fieldPath was a quota_weight path; callers must skip the
// generic field handling when it returns true without an error
func applyQuotaWeightPatch(targetAuth *coreauth.Auth, fieldPath string, value any) (bool, error) {
	if fieldPath != "quota_weight" {
		if rootAuthFileField(fieldPath) == "quota_weight" {
			return true, &authFileFieldPatchError{message: "quota_weight does not support nested fields"}
		}
		return false, nil
	}

	// null restores the default of 1 by removing the field, like weight
	if value == nil {
		delete(targetAuth.Metadata, "quota_weight")
		return true, nil
	}
	number, okNumber := value.(json.Number)
	quotaWeight, errQuotaWeight := number.Float64()
	if !okNumber || errQuotaWeight != nil || quotaWeight <= 0 || quotaWeight > 1000 {
		return true, &authFileFieldPatchError{message: "quota_weight must be a number above 0 and at most 1000"}
	}
	targetAuth.Metadata["quota_weight"] = quotaWeight
	return true, nil
}

package usage

// InputTransformationCounts contains bounded counts of provider-reported thinking drops
// These observations do not prove local or provider signature validity
type InputTransformationCounts struct {
	PrefixBindingMismatch       uint32 `json:"prefix_binding_mismatch,omitempty"`
	ModelBindingMismatch        uint32 `json:"model_binding_mismatch,omitempty"`
	OrganizationBindingMismatch uint32 `json:"organization_binding_mismatch,omitempty"`
	Unknown                     uint32 `json:"unknown,omitempty"`
}

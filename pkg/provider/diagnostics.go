package provider

// Diagnostic codes for the external-provider lifecycle. They are prefixed
// external_provider. to stay clear of the existing secret-provider provider_*
// codes emitted by `codefly doctor workspace`. Automation matches on these
// strings, never on message prose — adding a code is safe, renaming or removing
// one is a breaking change to the JSON contract.
const (
	CodeBindingsUnreadable     = "external_provider.bindings_unreadable"
	CodeBindingsSchemaUnknown  = "external_provider.bindings_schema_unknown"
	CodeBindingNotFound        = "external_provider.binding_not_found"
	CodeInvalidIdentity        = "external_provider.invalid_identity"
	CodeInvalidMode            = "external_provider.invalid_mode"
	CodeInvalidDeletionPolicy  = "external_provider.invalid_deletion_policy"
	CodeObserveMutation        = "external_provider.observe_mode_mutation"
	CodeSecretInSpec           = "external_provider.secret_in_spec"
	CodeSecretInPublicInput    = "external_provider.secret_in_public_input"
	CodeUndeclaredContract     = "external_provider.undeclared_output_contract"
	CodeLiteralEndpoint        = "external_provider.literal_endpoint_reference"
	CodeInvalidSecretRef       = "external_provider.invalid_secret_reference"
	CodeCoordinatorUnavailable = "external_provider.coordinator_unavailable"
)

// Diagnostic is one machine-readable finding about a binding or operation. It
// never carries a secret value or a raw provider body.
type Diagnostic struct {
	Code    string `json:"code"`
	Binding string `json:"binding,omitempty"`
	Message string `json:"message"`
}

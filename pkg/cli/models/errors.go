package models

import "errors"

// ErrPromptCancelled reports an explicit Ctrl+C from an interactive prompt.
// Callers decide whether cancellation is a clean no-op or a propagated error;
// prompt helpers never terminate the process themselves.
var ErrPromptCancelled = errors.New("prompt cancelled")

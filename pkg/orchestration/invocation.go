package orchestration

import (
	"crypto/rand"
	"encoding/base32"
	"strings"
)

// invocationIDAlphabet is unpadded, lowercase base32: the identity is folded
// into container names, DNS-ish resource names and log lines, so it must stay
// within the lowest common denominator of what those accept.
var invocationIDAlphabet = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewInvocationID returns a fresh identity for one disposable flow. It is
// generated from cryptographic randomness rather than derived from the
// workspace, the checkout path or a caller-supplied label: two independent test
// package processes in one workspace, and two worktrees of the same workspace,
// must not be able to reproduce each other's identity.
func NewInvocationID() string {
	var raw [5]byte
	// crypto/rand.Read fills the buffer completely or crashes the program; it
	// cannot return a short read.
	_, _ = rand.Read(raw[:])
	return "inv" + strings.ToLower(invocationIDAlphabet.EncodeToString(raw[:]))
}

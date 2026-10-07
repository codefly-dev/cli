// Package signing signs and verifies the canonical bytes of delivery documents
// (presence and authority documents) with Sigstore, keylessly.
//
// # Identity, not keys
//
// A delivery document is trusted because of who produced it, never because of
// which key happened to be at hand. A release is signed only by the owning
// repository's release workflow: the workflow presents its GitHub Actions OIDC
// identity to Fulcio, receives a short-lived certificate for an ephemeral key
// that lives for the duration of one Sign call, signs the document, and records
// the signature in the Rekor transparency log. The key is then discarded; the
// Sigstore bundle carries the certificate, the signature and the log entry.
//
// Verification pins an allowlist, a Policy: the OIDC issuer, and a pattern the
// certificate's subject (for GitHub Actions the repository, the workflow path
// and the git ref) must match in full. Nothing in that policy is a public key,
// so rotation never means redistributing keys; a change of trust reduces to a
// trusted-root update, and a disconnected perimeter verifies offline from the
// bundle against a mirrored trusted root (LoadTrustedRoot). The transparency
// log entry, or a signed timestamp, establishes when the signature was made,
// which is what lets a certificate valid for ten minutes vouch for a document
// for years. The bundle carries the log entry whole — the signed entry
// timestamp and the inclusion proof with its checkpoint — so verifying it
// needs the log's key and never the log; a bundle carrying no such evidence is
// refused by its own name (ErrNoTransparency), because a signer configured
// without a log is fixed at the signer and a bad signature is investigated,
// and the two must not reach an operator looking alike.
//
// Consequently this package holds no key material, offers no option to sign
// with a private key, and never reads a key from disk or the environment. The
// codefly binary cannot sign a release from a laptop: a process without a CI
// OIDC identity gets an Unavailable signer, whose Sign returns ErrNoIdentity
// naming the release workflow as the only signer, rather than a hint to supply
// a key.
package signing

// Package signing supplies custody for platform-owned delivery keys. Core's
// solutionhost package owns the signed envelope and its verification; this
// package supplies the key that approves the settled document.
//
// A release key stays in the platform's Vault Transit. A developer key stays
// on that developer's machine and signs only for a declared local cluster.
// The host pins the public half and the ownership domains it may speak for.
// No third party certifies, logs or approves a delivery: custody and admission
// belong to the platform that operates it. This is the handbook decision
// delivery-signed-with-platform-owned-keys, expressed at the publishing boundary.
package signing

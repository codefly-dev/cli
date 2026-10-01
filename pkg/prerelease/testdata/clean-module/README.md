Reproduces the four modules that must PASS — obin-ai/module-robin v0.1.7,
module-annotations v0.2.9, module-document-store v0.0.24, module-model-gateway v0.1.12 — in
the shape they actually have: every codefly agent pin is a released version, and the Go
trees carry first-party pseudo-versions anyway, direct and indirect.

That combination is the measurement behind the go.mod policy. Three of those four releases
carry first-party pseudo-versions, so a gate that refused them by default would fail three
of its own clean fixtures, and would be switched off in a week.

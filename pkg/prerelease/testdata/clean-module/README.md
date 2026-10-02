A module repository that must PASS: every codefly agent pin is a released version, while the Go
trees carry first-party pseudo-versions anyway, direct and indirect.

That combination is the measurement behind the go.mod policy. Of four module releases verified to
have correct agent pins, three carried first-party pseudo-versions — 27, 9 and 3 of them — so a
gate that refused them by default would fail three of its own clean fixtures, and would be
switched off in a week.

The module paths use an invented owner, so the fixture also exercises the derivation: first-party
is whatever owner the repository's own go.mod files publish under, never a list the CLI carries.

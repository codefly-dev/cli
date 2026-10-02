A composition's workspace.codefly.yaml carrying four agent-overrides dev pins, of which two are
labelled with the issue they stand in for and two are not, plus one composed module and one
solution pinned at a prerelease.

On the default branch the two labelled overrides are permitted and the two unlabelled ones
refused. Under --release all four are refused, which is the "drop every dev override before
tagging" step a release procedure can ask for but not enforce.

Modelled on a real composition's block, with its names replaced: the gate recognises the carrier
by the shape of the declaration, never by who wrote it.

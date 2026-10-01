Reproduces obin-ai/platform-obin's workspace.codefly.yaml as main carries it: four
agent-overrides dev pins, of which two are labelled with the issue they stand in for and two
are not, plus one composed module pinned at a prerelease.

On the default branch the two labelled overrides are permitted and the two unlabelled ones
refused. Under --release all four are refused, which is the "drop every dev override before
tagging" step of docs/release.md that nothing enforced.

# Core owner acceptance: bounded local process result v2

Accepted for exact reviewed CLI cmd/recover_process.go SHA256 52f7b993d3a3e52eea832f22070d351ca74373071875b601f9967621596b33d3 and docs/commands.md SHA256 060dc17f96e0619591d3964afba2c5414404c1c9283bc8c29a3a8b2fe16afb7d, following full reading of /tmp/cli934-graceful-recovery-review.md and the current source/guide.

This is the explicit additional exception required by the prior fixed-local-result contract: permit fixed schema_version2 processExitResult containing schema_version, pid, mode, signal, signal_attempted, signal_accepted, observation, expected_birth_gone. Mode and signal describe the REQUEST, not proof of delivery/exit/persistence. The native ProcessIdentity preview receipt remains canonical fixed schema_version1 unchanged; no registry/Docker/remote protocol exception follows. Future result or receipt evolution still requires explicit owner review or the protobuf default. Keep maintained exact-shape assertions for both independent versions.

The same reviewed implementation/host/boot and fresh-preview requirement remains. Unchanged receipt schema does not authorize reuse of a preview made with another binary. CLI owns explicit graceful/force policy, Core SignalProcess owns identity-bound delivery. One SIGTERM owner call for graceful, no escalation/group/reap/registry operation, preserved unknown/error outcomes. Accepted result v2 is intentionally distinguishable from force-only result v1; no compatibility fallback is required.

Small documentation precision: replace the current exception paragraph's ambiguous “Version1 ... and uses a fixed local result shape” with explicit receipt-v1/result-v2 wording and link this acceptance. This records the already accepted narrow exception; no mechanism/API change is required. The command guide now correctly documents graceful/force and resultv2 above that paragraph.

Source/contract acceptance only. Independent Darwin tests are attributed to the reviewer report, not rerun here. This receipt does not prove Redis final-save durability or authorize target selection/signaling. No production source edits or process/engine effects by Core owner.

Acceptance provenance: Core owner receipt `core-cli934-graceful-result-v2-exception.md`, SHA256 `0e4a44e1c9deef2f1d509484f81aad5f58b9c03da10d43bff888960d78ea8088`, recorded 2026-10-28. This copy retains the owner acceptance; hashes above identify the accepted inputs before the requested command-guide clarification.

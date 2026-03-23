# Daemon Architecture

The daemon subsystem manages background codefly processes -- long-running services, the Mind Gateway, and agent health monitoring.

## Overview

```
┌──────────────────────────────────────────────────┐
│                  codefly daemon                  │
│                                                  │
│  ┌────────────┐  ┌───────────┐  ┌─────────────┐ │
│  │   Start    │  │  Monitor  │  │   Gateway   │ │
│  │            │  │           │  │             │ │
│  │ Re-exec    │  │ CPU/Mem   │  │ Mind gRPC   │ │
│  │ + detach   │  │ tracking  │  │ server      │ │
│  └────────────┘  └───────────┘  └─────────────┘ │
│                                                  │
│  State: ~/.codefly/daemon.pid                    │
│  Logs:  ~/.codefly/daemon.log                    │
└──────────────────────────────────────────────────┘
```

## Commands

### `codefly daemon start`

Starts a detached background process. By default, it runs `codefly run service` with any flags passed after `--`.

```bash
codefly daemon start
codefly daemon start -- --runtime-context nix
codefly daemon start -- -d --service-path ./my-svc
```

**Gateway mode:** Start the Mind Gateway gRPC server instead of running services.

```bash
codefly daemon start --gateway
codefly daemon start --gateway --dir /path/to/workspace --port 50051
```

| Flag | Default | Description |
|------|---------|-------------|
| `--gateway` | false | Start Mind Gateway instead of services |
| `--dir` | `.` | Working directory (gateway mode only) |
| `--port` | 50051 | gRPC listen port (gateway mode only) |

### `codefly daemon stop`

Sends SIGTERM to the daemon process and waits up to 10 seconds for graceful shutdown. Falls back to SIGKILL if the process doesn't exit.

```bash
codefly daemon stop
```

### `codefly daemon restart`

Stops the running daemon (if any), then starts a new one.

```bash
codefly daemon restart
codefly daemon restart -- --runtime-context docker
```

### `codefly daemon status`

Checks if the daemon is running and prints PID and log path.

```bash
codefly daemon status
# Daemon is running (PID 12345)
#   Logs: /Users/you/.codefly/daemon.log
```

### `codefly daemon logs`

Displays daemon log output.

```bash
codefly daemon logs            # Print all logs
codefly daemon logs -f         # Follow log output (like tail -f)
codefly daemon logs -n 50      # Show last 50 lines
```

| Flag | Description |
|------|-------------|
| `-f`, `--follow` | Follow log output continuously |
| `-n`, `--tail` | Show last N lines |

### `codefly daemon monitor`

Checks all codefly-related processes for resource issues.

```bash
codefly daemon monitor                # One-shot check
codefly daemon monitor -w             # Continuous monitoring (every 30s)
codefly daemon monitor --kill-orphans # Kill orphaned agent processes
```

| Flag | Description |
|------|-------------|
| `-w`, `--watch` | Run continuously (check every 30s) |
| `--kill-orphans` | Kill orphaned go-grpc/go-generic agent processes |

### `codefly daemon gateway`

Runs the Mind Gateway gRPC server in the foreground. This is typically invoked by `daemon start --gateway`, not directly.

```bash
codefly daemon gateway --dir /workspace --port 50051
```

---

## Lifecycle

### Start

1. Check if a daemon is already running (read PID file, signal 0)
2. If already running, return error with existing PID
3. Open/truncate the log file (`~/.codefly/daemon.log`)
4. Find the current executable path
5. Re-exec the binary with the target command (e.g., `run service`)
6. Set `SysProcAttr.Setsid = true` to detach from the terminal session
7. Redirect stdout/stderr to the log file, close stdin
8. Start the child process
9. Write the child PID to `~/.codefly/daemon.pid`
10. Release the child process (prevent zombie)

### Stop

1. Read PID from `~/.codefly/daemon.pid`
2. Check if the process is alive (signal 0)
3. Send SIGTERM for graceful shutdown
4. Poll every 200ms for up to 10 seconds
5. If still alive after 10s, send SIGKILL
6. Remove the PID file

### Status Check

The daemon uses signal 0 (`proc.Signal(syscall.Signal(0))`) to check if a process is alive without affecting it. This handles stale PID files from crashed processes.

---

## PID Management

All state files live in `~/.codefly/`:

| File | Purpose |
|------|---------|
| `daemon.pid` | PID of the running daemon process |
| `daemon.log` | stdout/stderr output from the daemon |
| `monitor.log` | Output from the process monitor |

The state directory is created automatically (`0755` permissions) on first use.

---

## Process Monitor

The monitor scans for codefly-related processes using `ps aux` and checks against patterns: `go-grpc`, `go-generic`, `codefly`, `neo4j`, `mind-server`.

### Thresholds

| Check | Default | Action |
|-------|---------|--------|
| CPU > 200% | Warning | Auto-kill agent processes |
| CPU > 100% for 2 consecutive checks | Kill | Kill agent processes (30s interval) |
| Memory > 512MB | Warning | Log warning |
| Orphaned agents > 3 | Warning | Log warning |

### Continuous Monitoring

When running with `--watch`, the monitor:

1. Checks processes every 30 seconds
2. Tracks consecutive high-CPU PIDs across checks
3. Kills agent processes (go-grpc, go-generic) that exceed the threshold for 2+ consecutive checks
4. Logs all warnings and periodic status to `~/.codefly/monitor.log`
5. Cleans up tracking state when processes die

Only agent processes (`go-grpc`, `go-generic`) are auto-killed. The main `codefly` process and infrastructure services (`neo4j`) are never automatically killed.

### Status Output

```
Codefly Process Monitor -- 14:23:45
Total: 5 processes, 45.2% CPU, 256MB memory

PID      CPU%   MEM(MB)  PROCESS
12345    12.3   64       go-grpc
12346    8.5    128      codefly
12347    24.4   64       go-generic

Warnings:
  HIGH MEM: PID 12346 (codefly) at 640MB
```

---

## Gateway Mode

The gateway mode starts a Mind Gateway gRPC server as a daemon. This is used by the Mind engineering platform to communicate with the local codefly workspace.

```bash
codefly daemon start --gateway --dir /path/to/workspace --port 50051
```

The gateway:

- Listens on the specified gRPC port (default 50051)
- Writes a port file for service discovery
- Cleans up the port file on exit
- Responds to SIGINT for graceful shutdown

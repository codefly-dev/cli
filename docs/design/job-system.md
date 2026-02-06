# Job System Design

## Overview

Jobs are ephemeral execution units for scheduled or one-shot tasks. Unlike services (long-running processes), jobs execute to completion and exit.

## Use Cases

1. **Database Migrations** - Schema changes, data migrations
2. **Data Processing** - ETL pipelines, batch processing
3. **Scheduled Tasks** - Cron-like scheduled jobs (reports, cleanup)
4. **Deployment Tasks** - Post-deploy hooks, seeding data
5. **Background Workers** - Queue consumers, event processors

## Job Types

### Execution Patterns

- **one-shot**: Execute once immediately, then complete
- **scheduled**: Execute on a cron schedule
- **triggered**: Execute in response to events or manual invocation
- **workflow**: Coordinated execution of multiple jobs

### Job States

```
pending → running → completed
                  → failed
                  → cancelled
```

## Configuration

### job.codefly.yaml

```yaml
kind: job
name: db-migration
description: Run database migrations
version: 0.0.1

# Execution configuration
execution:
  type: one-shot  # one-shot, scheduled, triggered
  schedule: ""    # cron expression for scheduled jobs
  timeout: 5m     # maximum execution time
  retries: 3      # retry count on failure
  retry-delay: 30s

# Agent for execution (similar to services)
agent:
  kind: codefly:job
  name: go-job
  version: 0.0.1
  publisher: codefly.ai

# Dependencies
service-dependencies:
  - name: postgres
    module: infrastructure
    endpoints:
      - name: tcp

job-dependencies:
  - name: seed-data
    # Jobs can depend on other jobs in a workflow

library-dependencies:
  - name: shared-models
    version: "^1.0.0"
    languages:
      - go

workspace-configuration-dependencies:
  - database-credentials

# Job-specific configuration
spec:
  migration-dir: ./migrations
```

### Module with Jobs

```yaml
# module.codefly.yaml
kind: module
name: backend

services:
  - name: api
  - name: postgres

jobs:
  - name: db-migration
  - name: seed-data
```

## Core Types

### Job Struct

```go
type Job struct {
    Kind        string `yaml:"kind"`
    Name        string `yaml:"name"`
    Description string `yaml:"description,omitempty"`
    Version     string `yaml:"version"`

    // Execution configuration
    Execution   *JobExecution `yaml:"execution,omitempty"`

    // Agent for running the job
    Agent       *Agent `yaml:"agent,omitempty"`

    // Dependencies
    ServiceDependencies              []*ServiceDependency   `yaml:"service-dependencies,omitempty"`
    JobDependencies                  []*JobDependency       `yaml:"job-dependencies,omitempty"`
    LibraryDependencies              []*LibraryDependency   `yaml:"library-dependencies,omitempty"`
    WorkspaceConfigurationDependencies []string             `yaml:"workspace-configuration-dependencies,omitempty"`

    // Job-specific settings
    Spec map[string]any `yaml:"spec,omitempty"`

    // Internal
    dir    string
    module string
}

type JobExecution struct {
    Type       JobExecutionType `yaml:"type"`       // one-shot, scheduled, triggered
    Schedule   string           `yaml:"schedule"`   // cron expression
    Timeout    string           `yaml:"timeout"`    // e.g., "5m", "1h"
    Retries    int              `yaml:"retries"`
    RetryDelay string           `yaml:"retry-delay"`
}

type JobExecutionType string

const (
    JobExecutionOneShot   JobExecutionType = "one-shot"
    JobExecutionScheduled JobExecutionType = "scheduled"
    JobExecutionTriggered JobExecutionType = "triggered"
)

type JobDependency struct {
    Name   string `yaml:"name"`
    Module string `yaml:"module,omitempty"`
}
```

### Job Reference

```go
type JobReference struct {
    Name         string  `yaml:"name"`
    Module       string  `yaml:"-"`
    PathOverride *string `yaml:"path,omitempty"`
}
```

### Job Identity

```go
type JobIdentity struct {
    Name      string
    Module    string
    Workspace string
    Version   string
}

func (j *JobIdentity) Unique() string {
    return fmt.Sprintf("%s/%s", j.Module, j.Name)
}
```

## Orchestration

### Job Actions

```go
const (
    // Job-specific actions
    JobBegin   ActionType = "job-begin"
    JobLoad    ActionType = "job-load"
    JobInit    ActionType = "job-init"
    JobExecute ActionType = "job-execute"
    JobStop    ActionType = "job-stop"
)
```

### Job Flow

1. **JobBegin** - Start job processing
2. **JobLoad** - Load job configuration and resolve dependencies
3. **JobInit** - Initialize job resources (connections, etc.)
4. **JobExecute** - Run the actual job logic
5. **JobStop** - Cleanup and report results

### Job Policy

```go
type JobRunPolicy struct {
    // Implements PlaybookPolicy
    // Executes job actions in dependency order
}

func (p *JobRunPolicy) Execute(ctx context.Context, action Action) ([]Action, error) {
    switch action.Type {
    case JobBegin:
        return []Action{{Type: JobLoad, Job: action.Job}}, nil
    case JobLoad:
        return []Action{{Type: JobInit, Job: action.Job}}, nil
    case JobInit:
        return []Action{{Type: JobExecute, Job: action.Job}}, nil
    case JobExecute:
        return []Action{{Type: JobStop, Job: action.Job}}, nil
    case JobStop:
        return nil, nil // Job complete
    }
    return nil, nil
}
```

## CLI Commands

### Add Job

```bash
# Create a new job
codefly add job db-migration --module=backend --agent=go-job

# Create with execution type
codefly add job cleanup --module=backend --type=scheduled --schedule="0 0 * * *"
```

### Run Job

```bash
# Run a job immediately
codefly run job db-migration --module=backend

# Run with dependencies
codefly run job db-migration --module=backend --with-services

# Run as part of a workflow
codefly run job-workflow --jobs=db-migration,seed-data
```

### List Jobs

```bash
# List all jobs
codefly list jobs

# List jobs in a module
codefly list jobs --module=backend
```

## Agent Integration

### Job Agent Type

```go
const (
    JobAgent AgentKind = "codefly:job"
)
```

### Job Agent Capabilities

```proto
enum JobCapability {
    JOB_EXECUTE = 0;    // Can execute job logic
    JOB_RETRY = 1;      // Supports retry logic
    JOB_CHECKPOINT = 2; // Supports checkpointing for resumable jobs
}
```

### Job Agent Protocol

```proto
service JobAgent {
    // Lifecycle
    rpc Load(JobLoadRequest) returns (JobLoadResponse);
    rpc Init(JobInitRequest) returns (JobInitResponse);
    rpc Execute(JobExecuteRequest) returns (stream JobExecuteResponse);
    rpc Stop(JobStopRequest) returns (JobStopResponse);
}

message JobExecuteRequest {
    Job job = 1;
    map<string, string> configurations = 2;
    repeated NetworkMapping network_mappings = 3;
}

message JobExecuteResponse {
    JobState state = 1;
    string output = 2;
    map<string, string> results = 3;  // Job outputs
    string error = 4;
}

enum JobState {
    JOB_RUNNING = 0;
    JOB_COMPLETED = 1;
    JOB_FAILED = 2;
}
```

## Service Dependencies

Jobs can depend on services being available:

```yaml
service-dependencies:
  - name: postgres
    module: infrastructure
    endpoints:
      - name: tcp
```

When running a job:
1. Resolve service dependencies
2. Start required services (if not running)
3. Wait for services to be ready
4. Execute job
5. Optionally stop services after job completion

## Workflow Support

### Job Workflow

Multiple jobs can be coordinated:

```yaml
# workflow.codefly.yaml
kind: workflow
name: deployment

jobs:
  - name: db-migration
    module: backend
  - name: seed-data
    module: backend
    depends-on:
      - db-migration
  - name: cache-warmup
    module: backend
    depends-on:
      - seed-data
```

### Workflow Execution

Jobs execute respecting the dependency DAG, similar to service orchestration.

## Docker Integration

### Job Container

Jobs run in containers similar to services:

```dockerfile
FROM golang:1.22 AS builder
COPY . /app
RUN go build -o /job ./cmd/job

FROM alpine:latest
COPY --from=builder /job /job
ENTRYPOINT ["/job"]
```

### Network Access

Jobs need network access to dependent services:
- Use same NetworkMapping system as services
- Runtime context (native/container) applies to jobs

## Implementation Plan

### Phase 1: Core Types
1. Create `core/resources/job.go` with Job struct
2. Add JobReference to Module
3. Add JobIdentity
4. Create loader integration (job.codefly.yaml)

### Phase 2: CLI Commands
1. `codefly add job` - Create new job
2. `codefly list jobs` - List jobs
3. `codefly run job` - Execute job

### Phase 3: Orchestration
1. Add Job actions to action.go
2. Create JobRunPolicy
3. Integrate with Flow

### Phase 4: Agent Support
1. Define job agent protocol (proto)
2. Implement job runner
3. Create example go-job agent

## Testing Strategy

```go
func TestJobLoad(t *testing.T) {
    // Load job from testdata
}

func TestJobExecution(t *testing.T) {
    // Test job lifecycle
}

func TestJobDependencies(t *testing.T) {
    // Test job with service dependencies
}

func TestJobWorkflow(t *testing.T) {
    // Test multi-job workflow
}
```

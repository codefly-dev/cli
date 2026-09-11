//go:build integration

package ci

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/core/resources"
	"github.com/google/uuid"
)

func TestCIBuildOrdersRealImagePrerequisites(t *testing.T) {
	for _, producerFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("producerFails=%t", producerFails), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if _, err := ciDocker(ctx, "info"); err != nil {
				t.Fatalf("integration test requires Docker: %v", err)
			}
			builder, err := ciDocker(ctx, "context", "show")
			if err != nil {
				t.Fatal(err)
			}
			_, workspace := loadSchedulerFixture(t)
			suffix := uuid.NewString()
			images := map[string]string{}
			for _, name := range []string{"organization", "frontend", "worker"} {
				images[name] = "codefly-ci-prerequisite-" + name + ":" + suffix
			}
			created := make(chan string, 4)
			t.Cleanup(func() {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
				defer cleanupCancel()
				close(created)
				seen := map[string]bool{}
				for image := range created {
					if seen[image] {
						continue
					}
					seen[image] = true
					if _, err := ciDocker(cleanupCtx, "image", "rm", image); err != nil {
						t.Errorf("remove test image: %v", err)
					}
				}
			})
			build := func(ctx context.Context, dir, image string) error {
				_, err := ciDocker(ctx, "buildx", "build", "--builder", builder, "--load", "--pull=false", "-t", image, dir)
				if err == nil {
					created <- image
				}
				return err
			}
			writeDockerfile := func(contents string) string {
				dir := t.TempDir()
				if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(contents), 0o600); err != nil {
					t.Fatal(err)
				}
				return dir
			}
			old := writeDockerfile("FROM scratch\nLABEL ci.revision=old\n")
			if err := build(ctx, old, images["organization"]); err != nil {
				t.Fatal(err)
			}
			producer := "FROM scratch\nLABEL ci.revision=new\n"
			if producerFails {
				producer = "INVALID_DOCKERFILE_INSTRUCTION\n"
			}
			directories := map[string]string{
				"organization": writeDockerfile(producer),
				"frontend":     writeDockerfile("FROM " + images["organization"] + "\nLABEL ci.consumer=true\n"),
				"worker":       writeDockerfile("FROM scratch\nLABEL ci.revision=independent\n"),
			}
			plan := &Plan{Services: []PlannedService{
				{Service: "web/frontend"},
				{Service: "management/organization"},
				{Service: "management/worker"},
			}}
			reporter := fixedCIReporter(t, plan)
			producerStarted := make(chan struct{})
			independentBuilt := make(chan struct{})
			runErr := CIWithPlanOptions(ctx, workspace, plan, func(ctx context.Context, _ *resources.Workspace, _ *resources.Module, service *resources.Service) error {
				switch service.Name {
				case "organization":
					close(producerStarted)
					select {
					case <-independentBuilt:
					case <-ctx.Done():
						return ctx.Err()
					}
				case "worker":
					defer close(independentBuilt)
					select {
					case <-producerStarted:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				return build(ctx, directories[service.Name], images[service.Name])
			}, ScheduleOptions{Jobs: 2, Phase: "build", Reporter: reporter})
			report := reporter.Finalize(runErr)
			assertReportTask(t, report.Tasks[2], reportStatusPassed, "")
			if producerFails {
				if runErr == nil {
					t.Fatal("invalid producer Dockerfile passed the gate")
				}
				assertReportTask(t, report.Tasks[1], reportStatusFailed, "")
				assertReportTask(t, report.Tasks[0], reportStatusSkipped, reportReasonFailedPrerequisite)
				return
			}
			if runErr != nil {
				t.Fatal(runErr)
			}
			for _, name := range []string{"organization", "frontend"} {
				revision, err := ciDocker(ctx, "image", "inspect", "--format", `{{index .Config.Labels "ci.revision"}}`, images[name])
				if err != nil {
					t.Fatal(err)
				}
				if revision != "new" {
					t.Fatalf("%s image contains revision %q, want new", name, revision)
				}
			}
		})
	}
}

func ciDocker(ctx context.Context, args ...string) (string, error) {
	output, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output)), nil
}

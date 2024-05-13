package cmd

import (
	"context"
	"log"
	"os/exec"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/spf13/cobra"
)

// ClearCmd represents the add command
var ClearCmd = &cobra.Command{
	Use:   "clear",
	Short: "clear",
	Run: func(cmd *cobra.Command, args []string) {
		clearCommand(args)
	},
}

// args are service name
func clearCommand(args []string) {
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "bash", "-c", "ps aux | grep codefly.dev | grep -v grep | awk '{print $2}' | xargs kill -9")
	err := cmd.Run()
	if err != nil {
		log.Fatalf("can't clear all codefly processes %s\n", err)
	}
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatalf("can't create docker client %s\n", err)
	}
	// delete all c with name starting with /codefly
	cos, err := cli.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		log.Fatalf("can't list containers %s\n", err)
	}

	for _, c := range cos {
		if strings.HasPrefix(c.Names[0], "/codefly") {
			if len(args) > 0 {
				for _, arg := range args {
					if !strings.Contains(c.Names[0], arg) {
						continue
					}
				}
			}
			err = cli.ContainerKill(ctx, c.ID, "SIGKILL")
			err = cli.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true})
			if err != nil {
				log.Fatalf("can't remove container %s\n", err)
			}
		}
	}
}

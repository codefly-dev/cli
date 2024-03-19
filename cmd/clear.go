package cmd

import (
	"context"
	"log"
	"os/exec"

	"github.com/codefly-dev/core/runners"
	"github.com/spf13/cobra"
)

// ClearCmd represents the add command
var ClearCmd = &cobra.Command{
	Use:   "clear",
	Short: "clear",
	Run: func(cmd *cobra.Command, args []string) {
		clearCommand()
	},
}

func clearCommand() {
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "bash", "-c", "ps aux | grep codefly.dev | grep -v grep | awk '{print $2}' | xargs kill -9")
	err := cmd.Run()
	if err != nil {
		log.Fatalf("can't clear all codefly processes %s\n", err)
	}
	docker, err := runners.NewDocker(ctx)
	if err != nil {
		log.Fatalf("can't create docker runner %s\n", err)
	}
	err = docker.KillAll(ctx)
	if err != nil {
		log.Fatalf("can't clear all codefly docker processes %s\n", err)
	}

}

package cmd

import (
	"log"
	"os/exec"

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
	cmd := exec.Command("bash", "-c", "ps aux | grep codefly.dev | grep -v grep | awk '{print $2}' | xargs kill -9")
	err := cmd.Run()
	if err != nil {
		log.Fatalf("cmd.Run() failed with %s\n", err)
	}
}

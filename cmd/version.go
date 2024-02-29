package cmd

import (
	"embed"
	"io/fs"
	"log"

	"github.com/spf13/cobra"
	yaml "gopkg.in/yaml.v3"
)

type Information struct {
	Version string `json:"version"`
}

// VersionCmd represents the build command
var VersionCmd = &cobra.Command{
	Use:   "version",
	Short: "Version of codefly",
	Run: func(cmd *cobra.Command, args []string) {
		data, err := fs.ReadFile(infoFS, "info.yaml")
		if err != nil {
			log.Fatalf("error: %v", err)
		}

		// Unmarshal YAML into a struct
		var info Information
		err = yaml.Unmarshal(data, &info)
		if err != nil {
			log.Fatalf("error: %v", err)
		}
		cmd.Println(info.Version)
	},
}

//go:embed info.yaml
var infoFS embed.FS

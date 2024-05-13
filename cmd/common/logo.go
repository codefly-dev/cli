package common

import (
	_ "embed"
	"fmt"
	"os"
	"os/exec"

	resources "github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/golor"
)

func imgcat(filePath string) error {
	cmd := exec.Command("imgcat", filePath)
	err := cmd.Run()
	if err != nil {
		return err
	}
	return nil
}

func isInITerm() bool {
	return os.Getenv("TERM_PROGRAM") == "iTerm.app"
}

func Logo() {
	golor.Println(`#(bold,white)[Welcome to codefly.ai!]`)
	if isInITerm() {
		dir, err := resources.HomeDir()
		if err != nil {
			return
		}
		p := fmt.Sprintf("%s/.codefly/media/dragonfly.png", dir)
		err = imgcat(p)
		if err == nil {
			return
		}
	}
	golor.Println(fmt.Sprintf(`#(green)[%s]`, string(logo)))
}

//go:embed media/ascii-codefly.txt
var logo []byte

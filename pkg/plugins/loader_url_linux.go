//go:build amd64 && linux

package plugins

import (
	"fmt"

	"github.com/codefly-dev/core/configurations"
)

func DownloadURL(p *configurations.Plugin) string {
	return fmt.Sprintf("https://github.com/codefly-dev/service-%s/releases/download/v%s/service-%s_%s_linux_x86_64.tar.gz", p.Identifier, p.Version, p.Identifier, p.Version)

}

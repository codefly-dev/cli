package generator

import (
	"fmt"
	"path"
	"strings"

	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/configurations/generation"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/templates"
)

type visitor struct {
	base     shared.Dir
	logger   shared.BaseLogger
	replacer *templates.ServiceReplacer
	ignores  []string
}

func (v *visitor) Apply(p shared.File, to shared.Dir) error {
	tmpl := fmt.Sprintf("%s.%s", p.Base(), "tmpl")
	target := path.Join(to.Absolute(), tmpl)
	v.logger.Tracef("copying %s -> %s", p, target)
	err := templates.CopyAndReplace(shared.NewDirReader(), p, shared.NewFile(target), v.replacer)
	if err != nil {
		return v.logger.Wrapf(err, "cannot copy and apply template")
	}
	return nil
}

func (v *visitor) Ignore(file shared.File) bool {
	if strings.Contains(file.Relative(), ".idea") {
		return true
	}
	if strings.HasSuffix(file.Relative(), ".sum") {
		return true
	}
	if strings.HasSuffix(file.Relative(), ".lock") {
		return true
	}
	if file.Base() == "service.codefly.yaml" {
		return true
	}
	if file.Base() == "service.generation.codefly.yaml" {
		return true
	}
	for _, ignore := range v.ignores {
		if strings.Contains(file.Relative(), ignore) {
			return true
		}
	}
	return false
}

func GenerateServiceTemplate(dir string) error {
	logger := shared.NewLogger("generator.GenerateServiceTemplate")
	base := path.Join(dir, "base")
	err := shared.CheckDirectory(base)
	if err != nil {
		return shared.NewUserError("we expect to find a working service in </base> folder")
	}
	logger.Debugf("found base to generate new plugin templates")
	gen, err := configurations.LoadFromDir[generation.Service](base)
	if err != nil {
		logger.Warn(shared.NewUserWarning("no service generation configuration found, using default"))
	}
	logger.Debugf("ignoring files: %v", gen.Ignores)
	replacer := templates.NewServiceReplacer(gen)
	// For now, we copy everything to template and add .tmpl
	target := path.Join(dir, "templates/factory")

	visitor := &visitor{base: shared.NewDir(base), logger: logger, replacer: replacer, ignores: gen.Ignores}
	err = templates.CopyAndVisit(logger, shared.NewDirReader(), shared.NewDir(base), shared.NewDir(target), visitor)
	if err != nil {
		return shared.NewUserError("cannot copy base to templates: %s", err)
	}
	return nil
}

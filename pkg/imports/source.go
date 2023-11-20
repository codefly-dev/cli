package imports

import "github.com/codefly-dev/core/shared"

type SourceImporter interface {
	Analyze() (*Recommendation, error)
}

type LocalSourceImporter struct {
	dir string
}

func (l *LocalSourceImporter) Analyze() (*Recommendation, error) {
	logger := shared.NewLogger("import.LocalSourceImporter.Analyze")
	recommendations, err := Analyze(l.dir)
	if err != nil {
		return nil, logger.Wrapf(err, "cannot analyze directory")
	}
	return recommendations, nil
}

func (l *LocalSourceImporter) Import(target string) error {
	logger := shared.NewLogger("import.LocalSourceImporter.Import")
	err := shared.CopyDirectory(l.dir, target)
	if err != nil {
		return logger.Wrapf(err, "cannot copy directory")
	}
	return nil
}

func NewLocalSourceImporter(path string) (SourceImporter, error) {
	return &LocalSourceImporter{dir: path}, nil
}

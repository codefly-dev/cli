package imports

import (
	"context"

	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/wool"
)

type SourceImporter interface {
	Analyze(ctx context.Context) (*Recommendation, error)
}

type LocalSourceImporter struct {
	dir string
}

func (l *LocalSourceImporter) Analyze(ctx context.Context) (*Recommendation, error) {
	w := wool.Get(ctx).In("import.LocalSourceImporter.Analyze")
	recommendations, err := Analyze(ctx, l.dir)
	if err != nil {
		return nil, logger.Wrapf(err, "cannot analyze directory")
	}
	return recommendations, nil
}

func (l *LocalSourceImporter) Import(ctx context.Context, target string) error {
	w := wool.Get(ctx).In("import.LocalSourceImporter.Import")
	err := shared.CopyDirectory(ctx, l.dir, target)
	if err != nil {
		return logger.Wrapf(err, "cannot copy directory")
	}
	return nil
}

func NewLocalSourceImporter(path string) (SourceImporter, error) {
	return &LocalSourceImporter{dir: path}, nil
}

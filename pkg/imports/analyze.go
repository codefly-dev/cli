package imports

import (
	"context"
	"path/filepath"

	"github.com/codefly-dev/core/wool"
)

func Analyze(ctx context.Context, dir string) (*Recommendation, error) {
	w := wool.Get(ctx).In("imports.Analyze")
	recommenders := []Recommender{CheckDockerCompose, CheckDocker}
	for _, r := range recommenders {
		rec, err := r(ctx, dir)
		if err != nil {
			return nil, logger.Wrapf(err, "error checking with <%V>", r)
		}
		if rec != nil {
			rec.Name = filepath.Base(dir)
			return rec, nil
		}
	}
	return nil, nil
}

type Recommender func(ctx context.Context, dir string) (*Recommendation, error)

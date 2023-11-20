package imports

import (
	"path/filepath"

	"github.com/codefly-dev/core/shared"
)

func Analyze(dir string) (*Recommendation, error) {
	logger := shared.NewLogger("imports.Analyze")
	recommenders := []Recommender{CheckDockerCompose, CheckDocker}
	for _, r := range recommenders {
		rec, err := r(dir)
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

type Recommender func(dir string) (*Recommendation, error)

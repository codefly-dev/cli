package plugins

import "github.com/codefly-dev/core/configurations"

type Options struct {
	Quiet       bool
	Application *configurations.Application
}

type Option = func(options *Options)

func WithQuiet() Option {
	return func(options *Options) {
		options.Quiet = true
	}
}

func WithApplication(app *configurations.Application) Option {
	return func(options *Options) {
		options.Application = app
	}
}

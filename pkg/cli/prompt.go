package cli

var withDefault bool

func WithDefault() bool {
	return withDefault
}

func SetWithDefault(wd bool) {
	withDefault = wd
}

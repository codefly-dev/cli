package code

import (
	"path"
	"path/filepath"
	"strings"

	"github.com/codefly-dev/cli/pkg/plugins"
	"github.com/codefly-dev/core/shared"
	"github.com/fsnotify/fsnotify"
)

type Watcher struct {
	events       chan<- Change
	watcher      *fsnotify.Watcher
	PluginLogger *plugins.PluginLogger

	// internal
	excludes []string
	base     string
}

type Change struct {
	Path       string
	IsRelative bool
}

func NewWatcher(pluginLogger *plugins.PluginLogger, events chan<- Change, base string, includes []string, excludes ...string) (*Watcher, error) {
	logger := shared.NewLogger("code.NewWatcher")
	// Add new watcher.
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, logger.Wrapf(err, "cannot create fsnotify watcher")
	}
	for _, p := range includes {
		err = watcher.Add(path.Join(base, p))
		if err != nil {
			return nil, logger.Wrapf(err, "cannot add path: %s", p)
		}
	}

	w := &Watcher{watcher: watcher, base: base, events: events, PluginLogger: pluginLogger}
	w.excludes = append(w.excludes, excludes...)
	go w.Start()
	return w, nil
}

func (w *Watcher) Start() {
	// Start listening for events.
	defer w.watcher.Close()
	for {
		select {
		case event, ok := <-w.watcher.Events:
			if !ok {
				return
			}
			for _, exclude := range w.excludes {
				if strings.Contains(event.Name, exclude) {
					continue
				}
			}

			if event.Has(fsnotify.Write) {
				rel, err := filepath.Rel(w.base, event.Name)
				if err == nil {
					w.events <- Change{
						Path:       rel,
						IsRelative: true,
					}
					continue
				}
				w.events <- Change{
					Path: event.Name,
				}
			}
		case err, ok := <-w.watcher.Errors:
			if !ok {
				return
			}
			w.PluginLogger.Info("error: %v", err)
		}
	}
}

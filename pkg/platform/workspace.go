package platform

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
	"google.golang.org/protobuf/encoding/protojson"
)

func UpdateWorkspace(ctx context.Context, workspace *resources.Workspace) error {
	w := wool.Get(ctx).In("UpdateWorkspace")
	w.Info("updating workspace", wool.Field("workspace", workspace))
	url := fmt.Sprintf("%s/workspace", GetClient().URL("workspace/workspace"))
	c := http.Client{Timeout: 5 * time.Second}

	proto, err := architecture.LoadWorkspace(ctx, workspace)
	if err != nil {
		return w.Wrapf(err, "cannot serialize workspace")
	}
	json, err := protojson.Marshal(proto)
	if err != nil {
		return w.Wrapf(err, "cannot serialize workspace")
	}
	payload := fmt.Sprintf(`{"workspace": %s}`, string(json))

	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(payload))
	if err != nil {
		return w.Wrapf(err, "cannot create update workspace request")
	}
	req.Header.Add(wool.Header(wool.UserAuthIDKey), "test-auth-id")
	resp, err := c.Do(req)
	if err != nil {
		return w.Wrapf(err, "cannot update workspace")
	}
	if resp.StatusCode != http.StatusOK {
		return w.NewError("unexpected status code: %s", resp.Status)
	}
	return nil
}

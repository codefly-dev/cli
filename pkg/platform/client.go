package platform

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
)

type Client struct {
	Base string
}

func NewClient(ctx context.Context) (*Client, error) {
	//w := wool.Get(ctx).In("NewService")
	client := &Client{
		Base: "http://localhost:25012",
	}
	return client, nil
}

func (c *Client) UpdateWorkspace(ctx context.Context, workspace *resources.Workspace) error {
	w := wool.Get(ctx).In("UpdateWorkspace")
	w.Info("updating workspace", wool.Field("workspace", workspace))
	url := fmt.Sprintf("%s/workspace", c.Base)
	client := http.Client{Timeout: 5 * time.Second}
	proto, err := architecture.LoadWorkspace(ctx, workspace)
	if err != nil {
		return w.Wrapf(err, "cannot serialize workspace")
	}
	json, err := protojson.Marshal(proto)
	if err != nil {
		return w.Wrapf(err, "cannot serialize workspace")
	}
	payload := fmt.Sprintf(`{"workspace": %s}`, string(json))

	resp, err := client.Post(url, "application/json", strings.NewReader(payload))
	if err != nil {
		return w.Wrapf(err, "cannot update workspace")
	}
	if resp.StatusCode != http.StatusOK {
		return w.NewError("unexpected status code", wool.Field("status", resp.Status))
	}
	return nil
}

//
//func NewClient(ctx context.Context) (*Client, error) {
//	// Define the custom endpoint
//	customEndpoint, err := url.Parse("http://localhost:21172")
//	if err != nil {
//		return nil, err
//	}
//	transport := httptransport.New(customEndpoint.Host, customEndpoint.Path, []string{customEndpoint.Scheme})
//	transport.DefaultAuthentication = httptransport.BearerToken(token)
//
//	client := api.New(transport, strfmt.Default)
//	return client, nil
//}

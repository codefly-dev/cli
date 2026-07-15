package platform

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
)

type Client struct {
	baseURL string
}

func LoadToken(ctx context.Context, workspace *resources.Workspace) (string, error) {
	// Load from environment variable first
	token := os.Getenv("CODEFLY_TOKEN")
	if token != "" {
		return token, nil
	}
	// Load from file
	tokenBytes, err := os.ReadFile(filepath.Join(workspace.Dir(), ".token"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(tokenBytes)), nil
}

func GetClient() *Client {
	return platformClient
}

func (c *Client) URL(unique string) string {
	if c != nil && c.baseURL != "" {
		return strings.TrimRight(c.baseURL, "/") + "/" + strings.TrimLeft(unique, "/")
	}
	if os.Getenv("GATEWAY") != "" {
		base := "http://localhost:2532"
		return fmt.Sprintf("%s/%s", base, unique)
	}
	base := "http://localhost:25012"
	return base
}

func InitClient(ctx context.Context) error {
	w := wool.Get(ctx).In("InitClient")
	client, err := NewClient(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot create platform service")
	}
	platformClient = client
	return nil
}

func NewClient(ctx context.Context) (*Client, error) {
	client := &Client{}
	return client, nil
}

var platformClient *Client

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
//	platformClient := api.New(transport, strfmt.Default)
//	return platformClient, nil
//}

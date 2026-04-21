package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	cliv0 "github.com/codefly-dev/core/generated/go/codefly/cli/v0"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// terminalClient manages a lazy gRPC connection to the codefly daemon's TerminalService.
type terminalClient struct {
	mu     sync.Mutex
	conn   *grpc.ClientConn
	client cliv0.TerminalServiceClient
}

var globalTermClient = &terminalClient{}

const daemonAddr = "localhost:10000"

func (tc *terminalClient) getClient() (cliv0.TerminalServiceClient, error) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	if tc.client != nil {
		return tc.client, nil
	}
	conn, err := grpc.NewClient(daemonAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("cannot connect to codefly daemon at %s: %w", daemonAddr, err)
	}
	tc.conn = conn
	tc.client = cliv0.NewTerminalServiceClient(conn)
	return tc.client, nil
}

// registerTerminalTools adds terminal MCP tools.
func (s *Server) registerTerminalTools() {
	s.RegisterTool(Tool{
		Name:        "open_terminal",
		Description: "Open a new terminal session scoped to a module/service directory",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"module":  {Type: "string", Description: "Module name (optional)"},
				"service": {Type: "string", Description: "Service name (optional)"},
				"shell":   {Type: "string", Description: "Shell override (default: $SHELL)"},
			},
		},
	}, s.openTerminal)

	s.RegisterTool(Tool{
		Name:        "send_terminal_input",
		Description: "Send input to a terminal session and return output. Include \\n for enter key.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"session_id": {Type: "string", Description: "Terminal session ID"},
				"input":      {Type: "string", Description: "Input to send (include \\n for newline)"},
			},
			Required: []string{"session_id", "input"},
		},
	}, s.sendTerminalInput)

	s.RegisterTool(Tool{
		Name:        "read_terminal_output",
		Description: "Read latest output from a terminal session",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"session_id": {Type: "string", Description: "Terminal session ID"},
			},
			Required: []string{"session_id"},
		},
	}, s.readTerminalOutput)

	s.RegisterTool(Tool{
		Name:        "close_terminal",
		Description: "Close a terminal session",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"session_id": {Type: "string", Description: "Terminal session ID"},
			},
			Required: []string{"session_id"},
		},
	}, s.closeTerminal)

	s.RegisterTool(Tool{
		Name:        "list_terminals",
		Description: "List active terminal sessions",
		InputSchema: InputSchema{
			Type:       "object",
			Properties: map[string]PropertySchema{},
		},
	}, s.listTerminals)
}

func (s *Server) openTerminal(ctx context.Context, args map[string]string) ([]Content, error) {
	client, err := globalTermClient.getClient()
	if err != nil {
		return nil, err
	}

	resp, err := client.Open(ctx, &cliv0.OpenTerminalRequest{
		Module:  args["module"],
		Service: args["service"],
		Shell:   args["shell"],
		Rows:    40,
		Cols:    120,
	})
	if err != nil {
		return nil, fmt.Errorf("open_terminal: %w", err)
	}

	result := map[string]string{
		"session_id":  resp.SessionId,
		"shell":       resp.Shell,
		"working_dir": resp.WorkingDir,
	}
	data, _ := json.MarshalIndent(result, "", "  ")
	return []Content{TextContent(string(data))}, nil
}

func (s *Server) sendTerminalInput(ctx context.Context, args map[string]string) ([]Content, error) {
	client, err := globalTermClient.getClient()
	if err != nil {
		return nil, err
	}

	sessionID := args["session_id"]
	input := args["input"]
	if sessionID == "" || input == "" {
		return nil, fmt.Errorf("session_id and input are required")
	}

	// Open a short-lived Attach stream: send input, collect output for a brief window
	stream, err := client.Attach(ctx)
	if err != nil {
		return nil, fmt.Errorf("send_terminal_input: cannot attach: %w", err)
	}

	// Send the input
	if err := stream.Send(&cliv0.TerminalInput{
		SessionId: sessionID,
		Data:      []byte(input),
	}); err != nil {
		return nil, fmt.Errorf("send_terminal_input: cannot send: %w", err)
	}

	// Collect output for up to 2 seconds or until we get a pause in output
	var output []byte
	done := make(chan struct{})

	go func() {
		defer close(done)
		for {
			msg, err := stream.Recv()
			if err != nil {
				return
			}
			if len(msg.Data) > 0 {
				output = append(output, msg.Data...)
			}
			if msg.Done {
				return
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}

	_ = stream.CloseSend()

	if len(output) == 0 {
		return []Content{TextContent("(no output)")}, nil
	}
	return []Content{TextContent(string(output))}, nil
}

func (s *Server) readTerminalOutput(ctx context.Context, args map[string]string) ([]Content, error) {
	client, err := globalTermClient.getClient()
	if err != nil {
		return nil, err
	}

	sessionID := args["session_id"]
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}

	// Attach briefly to read any pending output
	stream, err := client.Attach(ctx)
	if err != nil {
		return nil, fmt.Errorf("read_terminal_output: cannot attach: %w", err)
	}

	// Send session ID (no input data)
	if err := stream.Send(&cliv0.TerminalInput{SessionId: sessionID}); err != nil {
		return nil, fmt.Errorf("read_terminal_output: cannot send: %w", err)
	}

	var output []byte
	done := make(chan struct{})

	go func() {
		defer close(done)
		for {
			msg, err := stream.Recv()
			if err != nil {
				return
			}
			if len(msg.Data) > 0 {
				output = append(output, msg.Data...)
			}
			if msg.Done {
				return
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
	}

	_ = stream.CloseSend()

	if len(output) == 0 {
		return []Content{TextContent("(no output)")}, nil
	}
	return []Content{TextContent(string(output))}, nil
}

func (s *Server) closeTerminal(ctx context.Context, args map[string]string) ([]Content, error) {
	client, err := globalTermClient.getClient()
	if err != nil {
		return nil, err
	}

	sessionID := args["session_id"]
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}

	_, err = client.Close(ctx, &cliv0.CloseTerminalRequest{SessionId: sessionID})
	if err != nil {
		return nil, fmt.Errorf("close_terminal: %w", err)
	}

	return []Content{TextContent("closed")}, nil
}

func (s *Server) listTerminals(ctx context.Context, args map[string]string) ([]Content, error) {
	client, err := globalTermClient.getClient()
	if err != nil {
		return nil, err
	}

	resp, err := client.List(ctx, &cliv0.ListTerminalsRequest{})
	if err != nil {
		return nil, fmt.Errorf("list_terminals: %w", err)
	}

	type termInfo struct {
		SessionID  string `json:"session_id"`
		Shell      string `json:"shell"`
		WorkingDir string `json:"working_dir"`
		Module     string `json:"module,omitempty"`
		Service    string `json:"service,omitempty"`
	}

	var terminals []termInfo
	for _, t := range resp.Terminals {
		terminals = append(terminals, termInfo{
			SessionID:  t.SessionId,
			Shell:      t.Shell,
			WorkingDir: t.WorkingDir,
			Module:     t.Module,
			Service:    t.Service,
		})
	}

	if len(terminals) == 0 {
		return []Content{TextContent("no active terminals")}, nil
	}

	data, _ := json.MarshalIndent(terminals, "", "  ")
	return []Content{TextContent(string(data))}, nil
}

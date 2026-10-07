package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	cliv0 "github.com/codefly-dev/core/generated/go/codefly/cli/v0"
	"github.com/codefly-dev/core/network"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func terminalAddress(workspaceName string) string {
	return fmt.Sprintf("127.0.0.1:%d", network.CLIServerPort(workspaceName))
}

// terminalClient connects to the same deterministic per-workspace endpoint as
// `codefly terminal`. The old fixed localhost:10000 listener no longer exists.
func (s *Server) terminalClient() (cliv0.TerminalServiceClient, *grpc.ClientConn, error) {
	workspace, err := s.requireWorkspace()
	if err != nil {
		return nil, nil, err
	}
	addr := terminalAddress(workspace.Name)
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, fmt.Errorf("cannot connect to codefly server at %s: %w", addr, err)
	}
	return cliv0.NewTerminalServiceClient(conn), conn, nil
}

// registerTerminalTools adds terminal MCP tools.
func (s *Server) registerTerminalTools() {
	s.register(&Tool{
		Name:        "open_terminal",
		Description: "Open a new terminal session scoped to a module/service directory",
		InputSchema: InputSchema{
			Type: schemaTypeObject,
			Properties: map[string]PropertySchema{
				fieldModule:  {Type: schemaTypeString, Description: "Module name (optional)"},
				fieldService: {Type: schemaTypeString, Description: "Service name (optional)"},
				"shell":      {Type: schemaTypeString, Description: "Shell override (default: $SHELL)"},
			},
		},
	}, s.openTerminal)

	s.register(&Tool{
		Name:        "send_terminal_input",
		Description: "Send input to a terminal session and return output. Include \\n for enter key.",
		InputSchema: InputSchema{
			Type: schemaTypeObject,
			Properties: map[string]PropertySchema{
				fieldSessionID: {Type: schemaTypeString, Description: describeTerminalSession},
				"input":        {Type: schemaTypeString, Description: "Input to send (include \\n for newline)"},
			},
			Required: []string{fieldSessionID, "input"},
		},
	}, s.sendTerminalInput)

	s.register(&Tool{
		Name:        "read_terminal_output",
		Description: "Read latest output from a terminal session",
		InputSchema: InputSchema{
			Type: schemaTypeObject,
			Properties: map[string]PropertySchema{
				fieldSessionID: {Type: schemaTypeString, Description: describeTerminalSession},
			},
			Required: []string{fieldSessionID},
		},
	}, s.readTerminalOutput)

	s.register(&Tool{
		Name:        "close_terminal",
		Description: "Close a terminal session",
		InputSchema: InputSchema{
			Type: schemaTypeObject,
			Properties: map[string]PropertySchema{
				fieldSessionID: {Type: schemaTypeString, Description: describeTerminalSession},
			},
			Required: []string{fieldSessionID},
		},
	}, s.closeTerminal)

	s.register(&Tool{
		Name:        "list_terminals",
		Description: "List active terminal sessions",
		InputSchema: InputSchema{
			Type:       schemaTypeObject,
			Properties: map[string]PropertySchema{},
		},
	}, s.listTerminals)
}

func (s *Server) openTerminal(ctx context.Context, args map[string]string) ([]Content, error) {
	client, conn, err := s.terminalClient()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	resp, err := client.Open(ctx, &cliv0.OpenTerminalRequest{
		Module:  args[fieldModule],
		Service: args[fieldService],
		Shell:   args["shell"],
		Rows:    40,
		Cols:    120,
	})
	if err != nil {
		return nil, fmt.Errorf("open_terminal: %w", err)
	}

	result := map[string]string{
		fieldSessionID: resp.SessionId,
		"shell":        resp.Shell,
		"working_dir":  resp.WorkingDir,
	}
	data, _ := json.MarshalIndent(result, "", "  ")
	return []Content{TextContent(string(data))}, nil
}

func (s *Server) sendTerminalInput(ctx context.Context, args map[string]string) ([]Content, error) {
	client, conn, err := s.terminalClient()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	sessionID := args[fieldSessionID]
	input := args["input"]
	if sessionID == "" || input == "" {
		return nil, fmt.Errorf("session_id and input are required")
	}

	output, err := collectTerminalOutput(ctx, client, &cliv0.TerminalInput{
		SessionId: sessionID,
		Data:      []byte(input),
	}, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("send_terminal_input: %w", err)
	}

	if len(output) == 0 {
		return []Content{TextContent("(no output)")}, nil
	}
	return []Content{TextContent(string(output))}, nil
}

func (s *Server) readTerminalOutput(ctx context.Context, args map[string]string) ([]Content, error) {
	client, conn, err := s.terminalClient()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	sessionID := args[fieldSessionID]
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}

	output, err := collectTerminalOutput(ctx, client, &cliv0.TerminalInput{SessionId: sessionID}, time.Second)
	if err != nil {
		return nil, fmt.Errorf("read_terminal_output: %w", err)
	}

	if len(output) == 0 {
		return []Content{TextContent("(no output)")}, nil
	}
	return []Content{TextContent(string(output))}, nil
}

func (s *Server) closeTerminal(ctx context.Context, args map[string]string) ([]Content, error) {
	client, conn, err := s.terminalClient()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	sessionID := args[fieldSessionID]
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}

	_, err = client.Close(ctx, &cliv0.CloseTerminalRequest{SessionId: sessionID})
	if err != nil {
		return nil, fmt.Errorf("close_terminal: %w", err)
	}

	return []Content{TextContent("closed")}, nil
}

func (s *Server) listTerminals(ctx context.Context, _ map[string]string) ([]Content, error) {
	client, conn, err := s.terminalClient()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

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

// collectTerminalOutput keeps all stream reads in the calling goroutine. The
// former timeout goroutine appended to a shared byte slice after handlers had
// started reading or returned, which was a data race and retained goroutines
// on quiet streams.
func collectTerminalOutput(ctx context.Context, client cliv0.TerminalServiceClient, input *cliv0.TerminalInput, timeout time.Duration) ([]byte, error) {
	readCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	stream, err := client.Attach(readCtx)
	if err != nil {
		return nil, fmt.Errorf("cannot attach: %w", err)
	}
	// The send half is closed once the read loop has returned, and its error
	// then has nothing left to affect: the read context is cancelled on
	// return, and the output is already collected or refused.
	defer func() { _ = stream.CloseSend() }()
	if err := stream.Send(input); err != nil {
		return nil, fmt.Errorf("cannot send: %w", err)
	}

	var output []byte
	for {
		msg, err := stream.Recv()
		if err != nil {
			switch {
			case errors.Is(err, io.EOF):
				return output, nil
			case readCtx.Err() != nil && ctx.Err() == nil:
				return output, nil
			case ctx.Err() != nil:
				return nil, ctx.Err()
			default:
				return nil, fmt.Errorf("cannot receive: %w", err)
			}
		}
		output = append(output, msg.Data...)
		if msg.Done {
			return output, nil
		}
	}
}

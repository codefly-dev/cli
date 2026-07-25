package mcp

import (
	"encoding/json"

	"github.com/codefly-dev/cli/pkg/toolbox"
)

// JSON-RPC 2.0 types for MCP protocol
const JSONRPCVersion = "2.0"
const MCPProtocolVersion = "2024-11-05"

// JSONRPCRequest represents a JSON-RPC 2.0 request
type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// JSONRPCResponse represents a JSON-RPC 2.0 response
type JSONRPCResponse struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      any           `json:"id,omitempty"`
	Result  any           `json:"result,omitempty"`
	Error   *JSONRPCError `json:"error,omitempty"`
}

// JSONRPCError represents a JSON-RPC 2.0 error
type JSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// MCP error codes
const (
	ParseError     = -32700
	InvalidRequest = -32600
	MethodNotFound = -32601
	InvalidParams  = -32602
	InternalError  = -32603
)

// MCP Initialize types
type InitializeParams struct {
	ProtocolVersion string     `json:"protocolVersion"`
	Capabilities    any        `json:"capabilities"`
	ClientInfo      ClientInfo `json:"clientInfo"`
}

type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type ServerCapabilities struct {
	Tools     *ToolsCapability    `json:"tools,omitempty"`
	Resources *ResourceCapability `json:"resources,omitempty"`
	Prompts   *PromptsCapability  `json:"prompts,omitempty"`
}

type ToolsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

type ResourceCapability struct {
	Subscribe   bool `json:"subscribe,omitempty"`
	ListChanged bool `json:"listChanged,omitempty"`
}

type PromptsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

type InitializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ServerCapabilities `json:"capabilities"`
	ServerInfo      ServerInfo         `json:"serverInfo"`
}

// MCP tool types are aliases of the transport-neutral toolbox contract. The
// JSON-RPC adapter adds no parallel definition or translation layer.
type Tool = toolbox.Tool
type InputSchema = toolbox.InputSchema
type PropertySchema = toolbox.PropertySchema

type ListToolsResult struct {
	Tools []Tool `json:"tools"`
}

type CallToolParams struct {
	Name      string            `json:"name"`
	Arguments map[string]string `json:"arguments,omitempty"`
}

type CallToolResult struct {
	Content []Content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

// MCP Resource types
type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

type ListResourcesResult struct {
	Resources []Resource `json:"resources"`
}

type ReadResourceParams struct {
	URI string `json:"uri"`
}

type ReadResourceResult struct {
	Contents []ResourceContents `json:"contents"`
}

type ResourceContents struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"`
}

// Content types
type Content = toolbox.Content

var TextContent = toolbox.TextContent
var ErrorContent = toolbox.ErrorContent

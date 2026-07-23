package helpprovider

import "encoding/json"

const ProtocolVersion = 1

type Request struct {
	ProtocolVersion int             `json:"protocol_version"`
	Application     string          `json:"application"`
	Command         string          `json:"command"`
	StaticHelp      string          `json:"static_help"`
	Context         json.RawMessage `json:"context"`
}

type Response struct {
	ProtocolVersion int    `json:"protocol_version"`
	Explanation     string `json:"explanation"`
}

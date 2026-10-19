package codex

import (
	"encoding/json"
	"errors"
)

type message struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func parseMessage(line []byte) (message, error) {
	var msg message
	if err := json.Unmarshal(line, &msg); err != nil {
		return msg, err
	}
	if msg.JSONRPC != "" && msg.JSONRPC != "2.0" {
		return msg, errors.New("invalid JSON-RPC version")
	}
	if len(msg.ID) == 0 && msg.Method == "" {
		return msg, errors.New("invalid JSON-RPC message")
	}
	if len(msg.ID) > 0 && msg.Method == "" && len(msg.Result) == 0 && msg.Error == nil {
		return msg, errors.New("invalid JSON-RPC response")
	}
	return msg, nil
}

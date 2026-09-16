package main

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ---- instructions ----
//
// The instructions string is handed to the client once, at connect. That is
// enough for a client that surfaces it to the model, but not for one that
// doesn't: with tool discovery, an agent often meets these tools long after the
// handshake, through a search over tool descriptions, and never sees the
// orientation text at all. So the same text is also reachable as a tool, which
// is the one surface every client exposes.
//
// It returns what the server sent at connect — the string loaded at startup,
// not a fresh read of the file — so the two can never disagree about what this
// vault was said to be. Re-reading would make the tool the more current of the
// two, which is worse: an agent would be working from instructions the rest of
// the session never saw.

type InstructionsInput struct{}

type InstructionsOutput struct {
	Instructions string `json:"instructions"`
}

// Instructions closes over the loaded text rather than reading a package-level
// global, so a test can stand up a server with its own instructions.
func Instructions(text string) func(context.Context, *mcp.CallToolRequest, InstructionsInput) (*mcp.CallToolResult, InstructionsOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in InstructionsInput) (*mcp.CallToolResult, InstructionsOutput, error) {
		return textResult("%s", text), InstructionsOutput{Instructions: text}, nil
	}
}

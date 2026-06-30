package main

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// maxLoggedArgs caps how much of a call's arguments we echo into the log, so a
// big write_file payload doesn't flood it.
const maxLoggedArgs = 300

// loggingMiddleware logs every incoming request. Tool calls are logged with
// their name, a summary of their arguments, timing, and whether they failed —
// both protocol errors and tool-level (IsError) errors.
func loggingMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		start := time.Now()
		res, err := next(ctx, method, req)
		elapsed := time.Since(start).Round(time.Millisecond)

		if method != "tools/call" {
			if err != nil {
				log.Printf("%s -> ERROR: %v (%s)", method, err, elapsed)
			} else {
				log.Printf("%s (%s)", method, elapsed)
			}
			return res, err
		}

		name, args := "?", ""
		// The server receives tool calls as CallToolParamsRaw, whose Arguments
		// are the raw JSON bytes from the wire.
		if p, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok {
			name = p.Name
			args = summarizeArgs(p.Arguments)
		}
		switch {
		case err != nil:
			log.Printf("tool %s %s -> PROTOCOL ERROR: %v (%s)", name, args, err, elapsed)
		case isToolError(res):
			log.Printf("tool %s %s -> FAILED: %s (%s)", name, args, toolErrorText(res), elapsed)
		default:
			log.Printf("tool %s %s -> ok (%s)", name, args, elapsed)
		}
		return res, err
	}
}

// summarizeArgs renders tool arguments as compact JSON, truncated for logging.
func summarizeArgs(args any) string {
	b, err := json.Marshal(args)
	if err != nil {
		return "<unprintable args>"
	}
	s := string(b)
	if len(s) > maxLoggedArgs {
		s = s[:maxLoggedArgs] + "...(truncated)"
	}
	return s
}

// isToolError reports whether res is a tool result that ended in an error.
func isToolError(res mcp.Result) bool {
	r, ok := res.(*mcp.CallToolResult)
	return ok && r.IsError
}

// toolErrorText extracts the first text content from a failed tool result.
func toolErrorText(res mcp.Result) string {
	r, ok := res.(*mcp.CallToolResult)
	if !ok {
		return ""
	}
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			return tc.Text
		}
	}
	return "(no message)"
}

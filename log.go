package main

import (
	"context"
	"encoding/json"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// maxLoggedArgs caps how much of a call's raw arguments we echo when we can't
// extract structured fields. payloadPreview is how many bytes of a read/write
// payload we show.
const (
	maxLoggedArgs  = 300
	payloadPreview = 20
)

// loggingMiddleware logs every incoming request. Tool calls are logged with
// their name, a summary of their arguments (path + payload preview), timing, and
// whether they failed — both protocol errors and tool-level (IsError) errors.
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
			extra := ""
			if isReadTool(name) {
				if pv := resultPreview(res, payloadPreview); pv != "" {
					extra = " read=" + pv
				}
			}
			log.Printf("tool %s %s%s -> ok (%s)", name, args, extra, elapsed)
		}
		return res, err
	}
}

// summarizeArgs surfaces the useful fields from a tool call's arguments — the
// path (always, regardless of JSON key order) and a short preview of any write
// payload — falling back to truncated raw JSON for tools without those fields.
func summarizeArgs(args any) string {
	raw, err := json.Marshal(args)
	if err != nil {
		return "<unprintable args>"
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return truncate(string(raw), maxLoggedArgs)
	}

	var parts []string
	if p, ok := m["path"].(string); ok {
		parts = append(parts, "path="+p)
	}
	if q, ok := m["query"].(string); ok {
		parts = append(parts, "query="+strconv.Quote(q))
	}
	// write payloads: write_file uses "content", edit_file uses "new_string"
	for _, k := range []string{"content", "new_string"} {
		if v, ok := m[k].(string); ok {
			parts = append(parts, k+"="+previewStr(v, payloadPreview))
			break
		}
	}
	if len(parts) == 0 {
		return truncate(string(raw), maxLoggedArgs)
	}
	return strings.Join(parts, " ")
}

// isReadTool reports whether a tool returns file content worth previewing.
func isReadTool(name string) bool {
	return name == "read_file" || name == "read_lines"
}

// resultPreview returns a short preview of the first text content of a result.
func resultPreview(res mcp.Result, n int) string {
	r, ok := res.(*mcp.CallToolResult)
	if !ok {
		return ""
	}
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			return previewStr(tc.Text, n)
		}
	}
	return ""
}

// previewStr quotes s (escaping newlines etc.), truncated to n bytes.
func previewStr(s string, n int) string {
	if len(s) > n {
		return strconv.Quote(s[:n]) + "…"
	}
	return strconv.Quote(s)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "...(truncated)"
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

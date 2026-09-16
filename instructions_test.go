package main

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// connectProbe stands up the real server wiring against an in-memory pair, so
// these tests see the tool exactly as a client would — registration included,
// which is where the "only when there is text" decision lives.
func connectProbe(t *testing.T, instructions string) *mcp.ClientSession {
	t.Helper()
	newRepo(t) // newServer registers tools that expect a valid root

	ctx := context.Background()
	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := newServer(instructions).Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "probe", Version: "0"}, nil).
		Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// The point of the tool is to reach an agent that never saw the handshake, so
// what it returns has to be the same prose, unaltered — not a summary of it.
func TestInstructionsToolReturnsTheSameTextAsConnect(t *testing.T) {
	want := "Read README.md first.\n\n- `search` matches note text\n"
	cs := connectProbe(t, want)

	if got := cs.InitializeResult().Instructions; got != want {
		t.Fatalf("connect instructions differ:\n got %q\nwant %q", got, want)
	}

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "instructions"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("instructions tool failed: %v", res.Content)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected text content, got %T", res.Content[0])
	}
	if text.Text != want {
		t.Errorf("tool altered the instructions:\n got %q\nwant %q", text.Text, want)
	}
}

// With no INSTRUCTIONS.md the server sends nothing at connect, so a tool that
// would answer with nothing is not offered at all.
func TestInstructionsToolIsAbsentWithoutInstructions(t *testing.T) {
	cs := connectProbe(t, "")

	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		if tool.Name == "instructions" {
			t.Error("instructions tool offered although the server has no instructions")
		}
	}
	if len(names) == 0 {
		t.Fatalf("no tools listed at all; got %v", names)
	}
}

// The tool has to be discoverable on its own terms: a client that finds these
// tools by searching descriptions only reaches it through tools/list.
func TestInstructionsToolIsListed(t *testing.T) {
	cs := connectProbe(t, "some instructions\n")

	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		if tool.Name == "instructions" {
			if !strings.Contains(tool.Description, "knowledge base") {
				t.Errorf("description does not say what it is for: %q", tool.Description)
			}
			return
		}
	}
	t.Error("instructions tool missing from tools/list")
}

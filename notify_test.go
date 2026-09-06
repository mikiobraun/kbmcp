package main

import (
	"context"
	"testing"
	"time"

	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A client that connects gets told the tool list may be stale, so one that
// cached the list before a restart refetches instead of calling a parameter
// that has since been renamed. This pins the mechanism, not just the intent:
// the notification is emitted by re-registering a tool, which only works while
// Server.AddTool reports "changed" unconditionally. If a future go-sdk starts
// comparing tools before notifying, the nudge goes silent and this fails.
func TestNewSessionIsToldToRefetchTools(t *testing.T) {
	newRepo(t) // newServer registers tools that expect a valid root

	got := make(chan struct{}, 4)
	client := mcp.NewClient(&mcp.Implementation{Name: "probe", Version: "0"},
		&mcp.ClientOptions{
			ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
				got <- struct{}{}
			},
		})

	ctx := context.Background()
	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := newServer().Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	select {
	case <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("no tools/list_changed after the session initialized")
	}
}

// The notification is only an invalidation — it carries no tool data, so the
// definitions have to come from the client's own tools/list. Check that the
// refetch a client makes in response actually returns the current schema.
func TestRefetchAfterNudgeReturnsCurrentSchema(t *testing.T) {
	newRepo(t)

	ctx := context.Background()
	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := newServer().Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "probe", Version: "0"}, nil).
		Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var search *mcp.Tool
	for _, tool := range res.Tools {
		if tool.Name == "search" {
			search = tool
		}
	}
	if search == nil {
		t.Fatal("search tool missing from tools/list")
	}
	// InputSchema arrives as a decoded value, not a *jsonschema.Schema, so go
	// through JSON rather than asserting a concrete type.
	raw, err := json.Marshal(search.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"regex", "substring"} {
		if _, ok := schema.Properties[want]; !ok {
			t.Errorf("search schema missing %q; got %v", want, keysOf(schema.Properties))
		}
	}
	if _, gone := schema.Properties["query"]; gone {
		t.Error("search schema still advertises the removed 'query' parameter")
	}
}

func keysOf[V any](m map[string]V) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

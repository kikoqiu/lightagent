package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"lightagent/internal/llm"
)

// stubTool is a minimal Tool used to populate the registry in tests.
type stubTool struct {
	name   string
	desc   string
	params map[string]any
	out    string
}

func (s stubTool) Name() string        { return s.name }
func (s stubTool) Description() string { return s.desc }
func (s stubTool) Parameters() map[string]any {
	if s.params == nil {
		return map[string]any{"type": "object"}
	}
	return s.params
}

func (s stubTool) Execute(_ context.Context, args map[string]any) *Result {
	if s.out != "" {
		return OK(s.out)
	}
	b, _ := json.Marshal(args)
	return OK(string(b))
}

const (
	deferredName = "mcp_github_create_issue"
	coreName     = "exec_command"
)

// newDiscoveryRegistry builds a registry with one core tool and one deferred
// (locked) function.
func newDiscoveryRegistry() *Registry {
	r := NewRegistry()
	r.Register(stubTool{name: coreName, desc: "run a shell command"})
	r.RegisterDeferred(stubTool{
		name: deferredName,
		desc: "Create a GitHub issue in a repository",
		params: map[string]any{
			"type":       "object",
			"properties": map[string]any{"title": map[string]any{"type": "string"}},
			"required":   []string{"title"},
		},
	})
	return r
}

// TestDeferredToolsStayOutOfDefinitions verifies deferred tools are never
// declared to the model and never advertised by Names() while locked.
func TestDeferredToolsStayOutOfDefinitions(t *testing.T) {
	reg := newDiscoveryRegistry()

	defs := reg.Definitions()
	if len(defs) != 1 || defs[0].Function.Name != coreName {
		t.Fatalf("definitions = %+v, want only the core tool", defs)
	}
	if names := reg.Names(); len(names) != 1 || names[0] != coreName {
		t.Fatalf("names = %v, want only the core tool while locked", names)
	}
	catalog := reg.DeferredToolCatalog()
	if len(catalog) != 1 || catalog[0].Name != deferredName {
		t.Fatalf("catalog = %+v, want the deferred function", catalog)
	}

	// Once granted, the deferred tool becomes callable but is still not
	// declared (provider tools array stays byte-stable).
	reg.GrantTools([]string{deferredName}, 3)
	if defs := reg.Definitions(); len(defs) != 1 {
		t.Fatalf("granted deferred tool leaked into definitions: %+v", defs)
	}
	if !reg.IsDeferred(deferredName) || reg.IsCoreTool(deferredName) {
		t.Fatal("deferred registration metadata is wrong")
	}
}

// TestBM25DiscoveryIsDiscoveryOnly verifies the search reports matched names
// and descriptions without unlocking anything.
func TestBM25DiscoveryIsDiscoveryOnly(t *testing.T) {
	reg := newDiscoveryRegistry()
	search := NewBM25SearchTool(reg, 5)

	res := search.Execute(context.Background(), map[string]any{"query": "create a github issue"})
	if res.IsError {
		t.Fatalf("search failed: %s", res.ForLLM)
	}
	if !res.Silent {
		t.Fatal("discovery search should be user-silent")
	}
	if !strings.Contains(res.ForLLM, deferredName) || !strings.Contains(res.ForLLM, "Create a GitHub issue") {
		t.Fatalf("search result missing match: %s", res.ForLLM)
	}
	if strings.Contains(res.ForLLM, "<tools>") || strings.Contains(res.ForLLM, "<![CDATA[") {
		t.Fatalf("discovery search must not deliver the schema: %s", res.ForLLM)
	}
	// Nothing was activated.
	if !reg.IsDeferredLocked(deferredName) {
		t.Fatal("search must not grant the matched function")
	}
}

// TestBM25DiscoveryEmptyLibraryAndNoMatch verifies graceful messaging.
func TestBM25DiscoveryEmptyLibraryAndNoMatch(t *testing.T) {
	empty := NewRegistry()
	empty.Register(stubTool{name: coreName, desc: "run a shell command"})
	res := NewBM25SearchTool(empty, 5).Execute(context.Background(), map[string]any{"query": "anything"})
	if res.IsError || !strings.Contains(res.ForLLM, "No locked functions found") {
		t.Fatalf("empty library result = %q (error=%v)", res.ForLLM, res.IsError)
	}

	reg := newDiscoveryRegistry()
	res = NewBM25SearchTool(reg, 5).Execute(context.Background(), map[string]any{"query": "zzzzzqqqq"})
	if res.IsError || !strings.Contains(res.ForLLM, "No locked functions found") {
		t.Fatalf("no-match result = %q (error=%v)", res.ForLLM, res.IsError)
	}

	bad := NewBM25SearchTool(reg, 5).Execute(context.Background(), map[string]any{"query": "  "})
	if !bad.IsError {
		t.Fatal("empty query must be rejected")
	}
}

// TestUnlockThenDynamicCall exercises the full control-plane loop.
func TestUnlockThenDynamicCall(t *testing.T) {
	reg := newDiscoveryRegistry()
	ctx := context.Background()

	unlock := NewUnlockTool(reg, 2)
	res := unlock.Execute(ctx, map[string]any{"name": deferredName})
	if res.IsError {
		t.Fatalf("unlock failed: %s", res.ForLLM)
	}
	if !res.Silent {
		t.Fatal("unlock result should be user-silent")
	}
	for _, want := range []string{
		`<unlock_schema id="` + deferredName + `">`,
		"</unlock_schema>",
		"<tools>",
		"<parameters>",
		"<![CDATA[",
		`"title"`,
		DynamicCallToolName,
	} {
		if !strings.Contains(res.ForLLM, want) {
			t.Fatalf("unlock result missing %q:\n%s", want, res.ForLLM)
		}
	}
	if reg.IsDeferredLocked(deferredName) {
		t.Fatal("unlock must grant the function")
	}

	dc := NewDynamicCallTool(reg)
	out := dc.Execute(ctx, map[string]any{
		"name":      deferredName,
		"arguments": map[string]any{"title": "bug"},
	})
	if out.IsError {
		t.Fatalf("dynamic_call failed: %s", out.ForLLM)
	}
	if !strings.Contains(out.ForLLM, `"title":"bug"`) {
		t.Fatalf("dynamic_call did not forward arguments: %s", out.ForLLM)
	}

	// A legacy string-typed arguments payload is still accepted.
	out = dc.Execute(ctx, map[string]any{"name": deferredName, "arguments": `{"title":"str"}`})
	if out.IsError || !strings.Contains(out.ForLLM, `"title":"str"`) {
		t.Fatalf("string arguments not accepted: %s (error=%v)", out.ForLLM, out.IsError)
	}
}

// TestLockedFunctionRejectedWithoutGrant verifies the self-healing "tool is
// locked" error from both the direct and the dynamic_call paths.
func TestLockedFunctionRejectedWithoutGrant(t *testing.T) {
	reg := newDiscoveryRegistry()
	ctx := context.Background()

	direct := reg.ExecuteArgs(ctx, deferredName, map[string]any{"title": "x"})
	if !direct.IsError || !strings.Contains(direct.ForLLM, "locked") || !strings.Contains(direct.ForLLM, UnlockToolName) {
		t.Fatalf("direct call gate = %q (error=%v)", direct.ForLLM, direct.IsError)
	}

	dc := NewDynamicCallTool(reg)
	out := dc.Execute(ctx, map[string]any{"name": deferredName, "arguments": map[string]any{}})
	if !out.IsError || !strings.Contains(out.ForLLM, "locked") {
		t.Fatalf("dynamic_call gate = %q (error=%v)", out.ForLLM, out.IsError)
	}
}

// TestGrantExpiresAfterTTLTick verifies grants count down per turn while the
// function stays discoverable.
func TestGrantExpiresAfterTTLTick(t *testing.T) {
	reg := newDiscoveryRegistry()
	ctx := context.Background()
	NewUnlockTool(reg, 1).Execute(ctx, map[string]any{"name": deferredName})

	if reg.IsDeferredLocked(deferredName) {
		t.Fatal("grant should be active right after unlock")
	}
	reg.TickTTL()
	if !reg.IsDeferredLocked(deferredName) {
		t.Fatal("grant should expire after one tick")
	}
	if res := reg.ExecuteArgs(ctx, deferredName, map[string]any{}); !res.IsError {
		t.Fatalf("expired grant should reject the call: %q", res.ForLLM)
	}
	// Still discoverable.
	res := NewBM25SearchTool(reg, 5).Execute(ctx, map[string]any{"query": "github issue"})
	if res.IsError || !strings.Contains(res.ForLLM, deferredName) {
		t.Fatalf("locked function should stay discoverable: %q", res.ForLLM)
	}
}

// TestUnlockCoreToolIsNoop verifies core tools are never treated as locked.
func TestUnlockCoreToolIsNoop(t *testing.T) {
	reg := newDiscoveryRegistry()
	res := NewUnlockTool(reg, 3).Execute(context.Background(), map[string]any{"name": coreName})
	if res.IsError || !res.Silent || !strings.Contains(res.ForLLM, "does not need to be unlocked") {
		t.Fatalf("unlock core = %q (error=%v silent=%v)", res.ForLLM, res.IsError, res.Silent)
	}
}

// TestUnlockUnknownToolFails verifies unknown names are rejected.
func TestUnlockUnknownToolFails(t *testing.T) {
	reg := newDiscoveryRegistry()
	res := NewUnlockTool(reg, 3).Execute(context.Background(), map[string]any{"name": "mcp_missing"})
	if !res.IsError || !strings.Contains(res.ForLLM, "does not exist") {
		t.Fatalf("unlock unknown = %q (error=%v)", res.ForLLM, res.IsError)
	}
}

// TestDynamicCallGuards verifies dynamic_call refuses core and control-plane
// tools and unknown names.
func TestDynamicCallGuards(t *testing.T) {
	reg := newDiscoveryRegistry()
	ctx := context.Background()
	// The control-plane tools are registered in the real assembly, so register
	// them here to exercise the guard against invoking them indirectly.
	reg.Register(NewUnlockTool(reg, 3))
	dc := NewDynamicCallTool(reg)
	reg.Register(dc)

	for _, name := range []string{DynamicCallToolName, UnlockToolName} {
		res := dc.Execute(ctx, map[string]any{"name": name})
		if !res.IsError || !strings.Contains(res.ForLLM, "control-plane") {
			t.Fatalf("dynamic_call %q = %q (error=%v)", name, res.ForLLM, res.IsError)
		}
	}
	if res := dc.Execute(ctx, map[string]any{"name": coreName}); !res.IsError || !strings.Contains(res.ForLLM, "native") {
		t.Fatalf("dynamic_call core = %q (error=%v)", res.ForLLM, res.IsError)
	}
	if res := dc.Execute(ctx, map[string]any{"name": "nope"}); !res.IsError || !strings.Contains(res.ForLLM, "does not exist") {
		t.Fatalf("dynamic_call unknown = %q (error=%v)", res.ForLLM, res.IsError)
	}
	if res := dc.Execute(ctx, map[string]any{"name": ""}); !res.IsError {
		t.Fatal("dynamic_call with empty name must fail")
	}
}

// TestUnlockSkipsSchemaWhenRecordInContext verifies the in-context schema
// dedupe path.
func TestUnlockSkipsSchemaWhenRecordInContext(t *testing.T) {
	reg := newDiscoveryRegistry()
	ctx := WithUnlockLookup(context.Background(), func(name string) bool { return name == deferredName })

	res := NewUnlockTool(reg, 4).Execute(ctx, map[string]any{"name": deferredName})
	if res.IsError {
		t.Fatalf("unlock failed: %s", res.ForLLM)
	}
	if strings.Contains(res.ForLLM, "<tools>") {
		t.Fatalf("schema should not be resent when the record is in context: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, UnlockRecordReference(deferredName)) {
		t.Fatalf("result should reference the existing record: %s", res.ForLLM)
	}
	if reg.IsDeferredLocked(deferredName) {
		t.Fatal("the grant should still be refreshed")
	}
}

// TestMessagesContainUnlockRecord verifies the message scan used by the agent.
func TestMessagesContainUnlockRecord(t *testing.T) {
	msgs := []llm.Message{
		{Role: "user", Content: "hello"},
		{Role: "tool", Content: "Tool " + deferredName + " is now UNLOCKED:\n" + unlockRecordOpen(deferredName)},
	}
	if !MessagesContainUnlockRecord(msgs, deferredName) {
		t.Fatal("record should be detected")
	}
	if MessagesContainUnlockRecord(msgs, "other") {
		t.Fatal("unrelated name must not match")
	}
}

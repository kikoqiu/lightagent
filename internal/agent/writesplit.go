package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"lightagent/internal/llm"
	"lightagent/internal/tools"
)

// writeFileToolName is the writer whose oversized payloads the loop spreads over
// several calls.
const writeFileToolName = "write_file"

// autoSplitWriteCalls prepares the write_file calls of one model reply for the
// auto-split feature. A call whose payload is longer than the per-call limit
// keeps its first part, and the remaining parts are returned as the follow-up
// calls the loop makes itself (see runWriteSplits), keyed by the index of the
// call they continue.
//
// Calls that fit are left untouched, and so is every call when the feature is
// off or the registered writer cannot plan a split. The rewrite happens before
// the assistant message is recorded, so the history — and with it every
// front-end and the next request — shows the arguments that were executed.
func (a *Agent) autoSplitWriteCalls(calls []llm.ToolCall) map[int][]llm.ToolCall {
	if !a.autoSplitWrites || len(calls) == 0 || a.reg == nil {
		return nil
	}
	registered, ok := a.reg.Get(writeFileToolName)
	if !ok {
		return nil
	}
	splitter, ok := registered.(tools.AutoSplitWriteTool)
	if !ok {
		return nil
	}

	var continuations map[int][]llm.ToolCall
	for i := range calls {
		call := &calls[i]
		if call.Function.Name != writeFileToolName {
			continue
		}
		args := map[string]any{}
		if trimmed := strings.TrimSpace(call.Function.Arguments); trimmed != "" {
			if err := json.Unmarshal([]byte(trimmed), &args); err != nil {
				// A malformed call is reported by the tool itself.
				continue
			}
		}
		planned, split := splitter.PlanWriteCalls(args)
		if !split || len(planned) < 2 {
			continue
		}
		first, err := encodeWriteArgs(planned[0])
		if err != nil {
			continue
		}
		partType := call.Type
		if partType == "" {
			partType = "function"
		}
		rest := make([]llm.ToolCall, 0, len(planned)-1)
		for part, next := range planned[1:] {
			encoded, err := encodeWriteArgs(next)
			if err != nil {
				// Half a chain would split the payload wrongly, so the call is
				// left to the tool exactly as it came in.
				rest = nil
				break
			}
			rest = append(rest, llm.ToolCall{
				ID:       splitCallID(call.ID, part+2),
				Type:     partType,
				Function: llm.ToolCallFunction{Name: call.Function.Name, Arguments: encoded},
			})
		}
		if len(rest) == 0 {
			continue
		}
		call.Function.Arguments = first
		if continuations == nil {
			continuations = make(map[int][]llm.ToolCall, 1)
		}
		continuations[i] = rest
	}
	return continuations
}

// runWriteSplits runs the follow-up calls of an auto-split write. The parts are
// not separate replies: attachSplitCalls adds them to the assistant message that
// requested the write (the one the model produced), so the whole split travels as
// a single assistant message carrying several tool calls — the shape a provider
// expects for a reply that asks for several calls at once, and the way a
// preserve-thinking provider such as DeepSeek keeps that message's reasoning with
// every call of the split. It reports whether the turn ran to the end (false when
// the user cancelled it).
//
// A write_file whose payload exceeds the per-call limit is simply spread over
// several write calls, so these parts are ordinary calls of the same batch the
// model issued in that one reply — they get no treatment of their own on an
// interrupt: the part in flight is answered with its real result and the parts
// after it, like any call that never started, are answered as interrupted (see
// the interrupt rules in docs/architecture.md).
func (a *Agent) runWriteSplits(ctx context.Context, owner int, calls []llm.ToolCall) bool {
	if len(calls) == 0 {
		return true
	}
	a.attachSplitCalls(owner, calls)
	for i, call := range calls {
		if ctx.Err() != nil {
			a.reportInterruptedTools(calls, i)
			return false
		}
		a.dispatchToolCall(ctx, call)
		a.bus.Publish(a.usageEvent())
	}
	return true
}

// attachSplitCalls appends the follow-up calls of an auto-split write to the
// assistant message that owns the write, so that one message lists every call of
// the split. The parts go after the calls the model itself made, which is the
// order they run in (the model's batch first, then the parts), and that keeps
// every call answered by the tool message that follows it.
func (a *Agent) attachSplitCalls(owner int, calls []llm.ToolCall) {
	if len(calls) == 0 {
		return
	}
	a.mu.Lock()
	if owner >= 0 && owner < len(a.history) {
		a.history[owner].ToolCalls = append(a.history[owner].ToolCalls, calls...)
	}
	a.mu.Unlock()
}

// encodeWriteArgs renders one planned call's arguments back into the JSON string
// a tool call carries. HTML escaping is off so the payload stays readable in the
// transcript and in the front-ends.
func encodeWriteArgs(args map[string]any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(args); err != nil {
		return "", err
	}
	return strings.TrimSuffix(buf.String(), "\n"), nil
}

// splitCallID derives the id of a synthesized continuation call from the id of
// the call the model made, so the chain stays recognizable in the transcript.
func splitCallID(parent string, part int) string {
	if strings.TrimSpace(parent) == "" {
		parent = writeFileToolName
	}
	return fmt.Sprintf("%s_part%d", parent, part)
}

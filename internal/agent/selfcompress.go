package agent

import (
	"context"
	"fmt"
	"strings"

	"lightagent/internal/llm"
)

// Self-compression: a tool whose result is a large body (webfetch returns a
// whole page as markdown) asks the engine to condense that result before it
// becomes part of the context. The answers of a round are recorded as usual —
// ordinary tool messages, exactly the text the tool returned — and the pass
// follows the ordinary tool-call flow with one addition: once the whole round is
// recorded, an engine user message per answer asks the model to report that
// answer back in condensed form. The instruction names the call it is about,
// because several answers (several webfetch calls, say) can sit in the context at
// once. When every answer that could be condensed has come back, the pass is
// rolled back (its instructions and replies) and each tool message is recorded
// again with the condensed content. The conversation then continues with small
// tool results, while the front-ends keep the rows they already showed.
const (
	// compressedContentOpen and compressedContentClose mark the block the
	// compression reply has to put the condensed content in.
	compressedContentOpen  = "<compressed-content>"
	compressedContentClose = "</compressed-content>"

	// maxCallArgumentsChars caps the arguments quoted by an instruction: they
	// are there to identify the call, not to restate a payload.
	maxCallArgumentsChars = 200
)

// selfCompressInstruction renders the user message the pass appends behind the
// answers of a round. It names the call whose answer is to be condensed — one
// round can answer several calls, so all the instructions must say which answer
// they are about — and demands nothing but the wrapped content, so that whatever
// comes back can be used as it is.
func selfCompressInstruction(tc llm.ToolCall) string {
	return fmt.Sprintf("[engine] Among the tool answers above, condense the content returned by the %s call. "+
		"Pause the processing of that content for now, keep the formatting, and drop advertising, "+
		"navigation and everything else that is irrelevant. Report the complete condensed content in "+
		"your next message, wrapped in %s%s, and write nothing else: no explanation, no commentary, "+
		"nothing outside the block.", callReference(tc), compressedContentOpen, compressedContentClose)
}

// selfCompressRetryInstruction renders the message that asks for the complete
// content once more, naming the call for the same reason.
func selfCompressRetryInstruction(tc llm.ToolCall) string {
	return fmt.Sprintf("[engine] Your last message about the content returned by the %s call did not follow "+
		"the required format: the complete condensed content has to be the whole message, wrapped in "+
		"%s%s, with nothing outside the block. Send it again now, in that format only.",
		callReference(tc), compressedContentOpen, compressedContentClose)
}

// callReference names a tool call the way the instructions refer to it: the
// function name with the arguments it was called with (on one line, cut when the
// payload is long) and the call id when the provider sent one. Several answers
// can sit in the conversation at once, so this is what tells them apart.
func callReference(tc llm.ToolCall) string {
	name := strings.TrimSpace(tc.Function.Name)
	if name == "" {
		name = "an unnamed tool"
	}
	reference := name
	if args := abbreviateArguments(tc.Function.Arguments); args != "" {
		reference += " " + args
	}
	if id := strings.TrimSpace(tc.ID); id != "" {
		reference += " (call id " + id + ")"
	}
	return reference
}

// abbreviateArguments returns the arguments of a call on one line, cut to a
// readable length, or "" when the call carries none.
func abbreviateArguments(args string) string {
	compact := strings.Join(strings.Fields(args), " ")
	if compact == "" {
		return ""
	}
	if len(compact) > maxCallArgumentsChars {
		compact = compact[:maxCallArgumentsChars] + "…"
	}
	return compact
}

// extractCompressedContent returns the content of a compression reply's block
// and whether the reply followed the requested format. A reply without the
// block, with the closing tag before the opening one, or with an empty block is
// malformed and asks for another attempt.
func extractCompressedContent(reply string) (string, bool) {
	start := strings.Index(reply, compressedContentOpen)
	if start < 0 {
		return "", false
	}
	rest := reply[start+len(compressedContentOpen):]
	end := strings.Index(rest, compressedContentClose)
	if end < 0 {
		return "", false
	}
	content := strings.TrimSpace(rest[:end])
	if content == "" {
		return "", false
	}
	return content, true
}

// compressRequest is one recorded tool answer the loop wants condensed, together
// with how many times its pass may ask again after a malformed reply.
type compressRequest struct {
	call    llm.ToolCall
	retries int
}

// compressToolResults condenses the tool answers of one round that asked for it —
// the ones that did not are left exactly as they are. The answers are already
// recorded as ordinary tool messages, so the pass walks the requests in the order
// the calls were made, one at a time: each one appends its own instruction —
// naming the call it is about, which is what keeps several answers in the same
// context apart — asks the model for the condensed content and remembers it. Once
// every answer that could be condensed has come back, the pass is rolled back
// (its instructions and replies) and each tool message is recorded again with the
// condensed content in its place.
//
// A malformed reply is asked again, at most req.retries times per answer;
// reaching the limit keeps that answer as it is, which costs context but loses
// nothing. The pass reports its progress on the bus (an info line before each
// answer, one when it gives up, one when it succeeded), so a model call that can
// take a while is not silent. It reports whether the turn is still running
// (false = cancelled while asking).
func (a *Agent) compressToolResults(ctx context.Context, requests []compressRequest) bool {
	if a.client == nil || len(requests) == 0 {
		return true
	}
	// Everything the pass appends lands behind the answers of the round, so
	// rolling back to this point removes every instruction and every reply of
	// the pass, whichever way the pass ends. The tool messages themselves sit in
	// front of that point: each one is recorded again, in place, with the
	// condensed content.
	rollback := a.historyLen()
	defer a.truncateHistory(rollback)

	type condensedAnswer struct {
		call    llm.ToolCall
		content string
	}
	var condensed []condensedAnswer
	for _, req := range requests {
		content, ok, running := a.condenseAnswer(ctx, req)
		if !running {
			return false
		}
		if ok {
			condensed = append(condensed, condensedAnswer{call: req.call, content: content})
		}
	}

	// Every answer that could be condensed is in: the round keeps one answer per
	// call, and the short one takes the place of the full one.
	for _, answer := range condensed {
		a.replaceToolMessage(answer.call, answer.content)
	}
	return true
}

// condenseAnswer runs the self-compression pass of one recorded tool answer. It
// returns the condensed content, whether the reply finally followed the
// requested format, and whether the turn is still running.
func (a *Agent) condenseAnswer(ctx context.Context, req compressRequest) (string, bool, bool) {
	retries := req.retries
	if retries < 0 {
		retries = 0
	}
	reference := callReference(req.call)
	a.bus.Publish(Event{Type: EventInfo, Text: fmt.Sprintf(
		"condensing the answer of the %s call", reference)})

	a.appendMessage(llm.Message{Role: "user", Content: selfCompressInstruction(req.call)})
	for attempt := 0; ; attempt++ {
		reply, err := a.callInternalLLM(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return "", false, false
			}
			a.bus.Publish(Event{Type: EventError, Text: "self-compression: " + err.Error()})
			a.bus.Publish(Event{Type: EventInfo, Text: fmt.Sprintf(
				"the answer of the %s call is kept as it is", reference)})
			return "", false, true
		}
		// The reply joins the history so that a retry builds on what the model
		// answered. The whole pass is rolled back when it ends, either way.
		a.appendMessage(llm.Message{Role: "assistant", Content: reply})

		if content, ok := extractCompressedContent(reply); ok {
			a.bus.Publish(Event{Type: EventInfo, Text: fmt.Sprintf(
				"the answer of the %s call was condensed to %d chars", reference, len(content))})
			return content, true, true
		}

		if attempt >= retries {
			a.bus.Publish(Event{Type: EventInfo, Text: fmt.Sprintf(
				"self-compression got %d reply/replies for the %s call that were not wrapped in %s; "+
					"its answer is kept as it is", attempt+1, reference, compressedContentOpen)})
			return "", false, true
		}
		a.bus.Publish(Event{Type: EventInfo, Text: fmt.Sprintf(
			"self-compression reply %d for the %s call did not follow the required format; asking again (%d/%d)",
			attempt+1, reference, attempt+1, retries)})
		a.appendMessage(llm.Message{Role: "user", Content: selfCompressRetryInstruction(req.call)})
	}
}

// callInternalLLM sends the live conversation and returns the reply text. It
// carries no tools and publishes no streaming events: it is the engine asking
// for engine work, and nothing the model answers with stays in the conversation.
func (a *Agent) callInternalLLM(ctx context.Context) (string, error) {
	a.mu.Lock()
	msgs := a.buildMessagesLocked()
	a.mu.Unlock()

	resp, err := a.client.Chat(ctx, msgs, nil, nil, nil)
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

// truncateHistory drops every message recorded after index.
func (a *Agent) truncateHistory(index int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if index >= 0 && index < len(a.history) {
		a.history = a.history[:index]
	}
}

// replaceToolMessage records the given content as the answer of a tool call,
// taking the place of the full answer recorded earlier: the assistant message's
// tool_call keeps exactly one answer — the condensed one — and every other
// message of the round stays where it is. A call without a recorded answer is
// left alone.
func (a *Agent) replaceToolMessage(call llm.ToolCall, content string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := len(a.history) - 1; i >= 0; i-- {
		if a.history[i].Role == "tool" && a.history[i].ToolCallID == call.ID {
			a.history[i].Content = content
			return
		}
	}
}

package agent

import (
	"sync"
	"time"

	"lightagent/internal/llm"
)

// EventType enumerates the observable agent events.
type EventType string

// Event types published on the bus. CLI and web subscribe to render them.
const (
	EventUser           EventType = "user"
	EventAssistantDelta EventType = "assistant_delta"
	// EventReasoningDelta carries the model's incremental "thinking" text when
	// the provider exposes it. It precedes the visible answer and is rendered
	// by the frontends; the assembled reasoning is stored on the assistant
	// message (llm.Message.ReasoningContent) and sent back on later requests.
	EventReasoningDelta EventType = "reasoning_delta"
	EventAssistant      EventType = "assistant"
	EventToolCall       EventType = "tool_call"
	EventToolResult     EventType = "tool_result"
	EventInfo           EventType = "info"
	EventError          EventType = "error"
	EventCompacted      EventType = "compacted"
	EventUsage          EventType = "usage"
	EventTurnDone       EventType = "turn_done"
	// EventInterrupted reports that a running turn was cancelled by the user.
	EventInterrupted EventType = "interrupted"
	// EventPending reports a user message that has been queued (steering) and
	// not yet folded into the conversation. Front-ends draw it as a pending row
	// right away; the matching user event settles that row (see Event.ID).
	EventPending EventType = "pending"
	// EventTurnStart reports that a turn began. It carries no row of its own: it
	// exists for the turns that start without a user message (a continue after a
	// pause), so every front-end lights its running indicator.
	EventTurnStart EventType = "turn_start"
	// EventPaused reports that the pipeline paused: the round in flight finished
	// (its tool calls included) and the turn ends without asking the model
	// again. The queued messages stay queued until the user sends something.
	EventPaused EventType = "paused"
)

// Event is a single observable occurrence during a turn.
type Event struct {
	Type    EventType `json:"type"`
	Text    string    `json:"text,omitempty"`
	Name    string    `json:"name,omitempty"`
	Args    string    `json:"args,omitempty"`
	IsError bool      `json:"is_error,omitempty"`
	// Summary carries the accumulated context summary that replaced the
	// compressed messages. It is set on compacted events so every front-end
	// can show the summary exactly where the transcript was truncated,
	// without reading the agent's own state.
	Summary string `json:"summary,omitempty"`
	// Source identifies who submitted a user message ("cli", "web"). It is
	// empty for events that do not originate from user input.
	Source string `json:"source,omitempty"`
	// ID identifies a queued user message: the pending event that announces it
	// and the user event that folds it into the conversation carry the same
	// value, so a front-end can settle the pending row it drew (see pushSteer
	// and drainSteering).
	ID string `json:"id,omitempty"`
	// Pause marks a pending event that is a pause request rather than a message
	// (see Agent.Pause).
	Pause bool `json:"pause,omitempty"`
	// Attachments lists the files a user message carries (the web mirror's
	// attachments). It holds names and paths only — never the payload — so a
	// front-end can show what a message brought along.
	Attachments []llm.Attachment `json:"attachments,omitempty"`
	// Tokens and ContextWindow carry context-usage numbers for EventUsage.
	Tokens        int       `json:"tokens,omitempty"`
	ContextWindow int       `json:"context_window,omitempty"`
	Time          time.Time `json:"time"`
}

// Bus is a simple broadcast event bus. Subscribers receive events on buffered
// channels; slow subscribers drop events rather than blocking the agent.
type Bus struct {
	mu     sync.RWMutex
	subs   map[int]chan Event
	nextID int
}

// NewBus creates an empty bus.
func NewBus() *Bus {
	return &Bus{subs: make(map[int]chan Event)}
}

// Subscribe registers a listener and returns the channel plus a cancel func.
func (b *Bus) Subscribe() (<-chan Event, func()) {
	b.mu.Lock()
	id := b.nextID
	b.nextID++
	ch := make(chan Event, 512)
	b.subs[id] = ch
	b.mu.Unlock()

	cancel := func() {
		b.mu.Lock()
		if existing, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(existing)
		}
		b.mu.Unlock()
	}
	return ch, cancel
}

// Publish delivers e to every subscriber without blocking.
func (b *Bus) Publish(e Event) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.subs {
		select {
		case ch <- e:
		default:
			// Subscriber is behind; drop to keep the agent responsive.
		}
	}
}

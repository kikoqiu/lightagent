// Package llm implements a minimal OpenAI-compatible chat client.
//
// It only depends on the standard library and speaks the /chat/completions
// protocol, including tool calls and SSE streaming.
package llm

// Message is a single chat message in the OpenAI schema.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
	// ReasoningContent is the model's thinking text for providers that expose
	// it (reasoning_content). It is stored with the assistant message and sent
	// back on later requests, so a provider with a preserve-thinking chat
	// template can see the model's own earlier reasoning.
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

// ToolCall is a function call requested by the model.
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction carries the tool name and its raw JSON arguments.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolDef is a tool definition advertised to the model.
type ToolDef struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction describes one callable function.
type ToolFunction struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

// Usage reports token accounting from the provider.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Response is the assembled result of one chat completion call.
type Response struct {
	Content string
	// Reasoning is the model's "thinking" text, when the provider emits it
	// (e.g. reasoning_content or reasoning). It is carried onto the assistant
	// message (see Message.ReasoningContent) so it is preserved and sent back.
	Reasoning string
	ToolCalls []ToolCall
	Usage     Usage
	Finish    string
}

// ChatRequest is the request body sent to /chat/completions.
type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Tools       []ToolDef `json:"tools,omitempty"`
	ToolChoice  string    `json:"tool_choice,omitempty"`
	Temperature float64   `json:"temperature,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Stream      bool      `json:"stream,omitempty"`
}

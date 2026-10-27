package tools

import (
	"encoding/json"
	"fmt"
	"time"
)

// Structured command status values.
const (
	statusCompleted = "completed"
	statusRunning   = "running"
	statusFailed    = "failed"
	// statusInterrupted marks a call the user cut short: run_script's process
	// tree is terminated and the output it had produced until then is reported
	// with this status.
	statusInterrupted = "interrupted"
)

// commandResult is the unified JSON contract returned by run_script and
// manage_session.
type commandResult struct {
	Status         string  `json:"status"`
	ExitCode       *int    `json:"exit_code"`
	SessionID      *string `json:"session_id"`
	Output         string  `json:"output"`
	Truncated      bool    `json:"truncated"`
	TotalLines     int     `json:"total_lines"`
	TotalBytes     int     `json:"total_bytes"`
	ElapsedSeconds float64 `json:"elapsed_seconds"`
	Warning        *string `json:"warning"`
}

// marshal serializes the structured result, always yielding valid JSON.
func (cr commandResult) marshal() string {
	data, err := json.Marshal(cr)
	if err != nil {
		return fmt.Sprintf(
			`{"status":"failed","exit_code":null,"session_id":null,"output":%q,"truncated":false,"total_lines":0,"total_bytes":0,"elapsed_seconds":0,"warning":null}`,
			"failed to serialize command result: "+err.Error(),
		)
	}
	return string(data)
}

// resultWording is the phrasing one tool reports an outcome with: run_script
// names the script it ran, manage_session the session it acted on. The statuses
// are the same, only the subject of the sentence differs.
type resultWording struct {
	running     func(sessionID string) string
	interrupted string
	failed      string
	completed   string
}

var (
	// scriptWording is run_script's: the call ran a script.
	scriptWording = resultWording{
		running: func(sessionID string) string {
			return fmt.Sprintf("Script is running in session %s (poll with manage_session)", sessionID)
		},
		interrupted: "Script interrupted by user.",
		failed:      "Script failed.",
		completed:   "Script completed.",
	}
	// sessionWording is manage_session's: the call acted on a background
	// session (polled, fed input, killed or listed it), so the sentence is about
	// the session rather than about the script that started it.
	sessionWording = resultWording{
		running: func(sessionID string) string {
			return fmt.Sprintf("Session %s is still running (poll with manage_session)", sessionID)
		},
		interrupted: "Session operation interrupted by user.",
		failed:      "Session operation failed.",
		completed:   "Session operation completed.",
	}
)

// wordingForTool returns the phrasing of the tool that produced a result, read
// back from the tool name a persisted tool message carries.
func wordingForTool(toolName string) resultWording {
	if toolName == "manage_session" {
		return sessionWording
	}
	return scriptWording
}

// toResult converts a structured result into a tool Result, phrased in the
// subject of the tool that produced it.
func (cr commandResult) toResult(wording resultWording) *Result {
	res := &Result{
		ForLLM:  cr.marshal(),
		IsError: cr.Status == statusFailed || cr.Status == statusInterrupted,
	}
	switch cr.Status {
	case statusRunning:
		res.ForUser = wording.running(derefString(cr.SessionID))
	case statusInterrupted:
		res.ForUser = wording.interrupted
	case statusFailed:
		res.ForUser = wording.failed
	default:
		res.ForUser = wording.completed
	}
	// The warning explains a state the status alone cannot: the poll the user
	// interrupted, for instance, leaves the process running.
	if cr.Warning != nil && *cr.Warning != "" {
		res.ForUser += "\n" + *cr.Warning
	}
	if cr.Output != "" {
		res.ForUser += "\n" + cr.Output
	}
	return res
}

// toResultInfo is like toResult but never marks the call as an error. Used by
// manage_session poll/list that merely observe session state.
func (cr commandResult) toResultInfo(wording resultWording) *Result {
	res := cr.toResult(wording)
	res.IsError = false
	return res
}

// commandResultFields are the keys commandResult.marshal always emits (the
// struct has no omitempty). Requiring all of them keeps unrelated JSON (such as
// an MCP payload that happens to be an object) from being mistaken for a
// persisted command result.
var commandResultFields = []string{
	"status", "exit_code", "session_id", "output",
	"truncated", "total_lines", "total_bytes", "elapsed_seconds", "warning",
}

// RenderStoredResult recovers the user-facing rendering of a persisted
// tool-result message. Command tools store their structured JSON contract
// (see commandResult) rather than the display text, so it is reformatted here,
// phrased after the tool named by toolName (run_script and manage_session word
// the same statuses differently); any other tool result carries no user-facing
// rendering and reports ok=false, matching the live view where such results are
// not displayed.
func RenderStoredResult(content, toolName string) (text string, isError, ok bool) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(content), &raw); err != nil {
		return "", false, false
	}
	for _, key := range commandResultFields {
		if _, present := raw[key]; !present {
			return "", false, false
		}
	}
	var cr commandResult
	if err := json.Unmarshal([]byte(content), &cr); err != nil {
		return "", false, false
	}
	res := cr.toResult(wordingForTool(toolName))
	return res.ForUser, res.IsError, true
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func intPtr(v int) *int       { return &v }
func strPtr(s string) *string { return &s }

// elapsedSeconds returns rounded wall-clock seconds since start.
func elapsedSeconds(start time.Time) float64 {
	return float64(time.Since(start).Nanoseconds()) / 1e9
}

// sanitizeAndFold runs the output governance pipeline.
func sanitizeAndFold(raw string, maxLines, maxChars int) (shown, clean string, truncated bool) {
	clean = normalizeTerminalOutput(raw)
	shown, truncated = foldOutput(clean, maxLines, maxChars)
	return shown, clean, truncated
}

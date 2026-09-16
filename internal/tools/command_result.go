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
)

// commandResult is the unified JSON contract returned by exec_command and
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

// toResult converts a structured result into a tool Result.
func (cr commandResult) toResult() *Result {
	res := &Result{
		ForLLM:  cr.marshal(),
		IsError: cr.Status == statusFailed,
	}
	switch {
	case cr.Status == statusRunning:
		res.ForUser = fmt.Sprintf("Command is running in session %s (poll with manage_session)", derefString(cr.SessionID))
	case cr.Status == statusFailed:
		res.ForUser = "Command failed."
	default:
		res.ForUser = "Command completed."
	}
	if cr.Output != "" {
		res.ForUser += "\n" + cr.Output
	}
	return res
}

// toResultInfo is like toResult but never marks the call as an error. Used by
// manage_session poll/list that merely observe session state.
func (cr commandResult) toResultInfo() *Result {
	res := cr.toResult()
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
// (see commandResult) rather than the display text, so it is reformatted here;
// any other tool result carries no user-facing rendering and reports ok=false,
// matching the live view where such results are not displayed.
func RenderStoredResult(content string) (text string, isError, ok bool) {
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
	res := cr.toResult()
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

package slash

import (
	"fmt"
	"strings"

	"lightagent/internal/agent"
)

// Table renders the catalogue as an aligned listing, one command per line: a
// two-space indent, the usage column padded to the widest entry, then the
// summary. paint, when not nil, assembles one row from its name column and the
// text that follows it (the CLI passes a colouring function; the mirror page
// passes nil for plain text).
func Table(paint func(name, rest string) string) string {
	width := 0
	for _, c := range Commands {
		if n := len(c.Usage()); n > width {
			width = n
		}
	}
	var b strings.Builder
	for _, c := range Commands {
		usage := c.Usage()
		rest := strings.Repeat(" ", width-len(usage)+2) + c.Summary
		line := usage + rest
		if paint != nil {
			line = paint(usage, rest)
		}
		b.WriteString("  " + line + "\n")
	}
	return b.String()
}

// TerminalOnly returns the commands only the terminal REPL can run, so the
// mirror page can point at them in its own /help output.
func TerminalOnly() []Command {
	var out []Command
	for _, c := range Commands {
		if !c.Web {
			out = append(out, c)
		}
	}
	return out
}

// WebCommands returns the commands a browser can run, in catalogue order.
// Terminal-only commands are left out, so the page never offers a command it
// cannot run; the page keeps the Primary ones visible and folds the rest behind
// its title.
func WebCommands() []Command {
	out := make([]Command, 0, len(Commands))
	for _, c := range Commands {
		if c.Web {
			out = append(out, c)
		}
	}
	return out
}

// UsageText renders the context-usage line behind /history: message count,
// estimated tokens, the share of the context window and the last
// provider-reported prompt size.
func UsageText(st agent.Stats) string {
	pct := 0.0
	if st.ContextWindow > 0 {
		pct = float64(st.EstimatedTok) * 100 / float64(st.ContextWindow)
	}
	out := fmt.Sprintf("%d messages, ~%d tokens, context usage %.1f%% (~%d/%d)%s",
		st.Messages, st.EstimatedTok, pct, st.EstimatedTok, st.ContextWindow, summarySuffix(st.Summary))
	if st.UsageTokens > 0 {
		out += fmt.Sprintf(", last api prompt %d tokens", st.UsageTokens)
	}
	return out
}

// summarySuffix reports whether a context summary is present.
func summarySuffix(summary string) string {
	if strings.TrimSpace(summary) == "" {
		return ""
	}
	return ", compressed summary present"
}

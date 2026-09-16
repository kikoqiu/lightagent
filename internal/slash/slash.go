// Package slash is the shared catalogue of the REPL's slash commands. The
// terminal REPL and the web mirror offer the same commands, so their names,
// aliases, argument hints and one-line summaries live here instead of being
// copied into both front-ends: the CLI draws its /help table from the catalogue
// and the mirror page draws its side rail from the very same list (injected as
// JSON), so the two can never drift apart.
package slash

import (
	"strings"
)

// Command describes one slash command.
type Command struct {
	// Name is the primary spelling, leading slash included.
	Name string `json:"name"`
	// Aliases are the alternative spellings, also with the leading slash.
	Aliases []string `json:"aliases,omitempty"`
	// Args is the argument hint shown after the name ("[on|off]"); empty when
	// the command takes no argument.
	Args string `json:"args,omitempty"`
	// Summary is the one-line description.
	Summary string `json:"summary"`
	// Web reports whether the browser mirror runs the command itself. Commands
	// that only make sense in a terminal (/exit) are still described by the
	// page's /help, but the side rail does not offer them.
	Web bool `json:"web,omitempty"`
	// Primary keeps the command visible in the mirror's command rail; the other
	// ones start folded behind the rail's title, so the rail stays short.
	Primary bool `json:"primary,omitempty"`
}

// Names joins the primary name with its aliases, e.g. "/stop, /interrupt".
func (c Command) Names() string {
	if len(c.Aliases) == 0 {
		return c.Name
	}
	return c.Name + ", " + strings.Join(c.Aliases, ", ")
}

// Usage is the name column of a listing: Names plus the argument hint.
func (c Command) Usage() string {
	if c.Args == "" {
		return c.Names()
	}
	return c.Names() + " " + c.Args
}

// Commands is the catalogue, in the order the front-ends list it. Every
// front-end looks commands up by their canonical name, so the aliases never
// need a case of their own. The order is also the order of the mirror page's
// command rail, which shows the Primary entries and folds the rest.
var Commands = []Command{
	{Name: "/help", Aliases: []string{"/?"}, Summary: "show the command list", Web: true, Primary: true},
	{Name: "/new", Summary: "start a new conversation (clears the session)", Web: true, Primary: true},
	{Name: "/save", Summary: "write the current conversation to disk now", Web: true, Primary: true},
	{Name: "/stop", Aliases: []string{"/interrupt"}, Summary: "interrupt the turn that is running", Web: true, Primary: true},
	{Name: "/compact", Summary: "compress the context now", Web: true},
	{Name: "/history", Aliases: []string{"/context"}, Summary: "show message/token usage and context usage", Web: true},
	{Name: "/result", Aliases: []string{"/results"}, Args: "[on|off]", Summary: "show or hide tool/exec results (default on)", Web: true},
	{Name: "/markdown", Args: "[on|off]", Summary: "toggle markdown rendering", Web: true},
	{Name: "/exit", Aliases: []string{"/quit", "/q"}, Summary: "quit (you are asked whether to save)"},
}

// Normalize folds the full-width characters an East Asian IME produces
// ("／？") onto their ASCII equivalents so "／？" works like "/?".
func Normalize(s string) string {
	return strings.NewReplacer("／", "/", "？", "?").Replace(s)
}

// IsCommandLine reports whether line starts a slash command.
func IsCommandLine(line string) bool {
	return strings.HasPrefix(line, "/") || strings.HasPrefix(line, "／")
}

// Canonical maps a command (or one of its aliases) onto its primary name. It
// reports false when the catalogue does not know the spelling, in which case
// the normalized input is returned so the caller can echo it back.
func Canonical(name string) (string, bool) {
	name = strings.ToLower(Normalize(name))
	for _, c := range Commands {
		if c.Name == name {
			return c.Name, true
		}
		for _, alias := range c.Aliases {
			if alias == name {
				return c.Name, true
			}
		}
	}
	return name, false
}

// Split parses one submitted slash line into the canonical command name and its
// arguments. ok is false for a command the catalogue does not know; the name is
// then the normalized spelling the user typed.
func Split(line string) (name string, args []string, ok bool) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "", nil, false
	}
	name, ok = Canonical(fields[0])
	return name, fields[1:], ok
}

// ToggleArg interprets one on/off argument of a switch command (/result,
// /markdown): "on"/"true"/"1"/"yes" turn the switch on, "off"/"false"/"0"/"no"
// turn it off and an empty argument flips current. ok is false when the argument
// is not a recognized spelling.
func ToggleArg(arg string, current bool) (state, ok bool) {
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "":
		return !current, true
	case "on", "true", "1", "yes":
		return true, true
	case "off", "false", "0", "no":
		return false, true
	}
	return current, false
}

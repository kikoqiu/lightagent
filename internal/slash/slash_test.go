package slash

import (
	"strings"
	"testing"

	"lightagent/internal/agent"
)

// TestCatalogueIntegrity pins that the catalogue is usable as a lookup table:
// every name and alias is unique, carries a leading slash and is described.
func TestCatalogueIntegrity(t *testing.T) {
	seen := make(map[string]string)
	for _, c := range Commands {
		if !strings.HasPrefix(c.Name, "/") {
			t.Errorf("command %q does not start with a slash", c.Name)
		}
		if c.Summary == "" {
			t.Errorf("command %q has no summary", c.Name)
		}
		if c.Primary && !c.Web {
			t.Errorf("command %q is primary but not runnable in the page", c.Name)
		}
		for _, name := range append([]string{c.Name}, c.Aliases...) {
			if prev, dup := seen[name]; dup {
				t.Errorf("%s is used by both %s and %s", name, prev, c.Name)
			}
			seen[name] = c.Name
		}
	}
}

// TestCanonicalResolvesAliases maps every documented alias onto its primary
// name and leaves an unknown spelling untouched.
func TestCanonicalResolvesAliases(t *testing.T) {
	for _, c := range Commands {
		for _, name := range append([]string{c.Name}, c.Aliases...) {
			got, ok := Canonical(name)
			if !ok || got != c.Name {
				t.Errorf("Canonical(%q) = (%q, %v), want (%q, true)", name, got, ok, c.Name)
			}
		}
	}
	if got, ok := Canonical("/nope"); ok || got != "/nope" {
		t.Errorf("Canonical(/nope) = (%q, %v), want the spelling back with false", got, ok)
	}
}

// TestNormalize covers full-width IME input for slash commands.
func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"/help":       "/help",
		"/？":          "/?",
		"／help":       "/help",
		"／？":          "/?",
		"/result off": "/result off",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Fatalf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestIsCommandLine checks both ASCII and full-width slash prefixes.
func TestIsCommandLine(t *testing.T) {
	for _, line := range []string{"/help", "/?", "／？", "／exit"} {
		if !IsCommandLine(line) {
			t.Fatalf("IsCommandLine(%q) = false, want true", line)
		}
	}
	for _, line := range []string{"hello", " /help", "?/"} {
		if IsCommandLine(line) {
			t.Fatalf("IsCommandLine(%q) = true, want false", line)
		}
	}
}

// TestSplit routes aliases and full-width input onto the canonical name and
// keeps the arguments.
func TestSplit(t *testing.T) {
	cases := []struct {
		line string
		name string
		args []string
		ok   bool
	}{
		{"/help", "/help", nil, true},
		{"／？", "/help", nil, true},
		{"/results off", "/result", []string{"off"}, true},
		{"/markdown on", "/markdown", []string{"on"}, true},
		{"/bogus x", "/bogus", []string{"x"}, false},
	}
	for _, tc := range cases {
		name, args, ok := Split(tc.line)
		if name != tc.name || ok != tc.ok || strings.Join(args, " ") != strings.Join(tc.args, " ") {
			t.Errorf("Split(%q) = (%q, %v, %v), want (%q, %v, %v)", tc.line, name, args, ok, tc.name, tc.args, tc.ok)
		}
	}
}

// TestToggleArg covers the on/off argument shared by /result and /markdown.
func TestToggleArg(t *testing.T) {
	cases := []struct {
		arg     string
		current bool
		state   bool
		ok      bool
	}{
		{"", true, false, true},
		{"", false, true, true},
		{"off", true, false, true},
		{"ON", false, true, true},
		{"yes", false, true, true},
		{"0", true, false, true},
		{"bogus", true, true, false},
	}
	for _, tc := range cases {
		state, ok := ToggleArg(tc.arg, tc.current)
		if state != tc.state || ok != tc.ok {
			t.Errorf("ToggleArg(%q, %v) = (%v, %v), want (%v, %v)",
				tc.arg, tc.current, state, ok, tc.state, tc.ok)
		}
	}
}

// TestTableListsEveryCommand pins that the help listing carries every command
// (with its usage) and each summary, so a new command cannot be added without
// showing up in both front-ends.
func TestTableListsEveryCommand(t *testing.T) {
	text := Table(nil)
	for _, c := range Commands {
		if !strings.Contains(text, c.Usage()) {
			t.Errorf("the table is missing %q:\n%s", c.Usage(), text)
		}
		if !strings.Contains(text, c.Summary) {
			t.Errorf("the table is missing the summary of %s:\n%s", c.Name, text)
		}
	}
	// The styled variant replaces the names in place; the plain one carries no
	// markup at all.
	styled := Table(func(name, rest string) string { return "<" + name + ">" + rest })
	if strings.Contains(text, "<") || !strings.Contains(styled, "</help, /?>") {
		t.Errorf("styled table = %q", styled)
	}
}

// TestWebCommandsOffersEveryRunnableCommand checks the rail covers exactly the
// commands the mirror can run (in catalogue order), drops the terminal-only ones
// and leaves something folded, so the rail stays short by default.
func TestWebCommandsOffersEveryRunnableCommand(t *testing.T) {
	web := WebCommands()
	if len(web) == 0 {
		t.Fatal("no browser commands")
	}
	seen := make(map[string]bool)
	primary := 0
	var order []string
	for _, c := range web {
		if !c.Web {
			t.Errorf("the rail offers the terminal-only %s", c.Name)
		}
		if seen[c.Name] {
			t.Errorf("%s is listed twice", c.Name)
		}
		seen[c.Name] = true
		order = append(order, c.Name)
		if c.Primary {
			primary++
		}
	}
	for _, c := range Commands {
		if c.Web != seen[c.Name] {
			t.Errorf("command %s: Web = %v but listed = %v", c.Name, c.Web, seen[c.Name])
		}
	}
	if primary == 0 || primary == len(web) {
		t.Errorf("%d of %d commands are primary, want some (but not all) folded", primary, len(web))
	}
	// The rail keeps the catalogue order, so the folded entries simply continue
	// the list the page already shows.
	var want []string
	for _, c := range Commands {
		if c.Web {
			want = append(want, c.Name)
		}
	}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("rail order = %v, want %v", order, want)
	}
	// /exit stays out of the rail but is reported as terminal-only.
	var terminal []string
	for _, c := range TerminalOnly() {
		terminal = append(terminal, c.Name)
	}
	if strings.Join(terminal, ",") != "/exit" {
		t.Fatalf("terminal-only commands = %v, want /exit", terminal)
	}
}

// TestUsageText renders messages, tokens and the context-window percentage.
func TestUsageText(t *testing.T) {
	got := UsageText(agent.Stats{
		Messages:      3,
		EstimatedTok:  1000,
		ContextWindow: 10000,
	})
	if !strings.Contains(got, "3 messages") || !strings.Contains(got, "10.0%") || !strings.Contains(got, "1000/10000") {
		t.Fatalf("UsageText = %q", got)
	}
	if !strings.Contains(UsageText(agent.Stats{Summary: "sum"}), "compressed summary present") {
		t.Error("a present summary should be reported")
	}
}

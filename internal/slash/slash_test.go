package slash

import (
	"strings"
	"testing"

	"lightagent/internal/agent"
	"lightagent/internal/config"
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

// TestSplitHonoursQuotes checks that a quoted file name stays one argument, so
// /saveas "my notes.json" names a file with a space rather than two arguments.
func TestSplitHonoursQuotes(t *testing.T) {
	cases := []struct {
		line string
		args []string
	}{
		{`/saveas "my notes.json"`, []string{"my notes.json"}},
		{`/saveas 'my notes'`, []string{"my notes"}},
		{`/load -f "a b"`, []string{"-f", "a b"}},
		{`/rm plain.json`, []string{"plain.json"}},
		{`/load ""`, []string{""}},
	}
	for _, tc := range cases {
		_, args, ok := Split(tc.line)
		if !ok || strings.Join(args, "\x00") != strings.Join(tc.args, "\x00") {
			t.Errorf("Split(%q) args = %q, want %q", tc.line, args, tc.args)
		}
	}
}

// TestForceFlagPullsTheSwitchOut checks the shared -f/--force parsing used by
// /saveas and /load.
func TestForceFlagPullsTheSwitchOut(t *testing.T) {
	cases := []struct {
		args  []string
		force bool
		rest  string
	}{
		{nil, false, ""},
		{[]string{"notes"}, false, "notes"},
		{[]string{"-f", "notes"}, true, "notes"},
		{[]string{"notes", "--force"}, true, "notes"},
		{[]string{"-f", "my", "notes"}, true, "my notes"},
	}
	for _, tc := range cases {
		force, rest := ForceFlag(tc.args)
		if force != tc.force || rest != tc.rest {
			t.Errorf("ForceFlag(%q) = (%v, %q), want (%v, %q)", tc.args, force, rest, tc.force, tc.rest)
		}
	}
}

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
	// markup at all. («…» keeps the check independent of the argument hints,
	// which legitimately contain angle brackets.)
	styled := Table(func(name, rest string) string { return "«" + name + "»" + rest })
	if strings.Contains(text, "«") || !strings.Contains(styled, "«/help, /?»") {
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
		// A Hidden command is runnable from the composer but stays out of the
		// rail, so the two do not have to agree for it.
		if want := c.Web && !c.Hidden; want != seen[c.Name] {
			t.Errorf("command %s: offered in the rail = %v, want %v", c.Name, seen[c.Name], want)
		}
	}
	if primary == 0 || primary == len(web) {
		t.Errorf("%d of %d commands are primary, want some (but not all) folded", primary, len(web))
	}
	// The rail keeps the catalogue order, so the folded entries simply continue
	// the list the page already shows.
	var want []string
	for _, c := range Commands {
		if c.Web && !c.Hidden {
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

// TestWorkingDirectoryCommandsAreWebCommands pins that both front-ends offer
// /pwd, /cd and /ls: they read and move the process working directory the two
// share, so the page must be able to run them and the rail must describe them.
func TestWorkingDirectoryCommandsAreWebCommands(t *testing.T) {
	offered := make(map[string]bool)
	for _, c := range WebCommands() {
		offered[c.Name] = c.Web
	}
	for _, name := range []string{"/pwd", "/cd", "/ls"} {
		if canonical, ok := Canonical(name); !ok || canonical != name {
			t.Fatalf("%s is not in the catalogue", name)
		}
		if !offered[name] {
			t.Fatalf("%s is not offered as a web command", name)
		}
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

// TestRailOrderAndSwitchAPICatalogue pins the command order the mirror's rail
// draws from: the primary commands first, then /compact opening the folded group
// and /help and /stop at its very bottom. /switchapi stays out of the rail (the
// model picker covers it) though it is still a web command.
func TestRailOrderAndSwitchAPICatalogue(t *testing.T) {
	web := WebCommands()
	var names, primary []string
	for _, c := range web {
		names = append(names, c.Name)
		if c.Primary {
			primary = append(primary, c.Name)
		}
	}
	if len(primary) == 0 || len(primary) == len(names) {
		t.Fatalf("primary = %v of %v, want a split list", primary, names)
	}
	// The rail shows six commands before folding the rest.
	if len(primary) != 6 {
		t.Fatalf("primary = %v, want 6 commands shown before the fold", primary)
	}
	folded := names[len(primary):]
	if folded[0] != "/compact" {
		t.Fatalf("the folded group starts with %q, want /compact", folded[0])
	}
	if last := names[len(names)-1]; last != "/stop" {
		t.Fatalf("the last command is %q, want /stop", last)
	}
	if prev := names[len(names)-2]; prev != "/help" {
		t.Fatalf("the second to last command is %q, want /help", prev)
	}
	// /saveas is one of the primary entries.
	hasSaveAs := false
	for _, name := range primary {
		if name == "/saveas" {
			hasSaveAs = true
		}
	}
	if !hasSaveAs {
		t.Fatalf("primary = %v, want /saveas among them", primary)
	}
	// /switchapi is not offered in the rail.
	for _, name := range names {
		if name == "/switchapi" {
			t.Fatal("/switchapi must stay out of the rail (it is Hidden)")
		}
	}
	var sw *Command
	for i := range Commands {
		if Commands[i].Name == "/switchapi" {
			sw = &Commands[i]
		}
	}
	if sw == nil || !sw.Web || !sw.Hidden || sw.Args == "" {
		t.Fatalf("/switchapi = %+v, want a web command hidden from the rail with an argument hint", sw)
	}
}

// TestAPIListText renders the interface listing behind /switchapi.
func TestAPIListText(t *testing.T) {
	got := APIListText([]config.APIInfo{
		{Index: 1, Name: "default", Model: "gpt-4o-mini", Enabled: true, Active: true},
		{Index: 2, Name: "backup", Model: "m2", Enabled: false},
	})
	if !strings.Contains(got, "1. default — gpt-4o-mini (active)") {
		t.Fatalf("listing = %q", got)
	}
	if !strings.Contains(got, "2. backup — m2 (disabled)") {
		t.Fatalf("listing = %q", got)
	}
	if got := APIListText(nil); got != "no llm interfaces configured" {
		t.Fatalf("empty listing = %q", got)
	}
}

// Command lightagent is a tiny OpenAI-compatible command-line agent.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"

	"lightagent/internal/agent"
	"lightagent/internal/config"
	"lightagent/internal/proc"
)

func main() {
	watchShutdown()
	code := run(os.Args[1:], os.Stdout, os.Stderr)
	// Safety net for the normal return paths: nothing the agent started may
	// outlive it, whichever way the session ended.
	proc.Shutdown()
	os.Exit(code)
}

// run parses args, dispatches subcommands and returns the process exit code.
// Exit codes: 0 ok, 1 runtime failure, 2 usage error.
func run(args []string, stdout, stderr io.Writer) int {
	o, rest, err := parseOptions(args)
	if err != nil {
		fmt.Fprintln(stderr, "lightagent: "+err.Error())
		fmt.Fprintln(stderr, "try 'lightagent --help'")
		return 2
	}
	if o.help {
		fmt.Fprint(stdout, helpText(rest))
		return 0
	}
	if o.version {
		fmt.Fprintln(stdout, versionString())
		return 0
	}

	if len(rest) > 0 {
		switch rest[0] {
		case "help":
			fmt.Fprint(stdout, helpText(rest[1:]))
			return 0
		case "version":
			fmt.Fprintln(stdout, versionString())
			return 0
		case "gen-agent-prompt":
			if dirErr := applyDir(o); dirErr != nil {
				fmt.Fprintln(stderr, "lightagent: "+dirErr.Error())
				return 1
			}
			if err := genAgentPrompt(o.configPath, rest[1:], stdout); err != nil {
				fmt.Fprintln(stderr, "lightagent: "+err.Error())
				return 1
			}
			return 0
		case "sessions":
			if err := runSessions(o, rest[1:], stdout); err != nil {
				fmt.Fprintln(stderr, "lightagent: "+err.Error())
				return 1
			}
			return 0
		case "completion":
			if err := runCompletion(rest[1:], stdout); err != nil {
				fmt.Fprintln(stderr, "lightagent: "+err.Error())
				return 1
			}
			return 0
		default:
			fmt.Fprintf(stderr, "lightagent: unknown command %q\ntry 'lightagent --help'\n", rest[0])
			return 2
		}
	}

	if err := runSession(o, stdout); err != nil {
		fmt.Fprintln(stderr, "lightagent: "+err.Error())
		return 1
	}
	return 0
}

// options holds the parsed command-line flags.
type options struct {
	// session selection
	configPath string
	dir        string
	resume     bool
	session    string

	// one-shot mode
	prompt string
	json   bool
	quiet  bool

	// output and behaviour
	logPath  string
	printCfg bool
	help     bool
	version  bool
	save     bool
	noSave   bool

	// config overrides
	model    string
	apiBase  string
	stream   *bool
	markdown *bool
	result   *bool
	timeout  int // -1 keep, otherwise openai.timeout_seconds
	webHost  string
	webPort  int // -1 keep, 0 disable
	noWeb    bool
}

// parseOptions parses args into options plus the positional arguments (a
// command and its own arguments). Unknown flags are an error.
func parseOptions(args []string) (*options, []string, error) {
	o := &options{webPort: -1, timeout: -1}
	fs := flag.NewFlagSet("lightagent", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}

	// Short and long forms bind the same fields.
	fs.StringVar(&o.configPath, "c", "", "")
	fs.StringVar(&o.configPath, "config", "", "")
	fs.StringVar(&o.dir, "C", "", "")
	fs.StringVar(&o.dir, "dir", "", "")
	fs.BoolVar(&o.resume, "r", false, "")
	fs.BoolVar(&o.resume, "resume", false, "")
	fs.StringVar(&o.session, "session", "", "")
	fs.StringVar(&o.prompt, "p", "", "")
	fs.StringVar(&o.prompt, "prompt", "", "")
	fs.BoolVar(&o.json, "json", false, "")
	fs.BoolVar(&o.quiet, "q", false, "")
	fs.BoolVar(&o.quiet, "quiet", false, "")
	fs.StringVar(&o.logPath, "log", "", "")
	fs.BoolVar(&o.printCfg, "print-config", false, "")
	fs.BoolVar(&o.help, "h", false, "")
	fs.BoolVar(&o.help, "help", false, "")
	fs.BoolVar(&o.version, "V", false, "")
	fs.BoolVar(&o.version, "version", false, "")
	fs.BoolVar(&o.version, "v", false, "") // kept for backwards compatibility
	fs.StringVar(&o.model, "model", "", "")
	fs.StringVar(&o.apiBase, "api-base", "", "")
	fs.IntVar(&o.timeout, "timeout", -1, "")
	fs.StringVar(&o.webHost, "web-host", "", "")
	fs.IntVar(&o.webPort, "web-port", -1, "")
	fs.BoolVar(&o.noWeb, "no-web", false, "")
	fs.BoolVar(&o.save, "save", false, "")
	fs.BoolVar(&o.noSave, "no-save", false, "")
	stream := fs.String("stream", "auto", "")
	markdown := fs.String("markdown", "auto", "")
	result := fs.String("result", "auto", "")

	if err := fs.Parse(args); err != nil {
		return nil, nil, err
	}

	var err error
	if o.stream, err = parseOnOff(*stream); err != nil {
		return nil, nil, fmt.Errorf("--stream: %w", err)
	}
	if o.markdown, err = parseOnOff(*markdown); err != nil {
		return nil, nil, fmt.Errorf("--markdown: %w", err)
	}
	if o.result, err = parseOnOff(*result); err != nil {
		return nil, nil, fmt.Errorf("--result: %w", err)
	}
	if o.json && o.prompt == "" {
		return nil, nil, errors.New("--json requires -p/--prompt")
	}
	if o.save && o.noSave {
		return nil, nil, errors.New("--save and --no-save are mutually exclusive")
	}
	return o, fs.Args(), nil
}

// parseOnOff interprets an on|off|auto flag value. "auto" (the default) leaves
// the configured value untouched and returns nil.
func parseOnOff(v string) (*bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "auto":
		return nil, nil
	case "on", "true", "1", "yes":
		on := true
		return &on, nil
	case "off", "false", "0", "no":
		off := false
		return &off, nil
	}
	return nil, fmt.Errorf("expected on|off|auto, got %q", v)
}

// applyDir performs -C/--dir by changing the working directory.
func applyDir(o *options) error {
	if o.dir == "" {
		return nil
	}
	if err := os.Chdir(o.dir); err != nil {
		return fmt.Errorf("change dir to %q: %w", o.dir, err)
	}
	return nil
}

// versionString reports the module version plus VCS information when the binary
// was built from a repository.
func versionString() string {
	version := "devel"
	revision := ""
	commitTime := ""
	if info, ok := debug.ReadBuildInfo(); ok {
		if info.Main.Version != "" && info.Main.Version != "(devel)" {
			version = info.Main.Version
		}
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				revision = s.Value
			case "vcs.time":
				commitTime = s.Value
			case "vcs.modified":
				if s.Value == "true" && revision != "" {
					revision += "+dirty"
				}
			}
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "lightagent %s", version)
	if revision != "" {
		if len(revision) > 12 {
			revision = revision[:12]
		}
		fmt.Fprintf(&b, " (commit %s", revision)
		if commitTime != "" {
			fmt.Fprintf(&b, ", %s", commitTime)
		}
		b.WriteString(")")
	}
	fmt.Fprintf(&b, " %s %s/%s", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	return b.String()
}

// genAgentPrompt writes the built-in system prompt to agent.md next to the
// config file. It refuses to overwrite an existing file unless -f is given.
func genAgentPrompt(configPath string, args []string, out io.Writer) error {
	force := false
	for _, a := range args {
		switch a {
		case "-f", "--force":
			force = true
		default:
			return fmt.Errorf("gen-agent-prompt: unknown argument %q", a)
		}
	}
	path := configPath
	if strings.TrimSpace(path) == "" {
		p, err := config.Path()
		if err != nil {
			return err
		}
		path = p
	}
	target := config.AgentPromptPath(path)
	if _, statErr := os.Stat(target); statErr == nil && !force {
		return fmt.Errorf("%s already exists; use -f to overwrite", target)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(target, []byte(agent.DefaultSystemPrompt()+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "wrote built-in agent prompt template to %s\n", target)
	fmt.Fprintln(out, "edit it to customize the system prompt; it overrides the built-in one automatically.")
	return nil
}

// usage is the top-level help text.
const usage = `lightagent - a tiny OpenAI-compatible command-line agent

usage:
  lightagent [options]              start the interactive session
  lightagent [options] <command>    run a command (see below)

options:
  -h, --help              show this help (see also "lightagent help <command>")
  -v, -V, --version       show the version
  -c, --config PATH       config file (default: next to the executable)
  -C, --dir PATH          change to PATH before starting
  -r, --resume            resume the saved session without asking
      --session PATH      use an explicit session file
  -p, --prompt TEXT       run one prompt and exit ("-" reads stdin)
      --json              with -p: print a JSON result instead of text
  -q, --quiet             with -p: hide tool/info output
      --log FILE          also append rendered output to FILE
      --save              always save the session on exit
      --no-save           never save the session on exit
      --print-config      print the effective configuration and exit

config overrides (flag > LIGHTAGENT_CONFIG > config.json > built-in default):
      --model NAME        openai.model
      --api-base URL      openai.api_base
      --stream on|off     openai.stream
      --markdown on|off   ui.markdown
      --result on|off     show tool/exec results
      --timeout SECONDS   openai.timeout_seconds (idle/inactivity timeout)
      --web-host IP       web.host
      --web-port N        web.port (0 disables the mirror)
      --no-web            disable the web mirror

commands:
  gen-agent-prompt [-f]            export the built-in system prompt to agent.md
  sessions list|show|prune         manage session files under .lightagent/
  completion bash|zsh|powershell   print a shell completion script
  help [command]                   show help for a command

state:
  the conversation lives in memory while running (nothing is written per turn).
  /save writes it on demand and the exit prompt asks whether to persist
  (default yes). When a saved session exists, startup asks whether to resume it
  (default yes); declining archives it with a timestamp and starts fresh.
`

// commandHelp holds per-command help text.
var commandHelp = map[string]string{
	"gen-agent-prompt": `lightagent gen-agent-prompt [-f]

Export the built-in system prompt to agent.md next to config.json. An existing
file is kept unless -f/--force is given.

  -f, --force    overwrite an existing agent.md
`,
	"sessions": `lightagent sessions <list|show|prune> [options]

Manage the session files in the state directory (.lightagent/, or the directory
of --session).

  list                     list sessions (current first, then archives)
  show [--file NAME]       print a saved conversation
  prune [--keep N|--all]   delete old archives (default: keep the newest 5)
  prune --file NAME        delete one specific session file

Options:
      --keep N     how many archives to keep (default 5)
      --all        delete every archive (not the current session)
      --file NAME  session file name or path
`,
	"completion": `lightagent completion <bash|zsh|powershell>

Print a shell completion script. Example:

  lightagent completion bash > /etc/bash_completion.d/lightagent
  lightagent completion powershell | Out-String | Invoke-Expression
`,
	"help": `lightagent help [command]

Show the top-level help, or the help for a command
(gen-agent-prompt, sessions, completion).
`,
}

// helpText returns the help for the given topic, or the top-level usage.
func helpText(topics []string) string {
	if len(topics) == 0 {
		return usage
	}
	if text, ok := commandHelp[topics[0]]; ok {
		return text
	}
	return fmt.Sprintf("no help for %q\n\n%s", topics[0], usage)
}

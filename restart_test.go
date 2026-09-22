package main

import (
	"strings"
	"testing"
)

// TestRestartArgsRebuildsTheCommandLine pins what the replacement process is
// started with: the switches this run was given travel as they are (the config
// file, the model overrides, an explicit session), while the ones that describe
// state the replacement already inherits or that must be set exactly once are
// replaced by a single trailing --resume.
func TestRestartArgsRebuildsTheCommandLine(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{"no arguments", nil, []string{"--resume"}},
		{"unrelated flags travel", []string{"--model", "m1"}, []string{"--model", "m1", "--resume"}},
		{"the directory goes with its value", []string{"-C", "sub", "--save"}, []string{"--save", "--resume"}},
		{"the long directory form goes", []string{"--dir=sub"}, []string{"--resume"}},
		{"the short directory form goes", []string{"-dir", "sub"}, []string{"--resume"}},
		{"--no-save is dropped", []string{"--no-save"}, []string{"--resume"}},
		{"a resume switch travels once", []string{"-r"}, []string{"--resume"}},
		{"the long resume form travels once", []string{"--resume"}, []string{"--resume"}},
		{"a value that looks like a flag", []string{"--model", "dir"}, []string{"--model", "dir", "--resume"}},
		{
			"a whole command line",
			[]string{"-c", "cfg.json", "--web-port", "8081", "-C", "sub", "-r", "--session", "s.json", "--no-save"},
			[]string{"-c", "cfg.json", "--web-port", "8081", "--session", "s.json", "--resume"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := restartArgs(tc.args)
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Fatalf("restartArgs(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

// TestRestartArgsStayTheSameRun feeds the rebuilt command line back into the
// parser: the replacement has to come up as the same run — same config file, same
// overrides, same session — with the resume switch set and the dropped ones gone.
func TestRestartArgsStayTheSameRun(t *testing.T) {
	args := []string{"-c", "cfg.json", "--model", "m1", "-C", ".", "--session", "s.json", "--no-save", "-r", "--web-port", "8081"}
	o, rest, err := parseOptions(restartArgs(args))
	if err != nil {
		t.Fatalf("the rebuilt command line must parse: %v", err)
	}
	if len(rest) != 0 {
		t.Fatalf("positional arguments = %v, want none", rest)
	}
	if !o.resume {
		t.Error("the replacement must resume the saved session")
	}
	if o.noSave {
		t.Error("--no-save must not travel: the replacement still saves on exit")
	}
	if o.dir != "" {
		t.Error("-C must not travel: the replacement starts in the resolved directory")
	}
	if o.configPath != "cfg.json" || o.model != "m1" || o.session != "s.json" || o.webPort != 8081 {
		t.Fatalf("the rebuilt run = %+v, want the same settings", o)
	}
}

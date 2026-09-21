// Command html2md converts HTML files to Markdown using the project's
// internal html-to-markdown converter.
//
// Usage:
//
//	html2md [options] <file.html> [<file.html> ...]
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"lightagent/internal/utils"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run parses arguments, converts each HTML file and returns the exit code.
// Exit codes: 0 ok, 1 runtime failure, 2 usage error.
func run(args []string, stdout, stderr io.Writer) int {
	var (
		baseURL   = flag.String("base-url", "", "base URL used to resolve relative links")
		bullet    = flag.String("bullet", "", "bullet marker for unordered lists")
		codeFence = flag.String("code-fence", "", "code fence characters")
		outFile   = flag.String("o", "", "write output to this file (single input only)")
		showWarn  = flag.Bool("warnings", false, "print converter warnings to stderr")
	)
	flag.Usage = func() { printUsage(stderr) }
	flag.Parse()

	files := flag.Args()
	if len(files) == 0 {
		printUsage(stderr)
		return 2
	}
	if *outFile != "" && len(files) > 1 {
		fmt.Fprintln(stderr, "html2md: -o can only be used with a single input file")
		return 2
	}

	out := stdout
	if *outFile != "" {
		f, err := os.Create(*outFile)
		if err != nil {
			fmt.Fprintf(stderr, "html2md: %v\n", err)
			return 1
		}
		defer f.Close()
		out = f
	}

	opts := buildOptions(*baseURL, *bullet, *codeFence)
	code := 0
	for _, file := range files {
		if len(files) > 1 {
			fmt.Fprintf(out, "<!-- source: %s -->\n", file)
		}
		data, err := os.ReadFile(file)
		if err != nil {
			fmt.Fprintf(stderr, "html2md: %s: %v\n", file, err)
			code = 1
			continue
		}
		res, err := utils.Html2MdConvert(string(data), opts...)
		if err != nil {
			fmt.Fprintf(stderr, "html2md: %s: %v\n", file, err)
			code = 1
			continue
		}
		if *showWarn {
			for _, w := range res.Warnings {
				fmt.Fprintf(stderr, "html2md: %s: %s\n", file, w)
			}
		}
		fmt.Fprintln(out, res.Markdown)
	}
	return code
}

// buildOptions maps the non-empty CLI flags onto converter option functions.
func buildOptions(baseURL, bullet, codeFence string) []utils.Html2MdOptionFunc {
	var opts []utils.Html2MdOptionFunc
	if baseURL != "" {
		opts = append(opts, utils.WithBaseURL(baseURL))
	}
	if bullet != "" {
		opts = append(opts, utils.WithBulletMarker(bullet))
	}
	if codeFence != "" {
		opts = append(opts, utils.WithCodeFence(codeFence))
	}
	return opts
}

func printUsage(w io.Writer) {
	fmt.Fprint(w,
		"Usage:\n"+
			"  html2md [options] <file.html> [<file.html> ...]\n"+
			"\n"+
			"Options:\n"+
			"  -base-url string   base URL used to resolve relative links\n"+
			"  -bullet string     bullet marker for unordered lists (default \"-\")\n"+
			"  -code-fence string code fence characters (default \"```\")\n"+
			"  -o string          write output to this file (single input only)\n"+
			"  -warnings          print converter warnings to stderr\n"+
			"\n"+
			"html2md converts one or more HTML files to Markdown and writes the\n"+
			"result to stdout, or to the file given by -o.\n")
}

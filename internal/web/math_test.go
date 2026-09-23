package web

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// mathCase is one formula for the harness below: Src is the message the page
// would render (markdown and all), Maths is how many formulas protect has to
// find in it, and Want/Reject are substrings of the HTML the whole pipeline
// (protect → marked → inject) produced.
type mathCase struct {
	Name   string   `json:"name"`
	Src    string   `json:"src"`
	Maths  int      `json:"maths"`
	Want   []string `json:"want"`
	Reject []string `json:"reject"`
}

// mathResult is what the harness reports back for one case.
type mathResult struct {
	Lifted string `json:"lifted"`
	Count  int    `json:"count"`
	HTML   string `json:"html"`
	Error  string `json:"error"`
}

// mathHarness loads math.js (and the vendored marked) into a bare JS context and
// reports the HTML the page's pipeline produces for every case: the formula
// renderer is browser JavaScript, so the only honest way to check it is to run
// it. It is a raw string, hence no backticks in it.
const mathHarness = `
const fs = require('fs');
const vm = require('vm');

const ctx = { window: {} };
vm.createContext(ctx);
vm.runInContext(fs.readFileSync(process.argv[2], 'utf8'), ctx);
const MathTex = ctx.window.MathTex;
if (!MathTex) { throw new Error('math.js did not define window.MathTex'); }
vm.runInContext(fs.readFileSync(process.argv[3], 'utf8'), ctx);
const marked = ctx.marked;
if (!marked) { throw new Error('marked.min.js did not define marked'); }

const cases = JSON.parse(fs.readFileSync(process.argv[4], 'utf8'));
const out = cases.map(function (c) {
  const res = {};
  try {
    const lifted = MathTex.protect(c.src);
    res.lifted = lifted.text;
    res.count = lifted.items.length;
    res.html = MathTex.inject(marked.parse(lifted.text, { gfm: true, breaks: true }), lifted.items);
  } catch (e) {
    res.error = String(e && e.message ? e.message : e);
  }
  return res;
});
process.stdout.write(JSON.stringify(out));
`

// TestMathRender drives the page's formula renderer in Node: what it produces —
// and what the markdown pipeline around it produces — can only be checked by
// running it with a real JS engine and the vendored marked. The test is skipped
// when node is not installed, so a machine without one still runs the rest of
// the suite; math.js and marked are read from the package directory.
func TestMathRender(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: skipping the math.js render checks")
	}
	script := "math.js"
	marked := filepath.Join("assets", "marked.min.js")
	for _, path := range []string{script, marked} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s is not next to the test: %v", path, err)
		}
	}

	dir := t.TempDir()
	harness := filepath.Join(dir, "harness.js")
	if err := os.WriteFile(harness, []byte(mathHarness), 0o600); err != nil {
		t.Fatalf("write the harness: %v", err)
	}
	cases, err := json.Marshal(mathCases)
	if err != nil {
		t.Fatalf("encode the cases: %v", err)
	}
	caseFile := filepath.Join(dir, "cases.json")
	if err := os.WriteFile(caseFile, cases, 0o600); err != nil {
		t.Fatalf("write the cases: %v", err)
	}

	cmd := exec.Command(node, harness, script, marked, caseFile)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node failed: %v\n%s", err, stderr.String())
	}
	var results []mathResult
	if err := json.Unmarshal(out, &results); err != nil {
		t.Fatalf("decode the harness output: %v\n%s", err, out)
	}
	if len(results) != len(mathCases) {
		t.Fatalf("the harness reported %d results for %d cases", len(results), len(mathCases))
	}

	for i, tc := range mathCases {
		got := results[i]
		if got.Error != "" {
			t.Errorf("%s: the harness failed: %s", tc.Name, got.Error)
			continue
		}
		if got.Count != tc.Maths {
			t.Errorf("%s: found %d formulas, want %d (HTML %q)", tc.Name, got.Count, tc.Maths, got.HTML)
		}
		for _, want := range tc.Want {
			if !strings.Contains(got.HTML, want) {
				t.Errorf("%s: %q is missing from %q", tc.Name, want, got.HTML)
			}
		}
		for _, reject := range tc.Reject {
			if strings.Contains(got.HTML, reject) {
				t.Errorf("%s: %q should not be in %q", tc.Name, reject, got.HTML)
			}
		}
		// A placeholder that reaches the browser would show up as a control
		// character in the row, so inject has to have replaced every one of them.
		if strings.Contains(got.HTML, "\x00") {
			t.Errorf("%s: a formula placeholder leaked into the HTML: %q", tc.Name, got.HTML)
		}
		if tc.Maths > 0 && !strings.Contains(got.Lifted, "\x00") {
			t.Errorf("%s: protect lifted nothing: %q", tc.Name, got.Lifted)
		}
	}
}

// mathCases pin what the page's LaTeX subset covers (relations, \text, \quad,
// scripts, roots, fractions, big operators) and, just as important, where the
// dollars are *not* a formula: code, prices, a lone $, and a formula whose
// closer has not arrived yet.
var mathCases = []mathCase{
	{
		Name: "display inequality", Src: `$$f \ge 5,\quad g \ge 4$$`, Maths: 1,
		Want: []string{
			`<span class="mtex mtex-block">`, `<mstyle displaystyle="true">`,
			`<mi>f</mi>`, `<mo>≥</mo>`, `<mn>5</mn>`,
			`<mspace width="1em"/>`, `<mi>g</mi>`, `<mn>4</mn>`,
		},
		Reject: []string{"$$", `\ge`},
	},
	{
		Name: "text and quad", Src: `$$f \ge 7 ;\text{或}; g \ge 15 \quad(\text{排除①})$$`, Maths: 1,
		Want: []string{
			`<mtext>或</mtext>`, `<mtext>排除①</mtext>`, `<mo>;</mo>`,
			`<mo stretchy="true">(</mo>`, `<mo stretchy="true">)</mo>`,
		},
		Reject: []string{`\text`, `\quad`},
	},
	{
		Name: "inline pair", Src: `条件 $f \ge 5$ 与 $g \ge 4$ 时。`, Maths: 2,
		Want:   []string{`<span class="mtex"><math>`, `<mi>f</mi>`, `<mi>g</mi>`},
		Reject: []string{"mtex-block", `\ge`},
	},
	{
		Name: "scripts on one base", Src: `$x_i^2$`, Maths: 1,
		Want: []string{`<msubsup><mi>x</mi><mi>i</mi><mn>2</mn></msubsup>`},
	},
	{
		Name: "markdown does not eat the math", Src: `乘积 $a * b$ 与 $c_d$`, Maths: 2,
		Want:   []string{`<mo>∗</mo>`, `<msub><mi>c</mi><mi>d</mi></msub>`},
		Reject: []string{"<em>"},
	},
	{
		Name: "prices stay text", Src: `it costs $5 and $6, or US$7 exactly`, Maths: 0,
		Want:   []string{"$5 and $6"},
		Reject: []string{"<math>"},
	},
	{
		Name: "single variable is math", Src: `设 $x$ 为整数`, Maths: 1,
		Want: []string{`<mi>x</mi>`},
	},
	{
		Name: "escaped dollar", Src: `price is \$5 only`, Maths: 0,
		Want:   []string{"$5 only"},
		Reject: []string{"<math>"},
	},
	{
		Name: "code fence stays code", Src: "code:\n\n```\n$$x^2$$\n```\n\ndone", Maths: 0,
		Want:   []string{"<pre><code>$$x^2$$"},
		Reject: []string{"<math>"},
	},
	{
		Name: "inline code stays code", Src: "a `$x$` b", Maths: 0,
		Want:   []string{"<code>$x$</code>"},
		Reject: []string{"<math>"},
	},
	{
		Name: "fraction and root", Src: `$\frac{a}{b} + \sqrt{x} + \sqrt[3]{y}$`, Maths: 1,
		Want: []string{
			`<mfrac><mi>a</mi><mi>b</mi></mfrac>`,
			`<msqrt><mi>x</mi></msqrt>`,
			`<mroot><mi>y</mi><mn>3</mn></mroot>`,
		},
	},
	{
		Name: "big operator in display style", Src: `$$\sum_{i=1}^{n} i$$`, Maths: 1,
		Want:   []string{`<munderover><mo>∑</mo>`, `<mrow><mi>i</mi><mo>=</mo><mn>1</mn></mrow>`},
		Reject: []string{"<msubsup>"},
	},
	{
		Name: "big operator inline", Src: `$\sum_{i=1}^{n} i$`, Maths: 1,
		Want:   []string{`<msubsup><mo>∑</mo>`},
		Reject: []string{"<munderover>"},
	},
	{
		Name: "integral keeps its limits beside", Src: `$$\int_0^1 x\,dx$$`, Maths: 1,
		Want:   []string{`<msubsup><mo>∫</mo>`, `<mspace width="0.167em"/>`},
		Reject: []string{"<munderover>"},
	},
	{
		Name: "unknown command is shown", Src: `$\foo(1)$`, Maths: 1,
		Want:   []string{`<mtext>foo</mtext>`},
		Reject: []string{`\foo`},
	},
	{
		Name: "half-typed formula stays text", Src: `$$f \ge 5`, Maths: 0,
		Reject: []string{"<math>"},
	},
	{
		Name: "sized fences", Src: `$\left( \frac{a}{b} \right]$`, Maths: 1,
		Want: []string{`<mo stretchy="true">(</mo>`, `<mo stretchy="true">]</mo>`},
	},
	{
		Name: "text mode unescapes", Src: `$\text{a\_b} \%$`, Maths: 1,
		Want: []string{`<mtext>a_b</mtext>`, `<mo>%</mo>`},
	},
	{
		Name: "nothing between the dollars", Src: `$$$$`, Maths: 0,
		Want:   []string{"$$$$"},
		Reject: []string{"<math>"},
	},
	{
		Name: "accents", Src: `$\overline{AB} = \vec{v}$`, Maths: 1,
		Want: []string{`<mover accent="true"><mi>AB</mi>`, `<mover accent="true"><mi>v</mi>`},
	},
	{
		Name: "double-struck is the real character", Src: `$\mathbb{R}^n$`, Maths: 1,
		Want: []string{`<mi mathvariant="normal" class="mv-normal">ℝ</mi>`, `<msup>`},
	},
	{
		Name: "bold rides on the class", Src: `$\mathbf{E} = \mathbf{B}$`, Maths: 1,
		Want: []string{`<mstyle mathvariant="bold" class="mv-bold">`, `<mi>E</mi>`},
	},
	{
		Name: "script on a bracketed group", Src: `$(x_i - \mu)^2$`, Maths: 1,
		Want: []string{
			`<msup><mrow><mo stretchy="true">(</mo>`,
			`<msub><mi>x</mi><mi>i</mi></msub><mo>−</mo><mi>μ</mi>`,
			`<mo stretchy="true">)</mo></mrow><mn>2</mn></msup>`,
		},
		Reject: []string{`<msup><mo>)</mo>`},
	},
	{
		Name: "sized group carries the exponent", Src: `$\left(\frac{n}{e}\right)^n$`, Maths: 1,
		Want: []string{
			`<msup><mrow><mo stretchy="true">(</mo><mfrac><mi>n</mi><mi>e</mi></mfrac>`,
			`<mo stretchy="true">)</mo></mrow><mi>n</mi></msup>`,
		},
	},
	{
		Name: "unmatched left keeps the formula", Src: `$\left. \frac{dy}{dx} \right|_{x=0}$`, Maths: 1,
		Want: []string{
			`<msub><mrow><mfrac>`,
			`<mo stretchy="true">|</mo></mrow><mrow><mi>x</mi><mo>=</mo><mn>0</mn></mrow></msub>`,
		},
	},
	{
		Name: "pipes that do not pair up stay plain", Src: `$P(A|B) = 1$`, Maths: 1,
		Want: []string{`<mo stretchy="true">(</mo><mi>A</mi><mo>|</mo><mi>B</mi>`},
	},
	{
		Name: "norms pair up", Src: `$\|u\| \leq \|v\|$`, Maths: 1,
		Want: []string{`<mrow><mo stretchy="true">‖</mo><mi>u</mi><mo stretchy="true">‖</mo></mrow>`},
	},
	{
		Name: "leg reads as less or equal", Src: `$f \leg 5$`, Maths: 1,
		Want:   []string{`<mo>≤</mo>`},
		Reject: []string{"<mtext>leg</mtext>"},
	},
	{
		Name: "relations a reply reaches for next", Src: `$a \nleq b \prec c \sqsubseteq d$`, Maths: 1,
		Want: []string{"<mo>\u2270</mo>", "<mo>\u227A</mo>", "<mo>\u2291</mo>"},
	},
	{
		Name: "arrows and identifiers", Src: `$A \hookrightarrow B \longmapsto C,\quad \wp \rhd \triangle$`, Maths: 1,
		Want: []string{
			"<mo>\u21AA</mo>", "<mo>\u27FC</mo>", "<mo>\u22B3</mo>", "<mo>\u25B3</mo>",
			"<mi mathvariant=\"normal\" class=\"mv-normal\">\u2118</mi>",
		},
	},
	{
		Name: "congruence modulo", Src: `$x^2 \equiv 1 \pmod{8}$`, Maths: 1,
		Want: []string{`<mtext>(mod</mtext>`, `<mn>8</mn><mo>)</mo>`},
	},
	{
		Name: "brace above and below", Src: `$\overbrace{a + b}^{\text{三項}}$`, Maths: 1,
		Want: []string{`<mover accent="true">`, `<mo stretchy="true">⏞</mo>`, `<mtext>三項</mtext>`},
	},
	{
		Name: "layout switches stay invisible", Src: `$$\displaystyle\sum\limits_{i=1}^{n} i = \frac{n(n+1)}{2}$$`, Maths: 1,
		Want:   []string{`<munderover><mo>∑</mo>`},
		Reject: []string{"<mtext>displaystyle</mtext>", "<mtext>limits</mtext>"},
	},
	{
		Name: "limits switch keeps the limit inline too", Src: `$\sum\limits_{i=1}^{n} i$`, Maths: 1,
		Want:   []string{`<msubsup><mo>∑</mo>`},
		Reject: []string{"<mtext>limits</mtext>"},
	},
	{
		Name: "price glued to a word stays text", Src: `US$7 cheap`, Maths: 0,
		Want:   []string{"US$7 cheap"},
		Reject: []string{"<math>"},
	},
	{
		Name: "bare number keeps its dollars", Src: `x$5$ y`, Maths: 0,
		Want:   []string{"x$5$ y"},
		Reject: []string{"<math>"},
	},
	{
		Name: "formula glued to a word", Src: `Cauchy$|x| \leq 1$ holds`, Maths: 1,
		Want:   []string{`<mo>≤</mo>`},
		Reject: []string{`$|x|`},
	},
	{
		Name: "sign belongs to the limit", Src: `$$\int_{-\infty}^{\infty} e^{-x^2}\,dx$$`, Maths: 1,
		Want: []string{
			`<msubsup><mo>∫</mo><mrow><mo>−</mo><mi mathvariant="normal" class="mv-normal">∞</mi></mrow>`,
			`<msup><mi>e</mi><mrow><mo>−</mo><msup><mi>x</mi><mn>2</mn></msup></mrow></msup>`,
		},
	},
	{
		Name: "binomial", Src: `$\binom{n}{k} = \frac{n!}{k!}$`, Maths: 1,
		Want: []string{
			`<mrow><mo stretchy="true">(</mo><mfrac linethickness="0">`,
			`<mo stretchy="true">)</mo></mrow>`,
		},
	},
	{
		Name: "box", Src: `$(\Box + m^2)\phi = 0$`, Maths: 1,
		Want: []string{`<mo>□</mo>`, `<msup><mi>m</mi><mn>2</mn></msup>`},
	},
	{
		Name: "aligned rows", Src: "$$\\begin{aligned} f &= a + b \\\\ g &= c - d \\end{aligned}$$", Maths: 1,
		Want: []string{
			`<mtable columnalign="right left">`,
			`<mtr><mtd><mi>f</mi></mtd><mtd><mo>=</mo><mi>a</mi><mo>+</mo><mi>b</mi></mtd></mtr>`,
			`<mtr><mtd><mi>g</mi></mtd>`,
		},
		Reject: []string{"aligned"},
	},
	{
		Name: "cases rows and columns", Src: "$$\\begin{cases} f \\ge 5, & x > 0 \\\\ g \\le 4, & x \\le 0 \\end{cases}$$", Maths: 1,
		Want: []string{
			`<mo stretchy="true">{</mo><mtable columnalign="left left">`,
			`<mtr><mtd><mi>f</mi><mo>≥</mo><mn>5</mn><mo>,</mo></mtd><mtd><mi>x</mi><mo>&gt;</mo><mn>0</mn></mtd></mtr>`,
		},
		Reject: []string{"cases"},
	},
	{
		Name: "matrix with a transpose", Src: "$$\\begin{pmatrix} a & b \\\\ c & d \\end{pmatrix}^T$$", Maths: 1,
		Want: []string{
			`<msup><mrow><mo stretchy="true">(</mo><mtable>`,
			`</mtable><mo stretchy="true">)</mo></mrow><mi>T</mi></msup>`,
		},
	},
	{
		Name: "array column spec", Src: "$$\\begin{array}{lcr} a & b & c \\end{array}$$", Maths: 1,
		Want: []string{`<mtable columnalign="left center right">`},
	},
	{
		Name: "bare row break", Src: `$$a = b \\ c = d$$`, Maths: 1,
		Want: []string{`<mtable><mtr><mtd><mi>a</mi>`, `<mtr><mtd><mi>c</mi>`},
	},
	{
		Name: "unknown environment keeps its content", Src: `$$\begin{foo} a + b \end{foo}$$`, Maths: 1,
		Want:   []string{`<mi>a</mi><mo>+</mo><mi>b</mi>`},
		Reject: []string{"foo"},
	},
	{
		Name: "extra script is not dropped", Src: `$x^a^b$`, Maths: 1,
		Want: []string{`<msup><mi>x</mi><mi>a</mi></msup><mi>b</mi>`},
	},
	{
		Name: "angle brackets are escaped", Src: `$a < b > c$`, Maths: 1,
		Want:   []string{`<mo>&lt;</mo>`, `<mo>&gt;</mo>`},
		Reject: []string{"<b>", "<c>"},
	},
	{
		Name: "display formula on its own lines", Src: "前文\n\n$$\nf \\ge 5\n$$\n\n后文", Maths: 1,
		Want: []string{"<p>前文</p>", `<p><span class="mtex mtex-block">`, "<p>后文</p>"},
	},
}

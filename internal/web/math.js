// math.js renders the formulas a reply contains as MathML, so the browser lays
// them out itself. It is a deliberately small LaTeX subset: KaTeX would drag in
// a webfont and about 1 MB of assets, MathJax is larger still, while MathML
// needs no download at all — roots, fractions and scripts are engine features
// there (docs/web.md).
//
// It hooks into the markdown pipeline through protect/inject, because markdown
// and TeX fight over the same characters: marked reads x_i_y as emphasis and eats
// the backslash of \%, \{ and \;. A formula is therefore lifted out of the text
// *before* the markdown pass and put back as MathML *after* it, just before the
// result is sanitized with DOMPurify (see markdownToHTML in app.js). Fenced and
// inline code are skipped on the way out: a formula shown as code stays code.
(function () {
  'use strict';

  // --------------------------------------------------------------- escaping --

  function esc(text) {
    return String(text).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
  }

  // ----------------------------------------------------------------- tables --
  // SYMBOLS maps a TeX command to [element, glyph, mathvariant?]: mi is an
  // identifier (a single letter comes out italic, the way TeX sets variables),
  // mo an operator or a relation (the engine spaces those itself) and mtext
  // upright words. The variant keeps constants and function names upright.
  var SYMBOLS = {
    // Greek letters
    alpha: ['mi', '\u03B1'], beta: ['mi', '\u03B2'], gamma: ['mi', '\u03B3'],
    delta: ['mi', '\u03B4'], epsilon: ['mi', '\u03F5'], varepsilon: ['mi', '\u03B5'],
    zeta: ['mi', '\u03B6'], eta: ['mi', '\u03B7'], theta: ['mi', '\u03B8'],
    vartheta: ['mi', '\u03D1'], iota: ['mi', '\u03B9'], kappa: ['mi', '\u03BA'],
    lambda: ['mi', '\u03BB'], mu: ['mi', '\u03BC'], nu: ['mi', '\u03BD'],
    xi: ['mi', '\u03BE'], pi: ['mi', '\u03C0'], rho: ['mi', '\u03C1'],
    sigma: ['mi', '\u03C3'], tau: ['mi', '\u03C4'], upsilon: ['mi', '\u03C5'],
    phi: ['mi', '\u03D5'], varphi: ['mi', '\u03C6'], chi: ['mi', '\u03C7'],
    psi: ['mi', '\u03C8'], omega: ['mi', '\u03C9'],
    Gamma: ['mi', '\u0393'], Delta: ['mi', '\u0394'], Theta: ['mi', '\u0398'],
    Lambda: ['mi', '\u039B'], Xi: ['mi', '\u039E'], Pi: ['mi', '\u03A0'],
    Sigma: ['mi', '\u03A3'], Upsilon: ['mi', '\u03A5'], Phi: ['mi', '\u03A6'],
    Psi: ['mi', '\u03A8'], Omega: ['mi', '\u03A9'],
    // Upright letters and constants
    infty: ['mi', '\u221E', 'normal'], partial: ['mi', '\u2202', 'normal'],
    nabla: ['mi', '\u2207', 'normal'], ell: ['mi', '\u2113', 'normal'],
    hbar: ['mi', '\u210F', 'normal'], emptyset: ['mi', '\u2205', 'normal'],
    varnothing: ['mi', '\u2205', 'normal'], aleph: ['mi', '\u2135', 'normal'],
    // Function names (upright, as TeX sets them)
    sin: ['mi', 'sin', 'normal'], cos: ['mi', 'cos', 'normal'], tan: ['mi', 'tan', 'normal'],
    cot: ['mi', 'cot', 'normal'], sec: ['mi', 'sec', 'normal'], csc: ['mi', 'csc', 'normal'],
    arcsin: ['mi', 'arcsin', 'normal'], arccos: ['mi', 'arccos', 'normal'],
    arctan: ['mi', 'arctan', 'normal'], sinh: ['mi', 'sinh', 'normal'],
    cosh: ['mi', 'cosh', 'normal'], tanh: ['mi', 'tanh', 'normal'],
    log: ['mi', 'log', 'normal'], ln: ['mi', 'ln', 'normal'], lg: ['mi', 'lg', 'normal'],
    exp: ['mi', 'exp', 'normal'], lim: ['mi', 'lim', 'normal'], max: ['mi', 'max', 'normal'],
    min: ['mi', 'min', 'normal'], sup: ['mi', 'sup', 'normal'], inf: ['mi', 'inf', 'normal'],
    det: ['mi', 'det', 'normal'], dim: ['mi', 'dim', 'normal'], ker: ['mi', 'ker', 'normal'],
    hom: ['mi', 'hom', 'normal'], gcd: ['mi', 'gcd', 'normal'], deg: ['mi', 'deg', 'normal'],
    arg: ['mi', 'arg', 'normal'], mod: ['mi', 'mod', 'normal'], bmod: ['mi', 'mod', 'normal'],
    // Binary operators
    pm: ['mo', '\u00B1'], mp: ['mo', '\u2213'], times: ['mo', '\u00D7'],
    div: ['mo', '\u00F7'], cdot: ['mo', '\u22C5'], ast: ['mo', '\u2217'],
    star: ['mo', '\u22C6'], circ: ['mo', '\u2218'], bullet: ['mo', '\u2219'],
    oplus: ['mo', '\u2295'], ominus: ['mo', '\u2296'], otimes: ['mo', '\u2297'],
    oslash: ['mo', '\u2298'], odot: ['mo', '\u2299'], wedge: ['mo', '\u2227'],
    vee: ['mo', '\u2228'], land: ['mo', '\u2227'], lor: ['mo', '\u2228'],
    neg: ['mo', '\u00AC'], lnot: ['mo', '\u00AC'], setminus: ['mo', '\u2216'],
    // Relations
    le: ['mo', '\u2264'], leq: ['mo', '\u2264'], ge: ['mo', '\u2265'], geq: ['mo', '\u2265'],
    ne: ['mo', '\u2260'], neq: ['mo', '\u2260'], equiv: ['mo', '\u2261'],
    approx: ['mo', '\u2248'], sim: ['mo', '\u223C'], simeq: ['mo', '\u2243'],
    cong: ['mo', '\u2245'], propto: ['mo', '\u221D'], ll: ['mo', '\u226A'],
    gg: ['mo', '\u226B'], subset: ['mo', '\u2282'], supset: ['mo', '\u2283'],
    subseteq: ['mo', '\u2286'], supseteq: ['mo', '\u2287'], in: ['mo', '\u2208'],
    notin: ['mo', '\u2209'], ni: ['mo', '\u220B'], parallel: ['mo', '\u2225'],
    perp: ['mo', '\u22A5'], angle: ['mo', '\u2220'], therefore: ['mo', '\u2234'],
    because: ['mo', '\u2235'], prime: ['mo', '\u2032'],
    // Aliases models keep writing for the relations above (leg is not TeX, but
    // it is read as "less or equal").
    leg: ['mo', '\u2264'], greq: ['mo', '\u2265'],
    leqslant: ['mo', '\u2A7D'], geqslant: ['mo', '\u2A7E'],
    // Arrows
    to: ['mo', '\u2192'], rightarrow: ['mo', '\u2192'], leftarrow: ['mo', '\u2190'],
    leftrightarrow: ['mo', '\u2194'], Rightarrow: ['mo', '\u21D2'],
    Leftarrow: ['mo', '\u21D0'], Leftrightarrow: ['mo', '\u21D4'],
    mapsto: ['mo', '\u21A6'], uparrow: ['mo', '\u2191'], downarrow: ['mo', '\u2193'],
    implies: ['mo', '\u27F9'], iff: ['mo', '\u27FA'],
    // Quantifiers, dots, plain fences
    forall: ['mo', '\u2200'], exists: ['mo', '\u2203'],
    top: ['mo', '\u22A4'], bot: ['mo', '\u22A5'],
    Box: ['mo', '\u25A1'], square: ['mo', '\u25A1'], Square: ['mo', '\u25A1'],
    dots: ['mo', '\u2026'], ldots: ['mo', '\u2026'], cdots: ['mo', '\u22EF'],
    vdots: ['mo', '\u22EE'], ddots: ['mo', '\u22F1'],
    langle: ['mo', '\u27E8'], rangle: ['mo', '\u27E9'], backslash: ['mo', '\\'],
    vert: ['mo', '|'], Vert: ['mo', '\u2016'], lvert: ['mo', '|'], rvert: ['mo', '|'],
    lVert: ['mo', '\u2016'], rVert: ['mo', '\u2016'],
    // Relations and negations a reply reaches for next
    prec: ['mo', '\u227A'], succ: ['mo', '\u227B'], nless: ['mo', '\u226E'],
    ngtr: ['mo', '\u226F'], nleq: ['mo', '\u2270'], ngeq: ['mo', '\u2271'],
    nsim: ['mo', '\u2241'], ncong: ['mo', '\u2247'], nmid: ['mo', '\u2224'],
    nparallel: ['mo', '\u2226'], asymp: ['mo', '\u224D'], doteq: ['mo', '\u2250'],
    fallingdotseq: ['mo', '\u2252'], risingdotseq: ['mo', '\u2253'],
    bowtie: ['mo', '\u22C8'], models: ['mo', '\u22A8'], vdash: ['mo', '\u22A2'],
    dashv: ['mo', '\u22A3'], smile: ['mo', '\u2323'], frown: ['mo', '\u2322'],
    sqsubset: ['mo', '\u228F'], sqsupset: ['mo', '\u2290'],
    sqsubseteq: ['mo', '\u2291'], sqsupseteq: ['mo', '\u2292'],
    // Set and binary operators
    cup: ['mo', '\u222A'], cap: ['mo', '\u2229'], sqcup: ['mo', '\u2294'],
    sqcap: ['mo', '\u2293'], uplus: ['mo', '\u228E'], smallsetminus: ['mo', '\u2216'],
    barwedge: ['mo', '\u22BC'], veebar: ['mo', '\u22BB'], diamond: ['mo', '\u22C4'],
    wr: ['mo', '\u2240'], amalg: ['mo', '\u2A3F'], lhd: ['mo', '\u22B2'],
    rhd: ['mo', '\u22B3'], unlhd: ['mo', '\u22B4'], unrhd: ['mo', '\u22B5'],
    centerdot: ['mo', '\u22C5'], triangleq: ['mo', '\u225C'],
    blacktriangle: ['mo', '\u25B2'], triangledown: ['mo', '\u25BD'],
    triangle: ['mo', '\u25B3'], vartriangle: ['mo', '\u25B3'],
    vartriangleleft: ['mo', '\u22B2'], vartriangleright: ['mo', '\u22B3'],
    dagger: ['mo', '\u2020'], ddagger: ['mo', '\u2021'], blacksquare: ['mo', '\u25A0'],
    gets: ['mo', '\u2190'],
    // Long and harpoon arrows
    longrightarrow: ['mo', '\u27F6'], longleftarrow: ['mo', '\u27F5'],
    longleftrightarrow: ['mo', '\u27F7'], Longrightarrow: ['mo', '\u27F9'],
    Longleftarrow: ['mo', '\u27F8'], Longleftrightarrow: ['mo', '\u27FA'],
    longmapsto: ['mo', '\u27FC'], hookrightarrow: ['mo', '\u21AA'],
    hookleftarrow: ['mo', '\u21A9'], leftharpoonup: ['mo', '\u21BC'],
    rightharpoonup: ['mo', '\u21C0'], updownarrow: ['mo', '\u2195'],
    Uparrow: ['mo', '\u21D1'], Downarrow: ['mo', '\u21D3'], Updownarrow: ['mo', '\u21D5'],
    // Letters, angles and measures that are drawn upright
    imath: ['mi', '\u0131', 'normal'], jmath: ['mi', '\u0237', 'normal'],
    wp: ['mi', '\u2118', 'normal'], Re: ['mi', '\u211C', 'normal'],
    Im: ['mi', '\u2111', 'normal'], beth: ['mi', '\u2136', 'normal'],
    gimel: ['mi', '\u2137', 'normal'], daleth: ['mi', '\u2138', 'normal'],
    nexists: ['mi', '\u2204', 'normal'], complement: ['mi', '\u2201', 'normal'],
    mho: ['mi', '\u2127', 'normal'], omicron: ['mi', '\u03BF'], varsigma: ['mi', '\u03C2'],
    measuredangle: ['mo', '\u2221'], sphericalangle: ['mo', '\u2222'],
    degree: ['mo', '\u00B0'], celsius: ['mo', '\u2103'], micro: ['mi', '\u00B5', 'normal'],
    permil: ['mo', '\u2030'], backprime: ['mo', '\u2035'], colon: ['mo', ':'],
    dotsb: ['mo', '\u2026'], dotsc: ['mo', '\u2026'], dotsm: ['mo', '\u22EF'],
    dotsi: ['mo', '\u22EF'], idotsint: ['mo', '\u222B\u22EF\u222B'],
    // Function names a reply reaches for next
    limsup: ['mi', 'lim sup', 'normal'], liminf: ['mi', 'lim inf', 'normal'],
    coth: ['mi', 'coth', 'normal'], sech: ['mi', 'sech', 'normal'],
    csch: ['mi', 'csch', 'normal'], arsinh: ['mi', 'arsinh', 'normal']
  };

  // BIG_OPS maps the large operators to their glyph and to whether their limits
  // sit under and over the glyph in display style — TeX's own rule: a sum does,
  // an integral keeps them beside.
  var BIG_OPS = {
    sum: ['\u2211', true], prod: ['\u220F', true], coprod: ['\u2210', true],
    bigcup: ['\u22C3', true], bigcap: ['\u22C2', true], bigwedge: ['\u22C0', true],
    bigvee: ['\u22C1', true], bigoplus: ['\u2A01', true], bigotimes: ['\u2A02', true],
    bigsqcup: ['\u2A06', true], bigodot: ['\u2A00', true], biguplus: ['\u2A04', true],
    int: ['\u222B', false], iint: ['\u222C', false], iiint: ['\u222D', false],
    iiiint: ['\u2A0C', false], oint: ['\u222E', false], oiint: ['\u222F', false]
  };
  // SPACES is explicit horizontal space in em. MathML spaces operators itself,
  // so a plain space in the source is ignored the way TeX ignores it; only the
  // spacing commands (and ~, a non-breaking space) survive. Negative widths are
  // TeX's own way of pulling things together (\negthinspace, \!).
  var SPACES = {
    quad: '1em', qquad: '2em', enspace: '0.5em', thinspace: '0.167em',
    medspace: '0.222em', thickspace: '0.278em', negthinspace: '-0.167em',
    ':': '0.222em', '>': '0.222em', ';': '0.278em', ',': '0.167em',
    '!': '-0.167em', ' ': '0.25em'
  };

  // ACCENTS maps an accent command to its glyph and to where it sits.
  var ACCENTS = {
    overline: ['\u203E', 'over'], bar: ['\u00AF', 'over'], hat: ['\u02C6', 'over'],
    widehat: ['\u02C6', 'over'], tilde: ['\u02DC', 'over'], widetilde: ['\u02DC', 'over'],
    vec: ['\u20D7', 'over'], dot: ['\u02D9', 'over'], ddot: ['\u00A8', 'over'],
    check: ['\u02C7', 'over'], widecheck: ['\u02C7', 'over'], breve: ['\u02D8', 'over'],
    underline: ['_', 'under'], mathring: ['\u02DA', 'over'], acute: ['\u00B4', 'over'],
    grave: ['\u0060', 'over'],
    // Braces and brackets above and below, and the arrow accents: the glyph is
    // marked stretchy by the renderer, so it grows with the content.
    overbrace: ['\u23DE', 'over'], underbrace: ['\u23DF', 'under'],
    overparen: ['\u23DC', 'over'], wideparen: ['\u23DC', 'over'],
    underparen: ['\u23DD', 'under'], overbracket: ['\u23B4', 'over'],
    underbracket: ['\u23B5', 'under'], overrightarrow: ['\u2192', 'over'],
    overleftarrow: ['\u2190', 'over'], overleftrightarrow: ['\u2194', 'over'],
    underrightarrow: ['\u2192', 'under'], dddot: ['\u20DB', 'over']
  };

  // VARIANTS maps a font command to the MathML mathvariant it selects and to the
  // CSS class the page stylesheet uses for it: MathML Core replaced mathvariant
  // with CSS, so Chromium reads the class while the engines that still implement
  // the attribute read that (\mathbb is handled separately — it becomes the real
  // double-struck characters). The \text* commands made it in here too: a reply
  // that writes \textbf inside math wants bold, not the word "textbf".
  var VARIANTS = {
    mathbb: ['double-struck', 'normal'], mathbf: ['bold', 'bold'],
    boldsymbol: ['bold-italic', 'bolditalic'], mathit: ['italic', 'italic'],
    mathrm: ['normal', 'normal'], mathcal: ['script', 'script'],
    mathfrak: ['fraktur', 'fraktur'], mathsf: ['sans-serif', 'sans'],
    mathtt: ['monospace', 'mono'], bm: ['bold-italic', 'bolditalic'],
    pmb: ['bold', 'bold'], textbf: ['bold', 'bold'], textit: ['italic', 'italic'],
    textsf: ['sans-serif', 'sans'], texttt: ['monospace', 'mono']
  };

  // IGNORED commands say how something is laid out rather than what to draw:
  // TeX's style, size, spacing and atom-class switches. They go away instead of
  // printing their own name into the formula (\limits and \nolimits do not even
  // need an entry — a big operator takes its scripts either way).
  var IGNORED = {
    displaystyle: 1, textstyle: 1, scriptstyle: 1, scriptscriptstyle: 1,
    limits: 1, nolimits: 1, mathstrut: 1, strut: 1, phantom: 1, vphantom: 1,
    hphantom: 1, smash: 1, hfil: 1, hfill: 1, notag: 1, nonumber: 1,
    allowbreak: 1, displaybreak: 1, mathbin: 1, mathrel: 1, mathord: 1,
    mathop: 1, mathopen: 1, mathclose: 1, mathpunct: 1, mathinner: 1,
    over: 1, choose: 1, atop: 1
  };

  // TEXT_COMMANDS show their argument as upright text, exactly as written.
  var TEXT_COMMANDS = {
    text: 1, mbox: 1, textrm: 1, textnormal: 1, textup: 1, operatorname: 1
  };

  // FENCES maps a delimiter to the glyph it is drawn with; an empty string means
  // "draw nothing" (\left. and \right.). The key is either the bare character or
  // the command that stands for it (\{ , \langle …).
  var FENCES = {
    '(': '(', ')': ')', '[': '[', ']': ']', '{': '{', '}': '}', '|': '|', '.': '',
    '<': '\u27E8', '>': '\u27E9', '/': '/',
    '\\{': '{', '\\}': '}', '\\|': '\u2016', '\\langle': '\u27E8', '\\rangle': '\u27E9',
    '\\lvert': '|', '\\rvert': '|', '\\lVert': '\u2016', '\\rVert': '\u2016',
    '\\vert': '|', '\\Vert': '\u2016', '\\lbrace': '{', '\\rbrace': '}',
    '\\lbrack': '[', '\\rbrack': ']', '\\uparrow': '\u2191', '\\downarrow': '\u2193',
    '\\backslash': '\\'
  };

  // SIZED_FENCES are the commands that take a delimiter without being one: they
  // only decide how tall it is drawn, which the engine does on its own.
  var SIZED_FENCES = {
    left: 1, right: 1, big: 1, Big: 1, bigg: 1, Bigg: 1, bigl: 1, bigr: 1,
    Bigl: 1, Bigr: 1, biggl: 1, biggr: 1, Biggl: 1, Biggr: 1, bigm: 1, Bigm: 1
  };

  // FENCE_ROLES tells the pairing pass which delimiters open a group and which
  // close it. A closing delimiter that carries a script has to end up on the
  // whole (…) group: then both delimiters stretch to the same height and the
  // exponent sits beside the group, instead of hanging off a closing bracket
  // whose opening partner stretched to a different height.
  var FENCE_ROLES = {
    '(': ['paren', true, false], ')': ['paren', false, true],
    '[': ['bracket', true, false], ']': ['bracket', false, true],
    '{': ['brace', true, false], '}': ['brace', false, true],
    '\u27E8': ['angle', true, false], '\u27E9': ['angle', false, true],
    '|': ['bar', true, true], '\u2016': ['norm', true, true]
  };

  // SIGN_TOKENS are the tokens that belong to the script they introduce:
  // \int_{-\infty} is the integral from minus infinity, not an integral whose
  // lower limit is a minus sign with a stray \infty next to it.
  var SIGN_TOKENS = { '\u2212': 1, '+': 1, '\u00B1': 1, '\u2213': 1 };

  // ENVIRONMENTS are the alignment environments that become a table, with the
  // fences their name implies. Anything else between \begin and \end is dropped
  // while its content is still shown.
  var ENVIRONMENTS = {
    aligned: 'aligned', align: 'aligned', 'align*': 'aligned', split: 'aligned',
    eqnarray: 'aligned', gather: 'center', 'gather*': 'center', cases: 'cases',
    matrix: 'center', 'matrix*': 'center', pmatrix: 'paren', bmatrix: 'bracket',
    Bmatrix: 'brace', vmatrix: 'bar', Vmatrix: 'norm', array: 'array'
  };

  // ARRAY_ALIGN maps an array's column spec to the MathML alignment (| and
  // anything else in the spec is merely a rule, which is not drawn).
  var ARRAY_ALIGN = { l: 'left', c: 'center', r: 'right' };

  // DOUBLE_STRUCK_HOLES are the double-struck capitals Unicode keeps in the
  // Letterlike Symbols block instead of the mathematical alphanumerics; the
  // characters are what \mathbb becomes, since Chromium ignores mathvariant.
  var DOUBLE_STRUCK_HOLES = {
    C: '\u2102', H: '\u210D', N: '\u2115', P: '\u2119', Q: '\u211A',
    R: '\u211D', Z: '\u2124'
  };

  // OPERATOR_CHARS are the characters TeX would set as an operator but a message
  // types as plain ASCII: a hyphen is a minus sign, a star a real operator.
  var OPERATOR_CHARS = { '-': '\u2212', '*': '\u2217', "'": '\u2032', '"': '\u2033' };

  // RIGHT_COMMAND matches the \right that closes a \left group without swallowing
  // \rightarrow and friends.
  var RIGHT_COMMAND = /^\\right(?![A-Za-z])/;

  // isWide reports whether a character is set as upright text rather than as a
  // math symbol: CJK, its punctuation, and the circled numerals a Chinese reply
  // likes to number cases with.
  function isWide(ch) {
    var c = ch.charCodeAt(0);
    return (c >= 0x2460 && c <= 0x24FF) || (c >= 0x2E80 && c <= 0x303F) ||
      (c >= 0x3040 && c <= 0x9FFF) || (c >= 0xF900 && c <= 0xFAFF) ||
      (c >= 0xFF00 && c <= 0xFFEF);
  }
  // ----------------------------------------------------------------- parsing --
  // The parser turns the source into the small tree the renderer walks. It never
  // throws: a half-typed formula (the reply is still streaming) simply stops
  // where the source does, and whatever came before it is still shown.

  function Parser(src) {
    this.src = String(src);
    this.pos = 0;
  }

  // readName reads a command name: the letters after the backslash, or the one
  // non-letter character that follows it (\%, \{, \; …), returned as itself.
  Parser.prototype.readName = function () {
    var start = this.pos;
    while (this.pos < this.src.length && /[A-Za-z]/.test(this.src.charAt(this.pos))) {
      this.pos++;
    }
    if (this.pos === start) {
      this.pos++;
      return this.src.charAt(start);
    }
    return this.src.slice(start, this.pos);
  };

  // list parses nodes until the closing brace of the group, or the end.
  Parser.prototype.list = function (stop) {
    var nodes = [], node;
    while (this.pos < this.src.length) {
      if (stop && this.src.charAt(this.pos) === stop) { break; }
      node = this.atom();
      if (!node) { break; }
      if (node.type !== 'ignore') { nodes.push(node); }
    }
    return nodes;
  };

  // atom parses one element plus the scripts attached to it.
  Parser.prototype.atom = function () {
    var start = this.pos;
    var node = this.element();
    if (!node) {
      // An unbalanced brace or a stray marker: show it as itself instead of
      // dropping the rest of the formula.
      this.pos = start + 1;
      return { type: 'mtext', text: this.src.charAt(start) };
    }
    if (this.pos === start) { this.pos = start + 1; }
    var sub = null, sup = null, extra = [], ch, arg, slot;
    for (;;) {
      // A layout switch sits happily between a base and its script
      // (\sum\limits_{i=1}^n): it is stepped over, not parsed as a script token.
      if (this.skipIgnored()) { continue; }
      if (this.pos >= this.src.length) { break; }
      ch = this.src.charAt(this.pos);
      if (ch !== '^' && ch !== '_') { break; }
      this.pos++;
      arg = this.argument();
      // An empty argument (x^ at the end of a half-typed formula) adds nothing.
      if (!arg || !arg.length) { continue; }
      slot = ch === '^' ? 'sup' : 'sub';
      if (slot === 'sup' && !sup) { sup = arg; } else if (slot === 'sub' && !sub) {
        sub = arg;
      } else {
        // x^a^b: TeX refuses the second script, so it is shown beside the
        // formula instead of being dropped.
        extra = extra.concat(arg);
      }
    }
    if (!sub && !sup) { return node; }
    var scripts = { type: 'scripts', base: node, sub: sub, sup: sup };
    if (!extra.length) { return scripts; }
    return { type: 'row', children: [scripts].concat(extra) };
  };

  // skipIgnored steps over a command the renderer ignores (the layout switches
  // in IGNORED) and reports whether it did. Anything else leaves the position
  // untouched, so it can never eat a token.
  Parser.prototype.skipIgnored = function () {
    var save, name;
    if (this.src.charAt(this.pos) !== '\\') { return false; }
    save = this.pos;
    this.pos++;
    name = this.readName();
    if (name.length > 1 && IGNORED[name]) { return true; }
    this.pos = save;
    return false;
  };

  // argument parses one TeX argument: a {...} group, or the single next token
  // when no brace follows (\frac12, x^\alpha …). A script argument is one token,
  // not a whole expression, so x_i^2 gives both scripts to x, the way TeX reads
  // it; a sign in front of it belongs to it (_{-\infty}, ^{-1}).
  Parser.prototype.argument = function () {
    if (this.src.charAt(this.pos) === '{') {
      this.pos++;
      var nodes = this.list('}');
      if (this.src.charAt(this.pos) === '}') { this.pos++; }
      return nodes;
    }
    var node = this.element();
    if (!node) { return []; }
    var nodes = [node];
    if (node.type === 'mo' && SIGN_TOKENS[node.text]) {
      node = this.element();
      if (node) { nodes.push(node); }
    }
    return nodes;
  };

  // element parses one element; null means "nothing here", and the caller
  // (usually an unbalanced closing brace) walks over it as text.
  Parser.prototype.element = function () {
    var ch = this.src.charAt(this.pos), nodes;
    if (ch === '') { return null; }
    if (ch === '{') {
      this.pos++;
      nodes = this.list('}');
      if (this.src.charAt(this.pos) === '}') { this.pos++; }
      return { type: 'row', children: nodes };
    }
    if (ch === '&') { this.pos++; return { type: 'cell' }; }   // an alignment's column separator
    if (ch === '}') { return null; }
    if (ch === '\\') { this.pos++; return this.command(); }
    if (ch === '~') { this.pos++; return { type: 'mtext', text: '\u00A0' }; }
    if (ch === ' ' || ch === '\t' || ch === '\n' || ch === '\r') {
      this.pos++;
      return { type: 'ignore' };
    }
    if (/[0-9]/.test(ch)) { return this.number(); }
    if (/[A-Za-z]/.test(ch)) { return this.letters(); }
    if (isWide(ch)) { return this.wide(); }
    this.pos++;
    return { type: 'mo', text: OPERATOR_CHARS[ch] || ch };
  };

  Parser.prototype.number = function () {
    var out = /^[0-9]+/.exec(this.src.slice(this.pos))[0], more;
    this.pos += out.length;
    for (;;) {
      if (this.src.charAt(this.pos) !== '.' || !/[0-9]/.test(this.src.charAt(this.pos + 1))) {
        break;
      }
      more = /^[0-9]+/.exec(this.src.slice(this.pos + 1))[0];
      out += '.' + more;
      this.pos += 1 + more.length;
    }
    return { type: 'mn', text: out };
  };

  Parser.prototype.letters = function () {
    var word = /^[A-Za-z]+/.exec(this.src.slice(this.pos))[0];
    this.pos += word.length;
    return { type: 'mi', text: word };
  };

  Parser.prototype.wide = function () {
    var start = this.pos;
    while (this.pos < this.src.length && isWide(this.src.charAt(this.pos))) { this.pos++; }
    return { type: 'mtext', text: this.src.slice(start, this.pos) };
  };
  Parser.prototype.command = function () {
    var name = this.readName();
    if (name.length === 1) { return this.singleChar(name); }
    if (SYMBOLS[name]) { return this.symbol(name); }
    if (BIG_OPS[name]) {
      return { type: 'big', char: BIG_OPS[name][0], movable: BIG_OPS[name][1] };
    }
    if (SPACES[name] !== undefined) { return { type: 'space', width: SPACES[name] }; }
    if (ACCENTS[name]) {
      return { type: 'accent', accent: ACCENTS[name][0], where: ACCENTS[name][1], base: this.argument() };
    }
    if (VARIANTS[name]) {
      return {
        type: 'variant',
        variant: VARIANTS[name][0],
        cls: VARIANTS[name][1],
        children: this.argument()
      };
    }
    if (TEXT_COMMANDS[name]) { return { type: 'mtext', text: textOf(this.rawGroup()) }; }
    if (name === 'frac' || name === 'dfrac' || name === 'tfrac' || name === 'cfrac') {
      return { type: 'frac', num: this.argument(), den: this.argument() };
    }
    if (name === 'pmod') { return { type: 'pmod', children: this.argument() }; }
    if (name === 'binom' || name === 'dbinom' || name === 'tbinom') {
      return { type: 'binom', top: this.argument(), bottom: this.argument() };
    }
    if (name === 'sqrt') { return this.root(); }
    if (name === 'left') { return this.sized(); }
    if (SIZED_FENCES[name]) { return this.fence(); }
    if (name === 'begin') { return this.environment(); }
    if (name === 'end') {
      // A stray \end (the opening \begin was dropped) only goes away.
      this.rawGroup();
      return { type: 'ignore' };
    }
    if (IGNORED[name]) { return { type: 'ignore' }; }
    // An unknown command is shown as its own name (upright) rather than dropped.
    return { type: 'mtext', text: name };
  };

  // singleChar handles the one-character commands: \%, \{, \,, \\ …
  Parser.prototype.singleChar = function (ch) {
    if (SPACES[ch] !== undefined) { return { type: 'space', width: SPACES[ch] }; }
    if (ch === '\\') { return { type: 'break' }; }                  // \\ ends a table row
    if (ch === '|') { return { type: 'fence', text: '\u2016' }; }    // \| is the norm's double bar
    if (FENCES[ch] !== undefined) {
      return FENCES[ch] === '' ? { type: 'ignore' } : { type: 'fence', text: FENCES[ch] };
    }
    if (ch === '') { return { type: 'ignore' }; }
    return { type: 'mo', text: ch };
  };

  // root parses \sqrt, with or without the optional index (\sqrt[3]{x}).
  Parser.prototype.root = function () {
    var index = null, end;
    if (this.src.charAt(this.pos) === '[') {
      end = this.src.indexOf(']', this.pos);
      if (end !== -1) {
        index = new Parser(this.src.slice(this.pos + 1, end)).list(null);
        this.pos = end + 1;
      }
    }
    return { type: 'root', index: index, radicand: this.argument() };
  };

  // rawGroup returns the content of the next {...} group exactly as it was
  // written: a text-mode command shows it, it does not typeset it.
  Parser.prototype.rawGroup = function () {
    if (this.src.charAt(this.pos) !== '{') { return ''; }
    var depth = 0, start = this.pos + 1, i = this.pos, ch;
    for (; i < this.src.length; i++) {
      ch = this.src.charAt(i);
      if (ch === '\\') { i++; continue; }   // an escaped brace does not count
      if (ch === '{') { depth++; } else if (ch === '}') {
        depth--;
        if (depth === 0) { break; }
      }
    }
    var text = this.src.slice(start, i);
    this.pos = i < this.src.length ? i + 1 : i;
    return text;
  };

  // textOf undoes the escapes a text-mode argument uses, so that \text{a\_b}
  // reads the way it was written.
  function textOf(text) {
    return String(text).replace(/\\([{}%$&#_])/g, '$1').replace(/\\ /g, ' ');
  }

  // fence reads the delimiter that follows \big (or a stray \right); \left and
  // \right themselves are paired by sized.
  Parser.prototype.fence = function () {
    var text = this.delimiterText();
    return text === '' ? { type: 'ignore' } : { type: 'fence', text: text };
  };

  // delimiterText reads the delimiter of \left, \right or \big: a bare character
  // or the command standing for it (\{ , \langle …). An empty string means "no
  // visible delimiter" (\left. and \right.).
  Parser.prototype.delimiterText = function () {
    var ch = this.src.charAt(this.pos), name, text;
    if (ch === '') { return ''; }
    if (ch === '\\') {
      this.pos++;
      name = this.readName();
      text = FENCES['\\' + name];
      if (text === undefined) { text = SYMBOLS[name] ? SYMBOLS[name][1] : name; }
      return text;
    }
    this.pos++;
    text = FENCES[ch];
    return text === undefined ? ch : text;
  };

  // sized parses a \left … \right group: TeX's own pairing, which is what makes
  // the two delimiters stretch to the same height and a script land on the whole
  // group. A \left whose \right has not arrived (a streaming formula) keeps its
  // delimiter and the content after it.
  Parser.prototype.sized = function () {
    var open = this.delimiterText();
    var children = [], node, close = null, row;
    while (this.pos < this.src.length) {
      if (RIGHT_COMMAND.test(this.src.slice(this.pos))) {
        this.pos += 6;                       // \right
        close = this.delimiterText();
        break;
      }
      node = this.atom();
      if (!node) { break; }
      if (node.type !== 'ignore') { children.push(node); }
    }
    if (close === null) {
      row = [];
      if (open !== '') { row.push({ type: 'fence', text: open }); }
      return { type: 'row', children: row.concat(children) };
    }
    return { type: 'fenced', open: open, close: close, children: children };
  };

  // environment parses an alignment environment (\begin{aligned} … \end{aligned})
  // into a table with the fences its name implies.
  Parser.prototype.environment = function () {
    var name = this.rawGroup().trim();
    var kind = ENVIRONMENTS[name];
    var spec = kind === 'array' ? this.rawGroup() : '';
    var source = this.innerSource(name);
    var nodes = new Parser(source).list(null);
    if (!kind) {
      // An environment the renderer does not lay out: its content is still shown.
      return { type: 'row', children: nodes };
    }
    return tableNode(splitRows(nodes) || [nodes], kind, spec);
  };

  // innerSource returns the source between \begin{name} and the matching
  // \end{name}, nested environments included; a missing \end (still streaming)
  // takes the rest of the formula.
  Parser.prototype.innerSource = function (name) {
    var start = this.pos, depth = 0, i = start, begin, end, close, env;
    for (;;) {
      begin = this.src.indexOf('\\begin{', i);
      end = this.src.indexOf('\\end{', i);
      if (end === -1) {
        this.pos = this.src.length;
        return this.src.slice(start);
      }
      if (begin !== -1 && begin < end) {
        depth++;
        i = begin + 7;
        continue;
      }
      close = this.src.indexOf('}', end);
      env = close === -1 ? '' : this.src.slice(end + 5, close);
      if (depth > 0) {                       // it closes a nested environment
        depth--;
        i = end + 5;
        continue;
      }
      if (env === name) {
        this.pos = close === -1 ? this.src.length : close + 1;
        return this.src.slice(start, end);
      }
      i = end + 5;                           // a \end of some other environment
    }
  };

  Parser.prototype.symbol = function (name) {
    var def = SYMBOLS[name];
    return { type: def[0], text: def[1], variant: def[2] || '' };
  };
  // --------------------------------------------------------------- rendering --

  function emit(nodes, ctx) {
    var out = '';
    for (var i = 0; i < nodes.length; i++) { out += renderNode(nodes[i], ctx); }
    return out;
  }

  // prepare turns a parsed list into what the renderer walks: a list with row
  // breaks becomes a table, and the fence pairs in it are bound.
  function prepare(nodes) {
    var rows = splitRows(nodes);
    if (rows) { return [tableNode(rows, 'bare', '')]; }
    return bindFences(nodes);
  }

  // splitRows splits a list at the row breaks (\\) and each row at the column
  // separators (&). It returns null when the list has neither, which means it is
  // plain one-line math.
  function splitRows(nodes) {
    var found = false, i, node;
    for (i = 0; i < nodes.length; i++) {
      if (nodes[i].type === 'break' || nodes[i].type === 'cell') { found = true; break; }
    }
    if (!found) { return null; }
    var rows = [], cells = [], row = [];
    for (i = 0; i < nodes.length; i++) {
      node = nodes[i];
      if (node.type === 'break') {
        cells.push(row);
        rows.push(cells);
        cells = [];
        row = [];
        continue;
      }
      if (node.type === 'cell') { cells.push(row); row = []; continue; }
      row.push(node);
    }
    cells.push(row);
    rows.push(cells);
    // A trailing \\ leaves an empty last row (cases blocks end that way).
    while (rows.length && rows[rows.length - 1].length === 1 && rows[rows.length - 1][0].length === 0) {
      rows.pop();
    }
    return rows.length ? rows : [[]];
  }

  // bindFences pairs the delimiters of a list. `(x)^2` has to become one group
  // carrying the exponent: the engine sizes a stretchy delimiter against its
  // siblings, so a closing bracket inside msup would not be a sibling of the
  // content any more — the opening bracket would stretch to the height of the
  // exponent while the closing one stayed small.
  function bindFences(nodes) {
    var out = [], opens = [], i, k, node, role, at, entry, closer, children;
    for (i = 0; i < nodes.length; i++) {
      node = nodes[i];
      role = fenceRole(node);
      if (!role) { out.push(node); continue; }
      at = -1;
      if (role.closes) {
        for (k = opens.length - 1; k >= 0; k--) {
          if (opens[k].kind === role.kind) { at = k; break; }
        }
      }
      if (at === -1) {
        out.push(node);
        // A delimiter that could either open or close (|x|) and that carries a
        // script of its own is left alone: binding it would lose that script.
        if (role.opens && node.type !== 'scripts') {
          opens.push({ kind: role.kind, at: out.length - 1 });
        }
        continue;
      }
      entry = opens[at];
      opens.length = at;
      closer = out[entry.at];
      children = out.slice(entry.at + 1);
      out.length = entry.at;
      out.push(rebaseScripts(node, {
        type: 'fenced',
        open: fenceText(closer),
        close: fenceText(node),
        children: children
      }));
    }
    return out;
  }

  // fenceRole reports the delimiter role of a node (looking through a script that
  // sits on the delimiter), or null when the node is not a delimiter.
  function fenceRole(node) {
    var fence = node.type === 'scripts' ? node.base : node;
    if (!fence || (fence.type !== 'fence' && fence.type !== 'mo')) { return null; }
    var role = FENCE_ROLES[fence.text];
    return role ? { kind: role[0], opens: role[1], closes: role[2] } : null;
  }

  function fenceText(node) {
    return (node.type === 'scripts' ? node.base : node).text;
  }

  // rebaseScripts keeps a script that sat on the closing delimiter: after the
  // binding it belongs to the whole group.
  function rebaseScripts(node, group) {
    if (node.type !== 'scripts') { return group; }
    return { type: 'scripts', base: group, sub: node.sub, sup: node.sup };
  }

  // tableNode builds the table of an environment (or of a bare row break): kind
  // says what the environment implies, spec is an array's column spec.
  function tableNode(rows, kind, spec) {
    var node = { type: 'table', rows: rows, align: '', spec: '', open: '', close: '' };
    if (kind === 'aligned') { node.align = 'aligned'; }
    else if (kind === 'cases') { node.align = 'left'; node.open = '{'; }
    else if (kind === 'array') { node.align = 'array'; node.spec = spec || ''; }
    else if (kind === 'paren') { node.open = '('; node.close = ')'; }
    else if (kind === 'bracket') { node.open = '['; node.close = ']'; }
    else if (kind === 'brace') { node.open = '{'; node.close = '}'; }
    else if (kind === 'bar') { node.open = '|'; node.close = '|'; }
    else if (kind === 'norm') { node.open = '\u2016'; node.close = '\u2016'; }
    return node;
  }

  // group renders a node sequence as one MathML child.
  function group(nodes, ctx) {
    if (!nodes || nodes.length === 0) { return '<mrow></mrow>'; }
    var list = prepare(nodes);
    if (list.length === 1) { return renderNode(list[0], ctx); }
    return '<mrow>' + emit(list, ctx) + '</mrow>';
  }

  function renderNode(node, ctx) {
    switch (node.type) {
      case 'space': return '<mspace width="' + node.width + '"/>';
      case 'mtext': return '<mtext>' + esc(node.text) + '</mtext>';
      case 'mi': return '<mi' + styleAttrs(node) + '>' + esc(node.text) + '</mi>';
      case 'mo': return '<mo>' + esc(node.text) + '</mo>';
      case 'mn': return '<mn>' + esc(node.text) + '</mn>';
      case 'fence': return '<mo stretchy="true">' + esc(node.text) + '</mo>';
      case 'big': return '<mo>' + node.char + '</mo>';
      case 'row': return group(node.children, ctx);
      case 'break': return '';                        // nothing to break outside a table
      case 'cell': return '';                         // nor a column to separate
      case 'fenced': return renderFenced(node, ctx);
      case 'table': return renderTable(node, ctx);
      case 'binom': return renderBinom(node, ctx);
      case 'pmod':
        return '<mrow><mtext>(mod</mtext><mspace width="0.25em"/>' + group(node.children, ctx) + '<mo>)</mo></mrow>';
      case 'variant': return renderVariant(node, ctx);
      case 'accent':
        return node.where === 'under'
          ? '<munder accentunder="true">' + group(node.base, ctx) +
            '<mo stretchy="true">' + esc(node.accent) + '</mo></munder>'
          : '<mover accent="true">' + group(node.base, ctx) +
            '<mo stretchy="true">' + esc(node.accent) + '</mo></mover>';
      case 'frac': return '<mfrac>' + group(node.num, ctx) + group(node.den, ctx) + '</mfrac>';
      case 'root': return node.index
        ? '<mroot>' + group(node.radicand, ctx) + group(node.index, ctx) + '</mroot>'
        : '<msqrt>' + emit(prepare(node.radicand), ctx) + '</msqrt>';
      case 'scripts': return renderScripts(node, ctx);
    }
    return '';
  }

  // renderScripts attaches a superscript and/or a subscript. A large operator
  // keeps its limits under and over the glyph in display style, the way TeX sets
  // a \sum in a displayed formula.
  function renderScripts(node, ctx) {
    var under = node.base.type === 'big' && node.base.movable && ctx.display;
    var base = renderNode(node.base, ctx);
    var sub = node.sub ? group(node.sub, ctx) : '';
    var sup = node.sup ? group(node.sup, ctx) : '';
    if (sub && sup) {
      return under
        ? '<munderover>' + base + sub + sup + '</munderover>'
        : '<msubsup>' + base + sub + sup + '</msubsup>';
    }
    if (sub) { return under ? '<munder>' + base + sub + '</munder>' : '<msub>' + base + sub + '</msub>'; }
    if (sup) { return under ? '<mover>' + base + sup + '</mover>' : '<msup>' + base + sup + '</msup>'; }
    return base;
  }

  // renderFenced draws a delimited group. Both delimiters are marked stretchy so
  // the engine grows them to the content — and, when the group carries a script,
  // to the same height on both sides, with the exponent beside the group the way
  // TeX sets it.
  function renderFenced(node, ctx) {
    var open = '', close = '';
    if (node.open !== '') { open = '<mo stretchy="true">' + esc(node.open) + '</mo>'; }
    if (node.close !== '') { close = '<mo stretchy="true">' + esc(node.close) + '</mo>'; }
    return '<mrow>' + open + emit(prepare(node.children), ctx) + close + '</mrow>';
  }

  // renderBinom draws \binom{n}{k}: a fraction without its bar, in brackets.
  function renderBinom(node, ctx) {
    return '<mrow><mo stretchy="true">(</mo>' +
      '<mfrac linethickness="0">' + group(node.top, ctx) + group(node.bottom, ctx) + '</mfrac>' +
      '<mo stretchy="true">)</mo></mrow>';
  }

  // renderTable draws a table — an alignment environment, or the rows a bare \\
  // produced — inside the fences its environment implies.
  function renderTable(node, ctx) {
    var rows = node.rows, columns = 0, align, html, i, j;
    for (i = 0; i < rows.length; i++) {
      if (rows[i].length > columns) { columns = rows[i].length; }
    }
    align = tableAlign(node, columns);
    html = align === '' ? '<mtable>' : '<mtable columnalign="' + align + '">';
    for (i = 0; i < rows.length; i++) {
      html += '<mtr>';
      for (j = 0; j < rows[i].length; j++) {
        html += '<mtd>' + emit(prepare(rows[i][j]), ctx) + '</mtd>';
      }
      html += '</mtr>';
    }
    html += '</mtable>';
    if (node.open === '' && node.close === '') { return html; }
    return '<mrow>' +
      (node.open === '' ? '' : '<mo stretchy="true">' + esc(node.open) + '</mo>') + html +
      (node.close === '' ? '' : '<mo stretchy="true">' + esc(node.close) + '</mo>') + '</mrow>';
  }

  // tableAlign builds the columnalign list of a table: an aligned block alternates
  // right and left at the & marks (TeX's rule), a cases block keeps everything
  // left, an array follows its own spec, and everything else stays centred — the
  // engine's default, which is also what a bare \\ wants.
  function tableAlign(node, columns) {
    var out = [], i, ch;
    if (columns < 2) { return ''; }
    if (node.align === 'array') {
      for (i = 0; i < node.spec.length; i++) {
        ch = ARRAY_ALIGN[node.spec.charAt(i)];
        if (ch) { out.push(ch); }
      }
      return out.join(' ');
    }
    if (node.align === 'aligned') {
      for (i = 0; i < columns; i++) { out.push(i % 2 === 0 ? 'right' : 'left'); }
      return out.join(' ');
    }
    if (node.align === 'left') {
      for (i = 0; i < columns; i++) { out.push('left'); }
      return out.join(' ');
    }
    return '';
  }

  // styleAttrs writes the font a leaf asks for: the mathvariant attribute for the
  // engines that still implement it, and the class the page stylesheet styles for
  // Chromium, which reads CSS instead (MathML Core dropped the attribute).
  function styleAttrs(node) {
    var cls;
    if (!node.variant) { return ''; }
    cls = node.cls || (node.variant === 'normal' ? 'normal' : '');
    return ' mathvariant="' + node.variant + '"' + (cls ? ' class="mv-' + cls + '"' : '');
  }

  // renderVariant draws a font command. \mathbb is turned into the real
  // double-struck characters (ℝ, ℂ …), which every engine draws; anything else
  // keeps the attribute and the class of its variant.
  function renderVariant(node, ctx) {
    var letters;
    if (node.variant === 'double-struck') {
      letters = doubleStruck(node.children);
      if (letters) { return emit(letters, ctx); }
    }
    return '<mstyle mathvariant="' + node.variant + '" class="mv-' + node.cls + '">' +
      emit(prepare(node.children), ctx) + '</mstyle>';
  }

  // doubleStruck rewrites the content of \mathbb as double-struck characters, or
  // returns null when it holds something Unicode has no character for.
  function doubleStruck(nodes) {
    var out = [], i, k, node, text, ch;
    for (i = 0; i < nodes.length; i++) {
      node = nodes[i];
      if (node.type !== 'mi' && node.type !== 'mn') { return null; }
      text = '';
      for (k = 0; k < node.text.length; k++) {
        ch = doubleStruckChar(node.text.charAt(k));
        if (!ch) { return null; }
        text += ch;
      }
      out.push({ type: 'mi', text: text, variant: 'normal' });
    }
    return out;
  }

  // doubleStruckChar returns the double-struck character of a letter or digit
  // (the seven straked capitals live in the Letterlike Symbols block), or an
  // empty string when Unicode has none.
  function doubleStruckChar(ch) {
    var code = ch.charCodeAt(0);
    if (DOUBLE_STRUCK_HOLES[ch]) { return DOUBLE_STRUCK_HOLES[ch]; }
    if (code >= 65 && code <= 90) { return String.fromCodePoint(0x1D538 + (code - 65)); }
    if (code >= 97 && code <= 122) { return String.fromCodePoint(0x1D552 + (code - 97)); }
    if (code >= 48 && code <= 57) { return String.fromCodePoint(0x1D7D8 + (code - 48)); }
    return '';
  }

  // mathML returns just the <math> element. A display formula is wrapped in
  // mstyle displaystyle, which is what makes the engine set it in display style
  // (big operators grow, their limits move under and over them).
  function mathML(tex, display) {
    var body = emit(prepare(new Parser(tex).list(null)), { display: !!display });
    if (display) { body = '<mstyle displaystyle="true">' + body + '</mstyle>'; }
    return '<math>' + body + '</math>';
  }

  // render wraps a formula in the span the page stylesheet lays out: a display
  // formula is centred, and a wide one scrolls sideways on a phone instead of
  // overflowing the row. A formula that cannot be parsed is shown as it was
  // written rather than dropped.
  function render(tex, display, raw) {
    var cls = display ? 'mtex mtex-block' : 'mtex';
    try {
      return '<span class="' + cls + '">' + mathML(tex, display) + '</span>';
    } catch (err) {
      return '<span class="' + cls + '">' + esc(raw || tex) + '</span>';
    }
  }
  // -------------------------------------------------------- markdown glue --
  // A formula is lifted out of the message before markdown runs and put back
  // after it (see the note at the top of this file). The placeholder is a NUL
  // byte around an index: no reply contains one, and markdown passes it through
  // untouched, so inject finds every formula again by its own token.

  var TOKEN = '\u0000';

  // escaped reports whether the character at at is escaped by a backslash.
  function escaped(text, at) {
    var count = 0, i = at - 1;
    while (i >= 0 && text.charAt(i) === '\\') { count++; i--; }
    return (count % 2) === 1;
  }

  // findDelim finds the next occurrence of a delimiter that is not escaped.
  function findDelim(text, delim, from) {
    var at = text.indexOf(delim, from);
    while (at !== -1) {
      if (!escaped(text, at)) { return at; }
      at = text.indexOf(delim, at + delim.length);
    }
    return -1;
  }

  // fenceMark reports the code fence that starts at at, if any.
  function fenceMark(text, at) {
    var ch = text.charAt(at);
    if (ch !== '`' && ch !== '~') { return ''; }
    var run = /^(`+|~+)/.exec(text.slice(at))[0];
    return run.length >= 3 ? run : '';
  }

  // dollarOpen/dollarClose implement the usual guards for inline math: a lone $
  // and a price like $5 stay text, and a delimiter never touches a space, so
  // "weighs $ 5 and $ 6" is not a formula either. The closing delimiter is
  // looked for on the same line first, so a formula whose closer never arrives
  // does not swallow the next line's text.
  function dollarOpen(text, at) {
    if (escaped(text, at)) { return false; }
    var next = text.charAt(at + 1);
    return next !== '' && next !== '$' && !/\s/.test(next);
  }

  function dollarClose(text, from) {
    var lineEnd = text.indexOf('\n', from);
    var at = closerFrom(text, from, lineEnd === -1 ? text.length : lineEnd);
    return at !== -1 ? at : closerFrom(text, from, text.length);
  }

  // closerFrom finds the first delimiter that may close a formula before limit.
  function closerFrom(text, from, limit) {
    var at = text.indexOf('$', from);
    while (at !== -1 && at < limit) {
      if (!escaped(text, at) && at > from && text.charAt(at + 1) !== '$' &&
        !/\s/.test(text.charAt(at - 1))) {
        return at;
      }
      at = text.indexOf('$', at + 1);
    }
    return -1;
  }

  // PRICE is a body that holds nothing but a number: "$7" is a price, not a
  // formula, however it is glued to the words around it.
  var PRICE = /^[0-9][0-9.,\s]*$/;

  // MATH_HINT is what a body with spaces has to carry to count as a formula: a
  // command, one of the characters only math uses as syntax, or a symbol from
  // the operator blocks.
  var MATH_HINT = /[\\^_=+\-*/<>|(){}\[\]'%\u00B1\u00D7\u00F7]|[\u2190-\u21FF\u2200-\u22FF\u2A00-\u2AFF]/;

  // looksLikeMath keeps prose with prices out of the formula path: "$5 and $6"
  // is a sentence, "$f \ge 5$" is not, and a single token without spaces ($x$)
  // is taken at its word.
  function looksLikeMath(tex) {
    if (!/\s/.test(tex)) { return true; }
    return MATH_HINT.test(tex);
  }

  // inlineBody returns the TeX of the inline formula that starts at at, or an
  // empty string when the dollars there are not a formula: an unpaired $, a
  // price ($5 and $6), or a body that reads as prose.
  function inlineBody(text, at) {
    if (!dollarOpen(text, at)) { return ''; }
    var close = dollarClose(text, at + 1);
    if (close === -1) { return ''; }
    var body = text.slice(at + 1, close);
    if (body.indexOf('$') !== -1) { return ''; }        // the delimiters do not pair up
    if (PRICE.test(body)) { return ''; }                // a price keeps its dollars
    return looksLikeMath(body) ? body : '';
  }

  // lift renders one formula and returns the placeholder that stands in for it;
  // the formula itself waits in items until the markdown pass is over.
  function lift(items, tex, display) {
    var raw = (display ? '$$' : '$') + tex + (display ? '$$' : '$');
    var item = {
      token: TOKEN + items.length + TOKEN,
      html: render(tex, display, raw)
    };
    items.push(item);
    return item.token;
  }
  // protect lifts every formula out of a message. Code is skipped on the way: a
  // formula inside a fence or an inline span stays code, and the delimiters of a
  // formula whose closer has not arrived yet (a reply that is still streaming)
  // stay text until it does.
  function protect(src) {
    var text = String(src);
    var items = [], out = '';
    var fence = '', i = 0, n = text.length, lineStart = true;
    while (i < n) {
      if (lineStart) {
        var indent = 0, mark, eol;
        while (indent < 3 && text.charAt(i + indent) === ' ') { indent++; }
        mark = fenceMark(text, i + indent);
        if (mark) {
          if (fence === '') {
            fence = mark;
          } else if (mark.charAt(0) === fence.charAt(0) && mark.length >= fence.length) {
            fence = '';                       // the fence closes here
          }
          eol = text.indexOf('\n', i);
          if (eol === -1) { out += text.slice(i); break; }
          out += text.slice(i, eol + 1);
          i = eol + 1;
          continue;                           // the next line starts here
        }
        lineStart = false;
      }
      var ch = text.charAt(i);
      if (ch === '\n') { lineStart = true; out += ch; i++; continue; }
      if (fence !== '') { out += ch; i++; continue; }
      if (ch === '\\') { out += text.slice(i, i + 2); i += 2; continue; }
      if (ch === '`') {
        var run = /^`+/.exec(text.slice(i))[0];
        var lineEnd = text.indexOf('\n', i);
        var close = text.indexOf(run, i + run.length);
        // A span never reaches across the line here: an unpaired run of
        // backticks is ordinary text, not the start of a span that swallows the
        // rest of the message.
        if (close !== -1 && (lineEnd === -1 || close < lineEnd)) {
          out += text.slice(i, close + run.length);
          i = close + run.length;
          continue;
        }
        out += run;
        i += run.length;
        continue;
      }
      if (ch === '$' && text.charAt(i + 1) === '$') {
        var endD = findDelim(text, '$$', i + 2);
        if (endD === -1) { out += text.slice(i); break; }   // half-typed: leave it alone
        var body = text.slice(i + 2, endD);
        if (body.trim() === '') { out += text.slice(i, endD + 2); } else { out += lift(items, body, true); }
        i = endD + 2;
        continue;
      }
      if (ch === '$') {
        var inline = inlineBody(text, i);
        if (inline !== '') {
          out += lift(items, inline, false);
          i += inline.length + 2;
          continue;
        }
      }
      out += ch;
      i++;
    }
    return { text: out, items: items };
  }

  // inject puts every rendered formula back, right before the caller sanitizes
  // the HTML: markup the sanitizer would refuse never gets in. One pass over the
  // HTML through a token -> markup table, rather than a split/join per formula,
  // so a reply rich in formulas does not pay a full scan for each of them.
  function inject(html, items) {
    var out = String(html);
    if (!items || items.length === 0) { return out; }
    var table = {};
    for (var i = 0; i < items.length; i++) { table[items[i].token] = items[i].html; }
    var re = new RegExp(TOKEN + '\\d+' + TOKEN, 'g');
    return out.replace(re, function (token) {
      var markup = table[token];
      return markup === undefined ? token : markup;
    });
  }

  window.MathTex = {
    protect: protect,
    inject: inject,
    render: render,
    mathML: mathML
  };
})();

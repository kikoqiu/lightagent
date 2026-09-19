(function () {
  var log = document.getElementById('log');
  var input = document.getElementById('input');
  var dot = document.getElementById('dot');
  var statusEl = document.getElementById('status');
  var usageEl = document.getElementById('usage');
  var runEl = document.getElementById('run');
  var stopEl = document.getElementById('stop');
  var sendEl = document.getElementById('send');
  var current = null;
  var currentText = '';
  // pendingRows holds the rows of messages this page submitted while the turn was
  // running (steering). Such a row is drawn right away, marked as pending, and
  // every row the running reply still produces is inserted *before* it: the
  // previous round's feedback always stays above the inserted message. When the
  // agent sends the message the row becomes an ordinary one (see the user case in
  // render), because that is the moment the message joins the conversation.
  var pendingRows = [];
  var pendingRender = null;
  // reasoningRow/reasoningText stream the model's "thinking" (reasoning_delta);
  // it is finalized before the visible answer is drawn.
  var reasoningRow = null;
  var reasoningText = '';
  var pendingReasoningRender = null;
  // replaying is true while a snapshot rebuilds the log (history_start …
  // history_end). It suppresses live-only side effects (a replayed user row must
  // not light up the running indicator), holds the markdown work back to idle
  // slices and makes row insertion batch, so a long conversation rebuilds
  // without freezing the main thread.
  var replaying = false;
  // replayBatch collects the rows of the snapshot while it is being replayed;
  // it is inserted as a whole (see flushReplayBatch), which costs one reflow per
  // batch instead of a layout read plus a scroll write per row.
  var replayBatch = null;
  // The command rail's switches (/result, /markdown) are the only commands with
  // state: the rail shows it and sends the explicit opposite, so one click
  // always lands on the state the user asked for. The values arrive with the
  // injected page config, with a settings frame (another tab flipped one) or in
  // a history frame (after a reconnect).
  var switchKeys = { '/result': 'result', '/markdown': 'markdown' };
  var switchOn = { '/result': true, '/markdown': true };
  var switchEls = {};

  // The transcript follows new output exactly while the reader is already at the
  // bottom: dragging (or wheeling) up to read or copy history unpins the view, so
  // streamed output keeps growing below without yanking the reader back down.
  // Scrolling back to the bottom resumes following. The slack absorbs sub-pixel
  // rounding, so "at the bottom" does not need an exact scrollHeight match.
  //
  // Everything that changes the log samples atBottom() *before* touching the DOM
  // and calls pinBottom() afterwards only when it was true: adding or growing a
  // row moves the bottom away, so checking after the change would always report a
  // scrolled-away view and nothing would ever auto-follow. Sampling first also
  // keeps a drag intact without listening for scroll events.
  var STICK_SLACK = 8;

  function atBottom() {
    return log.scrollHeight - log.scrollTop - log.clientHeight <= STICK_SLACK;
  }

  // pinBottom shows the newest row regardless of where the reader was; on an
  // empty (freshly rebuilt) log it also establishes "at the bottom".
  function pinBottom() { log.scrollTop = log.scrollHeight; }

  // The elapsed badge in the running pill times the current turn, mirroring the
  // CLI prompt (busyElapsedLocked): tenths of a second up to a minute, then
  // minutes and seconds, then hours and minutes. It starts with the spinner,
  // keeps counting between events, and is cleared when the turn ends.
  var elapsedEl = document.getElementById('elapsed');
  var turnStart = 0;
  var turnTimer = null;

  function pad2(n) { return (n < 10 ? '0' : '') + n; }

  // queued counts the messages this page sent while the turn was already
  // running. Their rows appear when the agent folds them into the conversation
  // (after the reply they interrupted), so until then this count in the running
  // pill is the only acknowledgement that they went through.
  var queued = 0;

  function queuedText() {
    return queued > 0 ? ' · ' + queued + ' queued' : '';
  }

  function elapsedText(ms) {
    var s = ms / 1000;
    if (s < 60) { return s.toFixed(1) + 's'; }
    if (s < 3600) { return Math.floor(s / 60) + 'm' + pad2(Math.floor(s) % 60) + 's'; }
    return Math.floor(s / 3600) + 'h' + pad2(Math.floor(s / 60) % 60) + 'm';
  }

  function tickTurn() {
    if (elapsedEl) { elapsedEl.textContent = elapsedText(Date.now() - turnStart) + queuedText(); }
  }

  // A steering message joins the running turn, so an already-ticking clock is
  // left alone (the CLI does the same). The tick mirrors its 100ms animation.
  function startTurnTimer() {
    if (turnTimer) { return; }
    turnStart = Date.now();
    tickTurn();
    turnTimer = setInterval(tickTurn, 100);
  }

  function stopTurnTimer() {
    if (turnTimer) { clearInterval(turnTimer); turnTimer = null; }
    turnStart = 0;
    if (elapsedEl) { elapsedEl.textContent = ''; }
  }

  // running reports whether a turn is in progress (see setRunning).
  var running = false;

  // setRunning shows or hides the "running" indicator (and the Stop button). A
  // turn is in progress between a user message and the matching turn_done.
  function setRunning(on) {
    if (replaying) { return; }
    running = !!on;
    if (on) { runEl.classList.add('on'); stopEl.classList.add('on'); startTurnTimer(); }
    else {
      // The turn is over: nothing can be waiting to be folded in any more.
      queued = 0;
      runEl.classList.remove('on'); stopEl.classList.remove('on'); stopTurnTimer();
    }
  }

  // setUsage renders the context-usage badge in the header and mirrors the
  // numbers into the desktop side rail meter (which changes colour as the
  // window fills up). Both targets are optional so the page keeps working.
  function setUsage(tokens, win) {
    var fill = document.getElementById('meterFill');
    var pctEl = document.getElementById('meterPct');
    var noteEl = document.getElementById('meterNote');
    if (!win) {
      usageEl.textContent = '';
      if (fill) { fill.style.width = '0%'; fill.className = ''; }
      if (pctEl) { pctEl.textContent = '—'; }
      if (noteEl) { noteEl.textContent = 'waiting for usage…'; }
      return;
    }
    var pct = win > 0 ? (tokens * 100 / win) : 0;
    usageEl.textContent = 'context ' + pct.toFixed(1) + '% (' + tokens + '/' + win + ' tokens)';
    var clamped = Math.max(0, Math.min(100, pct));
    if (fill) {
      fill.style.width = clamped + '%';
      fill.className = pct >= 90 ? 'hot' : (pct >= 70 ? 'warm' : '');
    }
    if (pctEl) { pctEl.textContent = pct.toFixed(1) + '%'; }
    if (noteEl) { noteEl.textContent = tokens + ' / ' + win + ' tokens'; }
  }

  // syncResults follows the shared /result switch: the server announces every
  // change (made here, in the CLI or in another tab) as an info row, so the rail
  // stays accurate.
  function syncResults(text) {
    if (/hiding tool\/exec results/i.test(text)) { setSwitch('/result', false); }
    else if (/showing tool\/exec results/i.test(text)) { setSwitch('/result', true); }
  }

  // mdToHTML renders markdown with the vendored marked library and sanitizes the
  // result with DOMPurify. It returns null when rendering is disabled or the
  // libraries failed to load, so the caller falls back to plain text.
  function mdToHTML(text) {
    if (!MARKDOWN || typeof marked === 'undefined') { return null; }
    try {
      var html = marked.parse(text, { gfm: true, breaks: true });
      if (typeof DOMPurify !== 'undefined') { html = DOMPurify.sanitize(html); }
      return html;
    } catch (e) {
      return null;
    }
  }

  // MAX_MD_CHARS keeps one gigantic row (a huge tool result, a whole file in a
  // reply) from stalling the idle upgrade below: past this size the replayed row
  // stays plain text.
  var MAX_MD_CHARS = 200000;
  // Replaying a long conversation means parsing markdown for thousands of rows.
  // That work is sliced into idle chunks instead of running as one long task:
  // the main thread stays responsive, the transcript paints batch by batch, and
  // each replayed row's plain text is upgraded in place. Rows that arrive live
  // (a turn that is streaming) still render immediately.
  var mdPending = new Map(); // span -> latest text waiting for its markdown pass
  var mdFlushScheduled = false;
  var MD_SLICE_MS = 8;

  // applyMarkdown renders one row's text as markdown, falling back to plain
  // text when rendering is off or the libraries are missing.
  function applyMarkdown(span, text) {
    var html = mdToHTML(text);
    if (html !== null) { span.className = 'text md'; span.innerHTML = html; }
    else { span.className = 'text'; span.textContent = text; }
  }

  function queueMarkdown(span, text) {
    mdPending.set(span, text);
    scheduleMarkdownFlush();
  }

  function scheduleMarkdownFlush() {
    if (mdFlushScheduled || mdPending.size === 0) { return; }
    mdFlushScheduled = true;
    if (typeof window.requestIdleCallback === 'function') {
      // The timeout guarantees progress even while the snapshot keeps arriving.
      window.requestIdleCallback(flushMarkdown, { timeout: 250 });
    } else {
      window.setTimeout(function () { flushMarkdown(null); }, 16);
    }
  }

  // flushMarkdown upgrades as many queued rows as fit in one idle slice. The
  // Map keeps insertion order and the lowest text wins per row (a row that was
  // re-streamed while waiting is parsed once, with its final text).
  function flushMarkdown(deadline) {
    mdFlushScheduled = false;
    var started = Date.now();
    var entries = mdPending.entries();
    for (var next = entries.next(); !next.done; next = entries.next()) {
      var span = next.value[0];
      var text = next.value[1];
      mdPending.delete(span);
      // A row still sitting in the batch fragment counts too: only rows whose
      // log was replaced (no parent left) are skipped. The size cap keeps one
      // gigantic row from stalling the slice.
      if ((span.isConnected || span.parentNode) && text.length <= MAX_MD_CHARS) {
        applyMarkdown(span, text);
      }
      if (deadline && typeof deadline.timeRemaining === 'function') {
        if (deadline.timeRemaining() <= 1) { break; }
      } else if (Date.now() - started >= MD_SLICE_MS) {
        break;
      }
    }
    scheduleMarkdownFlush();
  }

  function setSpan(span, text, renderMD) {
    // While a snapshot is being replayed the markdown pass is deferred: the row
    // is drawn as plain text now and upgraded in an idle slice (see
    // queueMarkdown). Parsing thousands of rows in one synchronous pass is what
    // froze the tab (and hid the log behind "Waiting for messages…") on a long
    // conversation.
    if (renderMD && replaying) {
      span.className = 'text';
      span.textContent = text;
      queueMarkdown(span, text);
      return;
    }
    if (renderMD) { applyMarkdown(span, text); return; }
    span.className = 'text';
    span.textContent = text;
  }

  // scheduleRender re-renders the streaming row at most every 120ms.
  function scheduleRender() {
    if (pendingRender) { return; }
    pendingRender = setTimeout(function () {
      pendingRender = null;
      setRow(current, currentText, MARKDOWN);
    }, 120);
  }

  // scheduleReasoningRender re-renders the thinking row at most every 120ms,
  // through the same markdown path as the visible answer.
  function scheduleReasoningRender() {
    if (pendingReasoningRender) { return; }
    pendingReasoningRender = setTimeout(function () {
      pendingReasoningRender = null;
      setRow(reasoningRow, reasoningText, MARKDOWN);
    }, 120);
  }

  // finishReasoning commits the streamed thinking row (if any) so the visible
  // answer starts in its own row.
  function finishReasoning() {
    if (!reasoningRow) { return; }
    if (pendingReasoningRender) { clearTimeout(pendingReasoningRender); pendingReasoningRender = null; }
    setRow(reasoningRow, reasoningText, MARKDOWN);
    reasoningRow = null;
    reasoningText = '';
  }

  // buildRow creates one transcript row.
  function buildRow(cls, role, text, renderMD) {
    var row = document.createElement('div');
    row.className = 'row ' + cls;
    if (role) {
      var r = document.createElement('span');
      r.className = 'role';
      r.textContent = role;
      row.appendChild(r);
    }
    var t = document.createElement('span');
    setSpan(t, text, renderMD);
    row.appendChild(t);
    return row;
  }

  // placeRow adds a transcript row to the log: at the end, or before the pending
  // messages when some are waiting. The running reply's output is inserted before
  // them, so it always stays above the message that interrupted it. During a
  // history replay the rows go into the batch fragment instead (see
  // flushReplayBatch): no layout read, no scroll write, and the whole batch
  // reaches the document in one insert.
  function placeRow(row) {
    if (replayBatch) { replayBatch.appendChild(row); return; }
    var anchor = pendingRows.length ? pendingRows[0].el : null;
    // Follow the new row only if the reader was at the bottom (sampled first).
    var follow = atBottom();
    if (anchor) { log.insertBefore(row, anchor); } else { log.appendChild(row); }
    if (follow) { pinBottom(); }
  }

  function addRow(cls, role, text, renderMD) {
    var row = buildRow(cls, role, text, renderMD);
    placeRow(row);
    return row;
  }

  // addPendingRow draws a message submitted here while the turn was running. It is
  // appended after the pending rows already waiting (submission order) and stays
  // marked until the agent sends it: settlePendingRow then turns it into an
  // ordinary row, in place.
  function addPendingRow(text) {
    var row = buildRow('user pending', 'you', text, false);
    appendRow(row);
    pendingRows.push({ el: row, text: text });
    return row;
  }

  // settlePendingRow turns the pending row of a message the agent has just sent
  // into an ordinary one. It reports whether a row was converted; the row keeps
  // its place, which is where the message entered the conversation.
  function settlePendingRow(text) {
    for (var i = 0; i < pendingRows.length; i++) {
      if (pendingRows[i].text !== text) { continue; }
      var row = pendingRows[i].el;
      pendingRows.splice(i, 1);
      // The pending mark rides on the class alone; the text is already the final
      // one, since it is the message the agent recorded.
      row.className = 'row user';
      return true;
    }
    return false;
  }

  // appendRow puts a row at the very end of the log (the pending messages it
  // queues behind), following it when the reader was at the bottom.
  function appendRow(row) {
    var follow = atBottom();
    log.appendChild(row);
    if (follow) { pinBottom(); }
  }

  function setRow(el, text, renderMD) {
    if (!el) { return; }
    // A replayed row sits in the batch fragment (not in the log yet) and the view
    // is pinned once, when the snapshot ends: there is nothing to follow and no
    // layout to read here.
    if (replaying) { setSpan(el.lastChild, text, renderMD); return; }
    // A streamed re-render can grow the row, so the sample comes first too.
    var follow = atBottom();
    setSpan(el.lastChild, text, renderMD);
    if (follow) { pinBottom(); }
  }

  // parseArgObject turns a tool call's JSON argument string into an object, or
  // null when it is missing or is not a JSON object. Object.keys then keeps the
  // model's original argument order.
  function parseArgObject(raw) {
    if (!raw) { return null; }
    try {
      var v = JSON.parse(raw);
      if (v && typeof v === 'object' && !Array.isArray(v)) { return v; }
    } catch (e) {}
    return null;
  }

  // argText renders one argument value in its raw form: strings lose their
  // quotes, nested arrays/objects stay compact JSON.
  function argText(v) {
    if (typeof v === 'string') { return v; }
    if (v === null) { return 'null'; }
    if (typeof v === 'object') {
      try { return JSON.stringify(v); } catch (e) { return String(v); }
    }
    return String(v);
  }

  // addToolRow draws a tool call: the function name, then one "name - value"
  // line per argument, so the JSON wrapper is never shown. The line breaks are
  // part of the text (the row uses pre-wrap), so the layout does not depend on
  // any stylesheet rule. A payload that is not a JSON object falls back to raw
  // text.
  function addToolRow(name, args) {
    var row = document.createElement('div');
    row.className = 'row tool';
    var box = document.createElement('span');
    box.className = 'text';
    var fn = document.createElement('span');
    fn.className = 'fn';
    fn.textContent = name || '';
    box.appendChild(fn);
    // startArgLine opens the next argument line (a plain newline, no indent).
    function startArgLine() { box.appendChild(document.createTextNode('\n')); }
    var parsed = parseArgObject(args);
    if (parsed) {
      Object.keys(parsed).forEach(function (k) {
        var value = argText(parsed[k]);
        startArgLine();
        var key = document.createElement('span');
        key.className = 'arg-name';
        key.textContent = k;
        box.appendChild(key);
        var val = document.createElement('span');
        val.className = 'arg-val';
        if (value.indexOf('\n') === -1) {
          // Single-line value: keep it on the name's line.
          var sep = document.createElement('span');
          sep.className = 'arg-sep';
          sep.textContent = ' - ';
          val.textContent = value;
          box.appendChild(sep);
        } else {
          // Multi-line value: the name owns its line and the value starts on
          // the next one; neither name nor value is indented.
          val.textContent = value;
          box.appendChild(document.createTextNode('\n'));
        }
        box.appendChild(val);
      });
    } else if (typeof args === 'string' && args !== '') {
      startArgLine();
      var raw = document.createElement('span');
      raw.className = 'arg-raw';
      raw.textContent = args;
      box.appendChild(raw);
    }
    row.appendChild(box);
    // A tool row belongs to the running reply, so it goes above the pending
    // messages too.
    placeRow(row);
    return row;
  }

  // ---- command rail ----

  // setSwitch records a switch state and repaints its rail badge.
  function setSwitch(name, on) {
    switchOn[name] = on;
    var el = switchEls[name];
    if (!el) { return; }
    el.textContent = on ? 'on' : 'off';
    el.setAttribute('data-on', on ? '1' : '0');
    el.title = on ? 'currently on' : 'currently off';
  }

  // applySettings mirrors the switches the server reports: the injected page
  // config, a settings frame (another tab flipped one) or a history frame.
  function applySettings(s) {
    if (typeof s.markdown === 'boolean') {
      MARKDOWN = s.markdown;
      setSwitch('/markdown', s.markdown);
    }
    if (typeof s.result === 'boolean') { setSwitch('/result', s.result); }
  }

  // sendCommand submits one rail click on the shared connection.
  function sendCommand(name, args) {
    if (!ws || ws.readyState !== 1) { return; }
    ws.send(JSON.stringify({ text: args ? name + ' ' + args : name }));
  }

  // commandRow draws one command: its usage, the one-line summary and, for the
  // stateful switches, the current on/off state. The row itself is the button;
  // the state badge goes last so the summary can take a line of its own (see the
  // .cmd rules in app.css).
  function commandRow(cmd) {
    var li = document.createElement('li');
    var btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'cmd';
    btn.dataset.cmd = cmd.name;
    var name = document.createElement('code');
    name.textContent = cmd.name;
    btn.appendChild(name);
    if (cmd.args) {
      var args = document.createElement('span');
      args.className = 'cmd-args';
      args.textContent = ' ' + cmd.args;
      btn.appendChild(args);
    }
    if (switchKeys[cmd.name]) {
      var state = document.createElement('span');
      state.className = 'cmd-state';
      switchEls[cmd.name] = state;
      btn.appendChild(state);
    }
    var desc = document.createElement('span');
    desc.className = 'cmd-desc';
    desc.textContent = cmd.summary;
    btn.appendChild(desc);

    var hint = cmd.name + (cmd.args ? ' ' + cmd.args : '') + ' — ' + cmd.summary;
    if ((cmd.aliases || []).length > 0) { hint += ' (aliases: ' + cmd.aliases.join(', ') + ')'; }
    btn.title = hint;

    btn.onclick = function () {
      // A stateful switch sends the opposite of what the rail shows, so the
      // click always lands where the user aimed.
      if (switchKeys[cmd.name]) {
        sendCommand(cmd.name, switchOn[cmd.name] ? 'off' : 'on');
        return;
      }
      sendCommand(cmd.name, '');
    };
    li.appendChild(btn);
    return li;
  }

  // setCommandsOpen folds or unfolds the non-primary commands. The fold keeps the
  // rail short by default; the title carries its state for screen readers and the
  // count of what the fold hides.
  function setCommandsOpen(open) {
    var host = document.getElementById('cmds');
    var toggle = document.getElementById('cmdToggle');
    var more = document.getElementById('cmdMore');
    if (!host || !toggle) { return; }
    host.classList.toggle('open', open);
    toggle.setAttribute('aria-expanded', open ? 'true' : 'false');
    toggle.title = open ? 'fold the extra commands' : 'show all commands';
    if (more) { more.hidden = open; }
  }

  // buildCommands fills the rail from the server's command list, which is built
  // from the shared slash catalogue: the page and the CLI therefore describe the
  // same commands, and a command added there shows up here on the next reload.
  // Everything the catalogue does not mark primary starts folded.
  function buildCommands() {
    var host = document.getElementById('cmds');
    if (!host) { return; }
    var folded = 0;
    (CFG.commands || []).forEach(function (cmd) {
      var row = commandRow(cmd);
      if (!cmd.primary) { row.className = 'folded'; folded++; }
      host.appendChild(row);
    });
    var more = document.getElementById('cmdMore');
    if (more && folded > 0) { more.textContent = '+' + folded; }
    var toggle = document.getElementById('cmdToggle');
    if (toggle) { toggle.onclick = function () { setCommandsOpen(!host.classList.contains('open')); }; }
    setCommandsOpen(false);
    setSwitch('/result', !!CFG.result);
    setSwitch('/markdown', MARKDOWN);
  }

  var ws = null;
  // Configuration injected by the server into the page head.
  var CFG = window.__LIGHTAGENT__ || {};
  var MARKDOWN = !!CFG.markdown;
  // Sessions live in a cookie, so the WebSocket handshake authenticates itself.
  // The mirror only connects once the sign-in dialog says the browser is (or
  // need not be) signed in, and drops the socket when a session ends. The dialog
  // (auth.js) loads before this script and defines window.AUTH; the lookup
  // happens at call time, so a missing or failed auth.js degrades to "no login
  // required" instead of leaving the page unable to connect at all.
  var NO_AUTH = {
    ok: function () { return true; },
    ensure: function () { return Promise.resolve(true); },
    onChange: function () {},
    prompt: function () {},
    unauthorized: function () {}
  };
  function AUTH() { return window.AUTH || NO_AUTH; }
  // ensureSession asks the dialog for the session state, reporting "unknown" as
  // signed in when the dialog itself is broken (a synchronous throw inside its
  // check). The handshake is what the server enforces, so a page whose dialog
  // failed to load still connects — it shows offline and waits for a sign-in
  // instead of staying dead.
  function ensureSession() {
    try {
      return Promise.resolve(AUTH().ensure());
    } catch (err) {
      return Promise.resolve(true);
    }
  }
  // Read-aloud (tts.js): every live event is handed to the voice panel, so it can
  // read the same rows the transcript draws. A missing script degrades to a
  // no-op, and a replayed history frame is skipped (see render).
  var TTS = window.TTS || { event: function () {}, reset: function () {} };

  // The compressed-context summary is drawn as its own bordered block: it marks
  // the point where the older messages were cut out of the model context, so it
  // must stand out from the ordinary info rows. The CLI prints the same wording.
  var SUMMARY_ROLE = 'summary (older messages condensed)';

  function render(kind, ev) {
    // Read-aloud follows the same event stream as the rows below (a replayed
    // history frame is skipped: reloading the page must not read the whole
    // conversation aloud).
    if (!replaying) { TTS.event(kind, ev); }
    // Any event other than a further reasoning chunk ends the thinking row.
    if (kind !== 'reasoning_delta') { finishReasoning(); }
    if (kind === 'usage') { setUsage(ev.tokens || 0, ev.context_window || 0); return; }
    // A summary row is the truncation marker itself, so it is drawn as a whole
    // block instead of an [info] line; it arrives either live with a compacted
    // event or replayed from the history frame.
    if (kind === 'summary') { addRow('summary', SUMMARY_ROLE, ev.text || '', MARKDOWN); return; }
    if (kind === 'user') {
      setRunning(true);
      // A message this page sent while the turn was running is being sent now:
      // its pending row becomes an ordinary one, in place (it already sits after
      // everything the interrupted reply produced).
      if (settlePendingRow(ev.text || '')) {
        if (queued) { queued--; tickTurn(); }
        return;
      }
    }
    if (kind === 'turn_done' || kind === 'interrupted') { setRunning(false); }
    if (kind === 'reasoning_delta') {
      if (!reasoningRow) { reasoningRow = addRow('reasoning', 'thinking', '', MARKDOWN); }
      reasoningText += ev.text || '';
      scheduleReasoningRender();
      return;
    }
    if (kind === 'assistant_delta') {
      if (!current) { current = addRow('assistant', 'agent', '', false); }
      currentText += ev.text || '';
      if (MARKDOWN) { scheduleRender(); } else { setRow(current, currentText, false); }
      return;
    }
    var streamed = current;
    var streamedText = currentText;
    current = null;
    currentText = '';
    if (kind === 'turn_done') { return; }
    if (kind === 'user') { addRow('user', 'you', ev.text || '', false); }
    else if (kind === 'assistant') {
      var text = ev.text || streamedText;
      if (streamed) { setRow(streamed, text, true); }
      else { addRow('assistant', 'agent', text, true); }
    }
    else if (kind === 'tool_call') { addToolRow(ev.name, ev.args); }
    else if (kind === 'tool_result') {
      // Match the CLI: an empty result is only surfaced when it failed.
      if (!ev.text) {
        if (ev.is_error) { addRow('error', '', '[result] (error)', false); }
        return;
      }
      addRow(ev.is_error ? 'error' : 'result', '', '[result] ' + ev.text, false);
    }
    else if (kind === 'info') { addRow('result', '', '[info] ' + (ev.text || ''), false); syncResults(ev.text || ''); }
    else if (kind === 'compacted') {
      // The info row counts what was compressed away; the summary block shows
      // what replaced it. The mirror records the same block at this point of its
      // scrollback, so a reload replays it exactly here.
      addRow('result', '', '[info] ' + (ev.text || ''), false);
      if (ev.summary) { render('summary', { text: ev.summary }); }
    }
    else if (kind === 'interrupted') { addRow('interrupted', '', '[interrupted] ' + (ev.text || ''), false); }
    else if (kind === 'error') { addRow('error', '', '[error] ' + (ev.text || ''), false); }
  }

  // The snapshot's header carries whether a turn is already running; the
  // indicator is applied when the replay ends (setRunning ignores calls while
  // replaying, so a replayed user row cannot light it up early).
  var historyBusy = false;

  // flushReplayBatch moves the rows collected so far into the log: one insert,
  // one reflow, and no layout read per row (the view is pinned once, when the
  // snapshot ends).
  function flushReplayBatch() {
    if (!replayBatch) { return; }
    if (replayBatch.childNodes.length > 0) { log.appendChild(replayBatch); }
    replayBatch = document.createDocumentFragment();
  }

  // beginHistory replaces the log with an empty one and starts a replay. A
  // snapshot always describes the full conversation, so it replaces the log
  // instead of appending (otherwise reconnects duplicate every message).
  function beginHistory(ev) {
    log.innerHTML = '';
    current = null;
    currentText = '';
    if (pendingRender) { clearTimeout(pendingRender); pendingRender = null; }
    // The rebuild replaces every row, so the open thinking row of a stream that
    // is gone with it is dropped too (the next chunk draws a fresh one). Pending
    // messages are gone as well: the agent draws their rows when it sends them.
    reasoningRow = null;
    reasoningText = '';
    if (pendingReasoningRender) { clearTimeout(pendingReasoningRender); pendingReasoningRender = null; }
    pendingRows = [];
    // The rows the voice was reading are gone: drop its buffers and silence it.
    TTS.reset();
    // Nothing queued points at a row in the document any more.
    mdPending.clear();
    replaying = true;
    log.classList.add('replaying');
    replayBatch = document.createDocumentFragment();
    historyBusy = !!ev.busy;
    setUsage(ev.tokens || 0, ev.window || 0);
    applySettings(ev);
  }

  // appendHistoryRows draws one batch of the snapshot and inserts it. Batches
  // keep the browser responsive: each frame the page receives is small, and the
  // rows reach the document in one insert.
  function appendHistoryRows(ev) {
    if (!replayBatch) { beginHistory(ev); }
    (ev.messages || []).forEach(function (m) {
      if (m.role === 'user') { render('user', { text: m.content }); }
      else if (m.role === 'assistant' && m.content) { render('assistant', { text: m.content }); }
      else if (m.role === 'reasoning') { render('reasoning_delta', { text: m.content }); }
      else if (m.role === 'tool_call') { render('tool_call', { name: m.name, args: m.args }); }
      else if (m.role === 'tool_result') { render('tool_result', { text: m.content, is_error: m.is_error }); }
      else if (m.role === 'info') { render('info', { text: m.content }); }
      else if (m.role === 'summary') { render('summary', { text: m.content }); }
      else if (m.role === 'error') { render('error', { text: m.content }); }
      else if (m.role === 'interrupted') { render('interrupted', { text: m.content }); }
    });
    flushReplayBatch();
  }

  // endHistory closes the replay: the last batch is inserted, the view is pinned
  // to the bottom (the way a refreshed page sits) and the running indicator picks
  // up whatever the header reported.
  function endHistory() {
    if (!replaying) { return; }
    flushReplayBatch();
    replayBatch = null;
    replaying = false;
    log.classList.remove('replaying');
    // The compressed-context summary is not appended here: it is one of the rows
    // replayed above, recorded exactly where the context was cut, so a reload
    // rebuilds the truncation marker in the right place.
    setRunning(historyBusy);
    pinBottom();
  }

  // dropReplayState discards an unfinished replay: a socket that closed mid
  // snapshot leaves a half-built log behind, and the next connection replaces it
  // with a fresh snapshot anyway.
  function dropReplayState() {
    replayBatch = null;
    replaying = false;
    log.classList.remove('replaying');
  }

  function wsURL() {
    var proto = location.protocol === 'https:' ? 'wss://' : 'ws://';
    // The session cookie rides on the handshake, so there is no token to add.
    return proto + location.host + '/ws';
  }

  var reconnectTimer = null;

  function connect() {
    if (!AUTH().ok()) { return; }
    if (ws && (ws.readyState === 0 || ws.readyState === 1)) { return; }
    ws = new WebSocket(wsURL());
    ws.onopen = function () {
      dot.classList.add('on');
      dot.title = 'connected';
      statusEl.textContent = 'online';
      sendEl.disabled = false;
    };
    ws.onclose = function () {
      dot.classList.remove('on');
      dot.title = 'disconnected';
      statusEl.textContent = 'offline';
      sendEl.disabled = true;
      // A socket that dropped in the middle of a snapshot leaves a half-built
      // log: drop the replay state before clearing the indicator, so the
      // indicator is not swallowed by the replay guard.
      dropReplayState();
      setRunning(false);
      if (!AUTH().ok()) { return; } // signed out: wait for a new session
      if (reconnectTimer) { return; }
      reconnectTimer = setTimeout(function () {
        reconnectTimer = null;
        // A session can expire between reconnects, so check before dialing.
        ensureSession().then(function (ok) {
          if (ok) { connect(); } else { AUTH().prompt('Your session ended. Sign in again.'); }
        }).catch(function () { connect(); });
      }, 1500);
    };
    ws.onerror = function () { try { ws.close(); } catch (e) {} };
    ws.onmessage = function (e) {
      var ev;
      try { ev = JSON.parse(e.data); } catch (err) { return; }
      // The conversation snapshot arrives in three parts (see beginHistory):
      // header, row batches, terminator, so a long conversation paints while it
      // is still arriving instead of blocking on one giant frame.
      if (ev.type === 'history_start') { beginHistory(ev); return; }
      if (ev.type === 'history_rows') { appendHistoryRows(ev); return; }
      if (ev.type === 'history_end') { endHistory(); return; }
      // The switches the rail mirrors: no row, just state.
      if (ev.type === 'settings') { applySettings(ev); return; }
      render(ev.type, ev);
    };
  }

  function send() {
    var text = input.value.trim();
    if (!text || !ws || ws.readyState !== 1) { return; }
    input.value = '';
    autoGrow();
    // Sending is an explicit "show me what comes next" action: re-pin the view
    // (also makes the pending user row count as at-the-bottom) even when the
    // reader had scrolled back through history.
    pinBottom();
    // A message sent while a turn is running joins it (steering): its row is drawn
    // right away, marked pending, and the running reply keeps streaming above it
    // until the agent sends it (addPendingRow / settlePendingRow).
    if (running) {
      addPendingRow(text);
      queued++;
      tickTurn();
    }
    ws.send(JSON.stringify({ text: text }));
  }
  // autoGrow keeps the composer one row tall until the message wraps, then it
  // grows up to the CSS max-height and scrolls.
  function autoGrow() {
    input.style.height = 'auto';
    input.style.height = input.scrollHeight + 'px';
  }
  sendEl.onclick = send;
  stopEl.onclick = function () {
    if (ws && ws.readyState === 1) { ws.send(JSON.stringify({ text: '/stop' })); }
  };
  input.addEventListener('input', autoGrow);
  input.addEventListener('keydown', function (e) {
    // Enter inserts a newline (default textarea behavior); Ctrl+Enter sends.
    if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) { e.preventDefault(); send(); }
  });

  // The rail only mirrors the shared catalogue, so it is built once, before the
  // connection is dialed.
  buildCommands();
  sendEl.disabled = true;
  // Wait for the session state before dialing: the handshake fails without a
  // session, and the dialog may sign this browser in on its own with a stored
  // digest.
  ensureSession().then(function (ok) {
    if (ok) { connect(); }
  }).catch(function () { connect(); });
  // Signing in (or out) elsewhere on the page starts or stops the mirror.
  AUTH().onChange(function (ok) {
    if (ok) { connect(); return; }
    if (ws) { try { ws.close(); } catch (err) { /* already closed */ } }
    ws = null;
  });
})();

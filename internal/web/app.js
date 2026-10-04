(function () {
  var log = document.getElementById('log');
  var input = document.getElementById('input');
  var dot = document.getElementById('dot');
  var statusEl = document.getElementById('status');
  var usageEl = document.getElementById('usage');
  var runEl = document.getElementById('run');
  var stopEl = document.getElementById('stop');
  var sendEl = document.getElementById('send');
  var attachEl = document.getElementById('attach');
  var attachFileEl = document.getElementById('attachFile');
  var attachmentsEl = document.getElementById('attachments');
  var copyPop = document.getElementById('copyPop');
  var copyNote = document.getElementById('copyNote');
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
  // swapInAtEnd is true while a snapshot is rebuilding a transcript that is still
  // on screen: its rows are held back in the batch fragment and replace the log in
  // one insert when the snapshot is complete (see beginHistory/endHistory). A
  // rebuild that starts from an empty log (the first load) has nothing to keep on
  // screen, so its batches reach the log as they arrive.
  var swapInAtEnd = false;
  // The command rail's switches (/result, /markdown) are the only commands with
  // state: the rail shows it and sends the explicit opposite, so one click
  // always lands on the state the user asked for. The values arrive with the
  // injected page config, with a settings frame (another tab flipped one) or in
  // a history frame (after a reconnect).
  var switchKeys = { '/result': 'result', '/markdown': 'markdown' };
  var switchOn = { '/result': true, '/markdown': true };
  var switchEls = {};

  // ---- device and page-state probes ----
  // Both matchMedia queries are built once and shared by the layout wiring and
  // the pacing below. A "phone" is the stylesheet's phone breakpoint (see the
  // 480px media query in app.css) or a coarse pointer: a narrow window on a
  // desktop counts too, and either way the answer means "redraw less".
  var phoneQuery = window.matchMedia ? window.matchMedia('(max-width: 480px)') : null;
  var coarseQuery = window.matchMedia ? window.matchMedia('(pointer: coarse)') : null;

  function onPhone() {
    return !!((phoneQuery && phoneQuery.matches) || (coarseQuery && coarseQuery.matches));
  }

  // visible reports whether the page is actually being looked at. A page nobody
  // is looking at stops its clocks, its socket and its rendering work (see
  // goIdle): the agent runs on the server, so nothing is lost by not watching
  // it, and a fresh snapshot restores the transcript on the way back.
  function visible() { return !document.hidden; }

  // The transcript follows new output while the reader is following it. The
  // reader's own gestures — a wheel turning up, a finger dragging the content
  // down, a page key — stop that at once, and the view then stays where it was
  // put while the output keeps growing below. Reaching the end of the transcript
  // again (by hand, by sending a message, or with the "latest" button) resumes
  // following. The slack absorbs sub-pixel rounding, so "at the bottom" does not
  // need an exact scrollHeight match.
  //
  // Geometry alone cannot express this: while text streams in, the bottom moves
  // away every frame, so a reader who is dragging upward is back inside "at the
  // bottom" the moment a pin lands there — one such pin is enough to cancel the
  // gesture and yank the reader down again, which is what made a scroll-back so
  // hard to hold. Following is therefore a state of its own (see the gesture
  // probes below), not a distance.
  //
  // Every part of the page that adds or grows a row goes through the same two
  // steps: change the DOM, then keepBottom(). The write comes last — it must see
  // the grown content to land on the new bottom — and it is the reader's own
  // position that decides whether it happens now or in the frame (see
  // keepBottom), so a row that arrives while the reader is up in the history is
  // not followed.
  var STICK_SLACK = 8;

  function atBottom() {
    return log.scrollHeight - log.scrollTop - log.clientHeight <= STICK_SLACK;
  }

  // Whether new output pulls the view down is the reader's intent (following), and
  // only that: while text streams in the bottom moves away every frame, so a view
  // that is a few pixels off it still belongs to a reader who is following.
  // Geometry only says where that reader is sitting right now — which is what
  // separates a change the page writes back in the same task (see keepBottom) from
  // one it leaves to the animation frame, where a gesture still wins (see
  // pinBottom). A page opens at the newest row, so it starts as yes, and only the
  // reader's own gestures turn it off.
  var following = true;

  // unpinnedAt is when following was last switched off: the scroll event of that
  // very move (a gesture, the "previous message" button) arrives right after it,
  // and a move that ends inside the STICK_SLACK band must not read as "heading
  // back down" and turn following on again — see the band rule in onLogScroll,
  // which holds that reading back for REPIN_GRACE_MS.
  var unpinnedAt = 0;
  var REPIN_GRACE_MS = 400;

  // pinned reports the view sitting at the exact end of the transcript. Reader
  // gestures land there (the browser clamps a scroll at the end) and so does the
  // page's own pin, so this is the geometry that means "follow again". A gesture
  // that only moved the view inside the STICK_SLACK band deliberately does not
  // count: that small step up is exactly how a scroll-back starts.
  function pinned() {
    return log.scrollHeight - log.scrollTop - log.clientHeight <= 1;
  }

  // setFollowing records that answer and keeps the transcript's jump buttons in
  // step: the "latest" one is exactly the way back from a view that stopped
  // following, so it is faded out while the view follows (see updateJump).
  function setFollowing(on) {
    on = !!on;
    if (on === following) { return; }
    following = on;
    if (!on) { unpinnedAt = Date.now(); }
    updateJump();
  }

  // pinBottom shows the newest row regardless of where the reader was; on an
  // empty (freshly rebuilt) log it also establishes "at the bottom". The write
  // is coalesced into one animation frame: a streamed row can be re-drawn and
  // several rows appended between two frames, and every scroll write forces a
  // layout, so the frame ends with a single write to the true bottom. It is what
  // handles a reader who is following from a distance — one whose view the page
  // itself last wrote gets the immediate write instead (see keepBottom). The
  // reader can still take the view back in the frame between that decision and
  // this write: the browser fires a gesture's scroll event before the animation
  // frame callbacks of the same frame, so `following` is already off by the time a
  // pin queued for that frame writes — the gesture wins over a pin on its way.
  var pinQueued = false;

  // pageScrollTop is the position the page itself last wrote. It is how a change
  // tells a view that is still where the page put it (that reader is at the
  // bottom, following) from one the reader has just moved — before that move's
  // scroll event has been dispatched.
  var pageScrollTop = -1;

  // contentOffset is how far the content's bottom (the last row's margin and the
  // box's own bottom padding included) sits below the box's bottom: 0 means exactly
  // at the bottom. Both numbers are read live — the padding follows the phone
  // breakpoint and a row's margin is what the stylesheet says, not a constant.
  function contentOffset() {
    var last = log.lastElementChild;
    if (!last) { return 0; }
    var paddingBottom = parseFloat(window.getComputedStyle(log).paddingBottom) || 0;
    var marginBottom = parseFloat(window.getComputedStyle(last).marginBottom) || 0;
    return (last.getBoundingClientRect().bottom + marginBottom + paddingBottom)
      - log.getBoundingClientRect().bottom;
  }

  function writeBottom() {
    // Nothing to scroll (the transcript is shorter than its box): the bottom is
    // the top.
    if (log.scrollHeight <= log.clientHeight) {
      if (log.scrollTop !== 0) { log.scrollTop = 0; }
      pageScrollTop = log.scrollTop;
      return;
    }
    // Correct by measurement rather than by arithmetic: scrollHeight and
    // clientHeight are rounded integers while the content's real bottom is what the
    // last row's box says, and the browser quantizes the scroll position it stores
    // as well. The transcript's line heights are whole pixels for exactly this
    // reason (see #log in app.css): a fractional content height left a fraction of
    // a pixel behind on every chunk, and the pending row trembled by that fraction
    // (1-3 device pixels on a phone). Two passes land the content's bottom exactly
    // on the box's bottom.
    for (var pass = 0; pass < 2; pass++) {
      var off = contentOffset();
      if (off === 0) { break; }
      log.scrollTop = log.scrollTop + off;
    }
    // Read the value back: the browser quantizes what it stores, and that value is
    // what the next comparison must use.
    pageScrollTop = log.scrollTop;
  }

  function pinBottom() {
    if (pinQueued || !following) { return; }
    pinQueued = true;
    var write = function () {
      pinQueued = false;
      if (!following) { return; }
      writeBottom();
    };
    if (typeof window.requestAnimationFrame === 'function') {
      window.requestAnimationFrame(write);
    } else {
      write();
    }
  }

  // ---- a change the reader is sitting on ----
  // A row that grows moves everything below it down, and the pending row of a
  // steering message is always the last one: writing the bottom only in the
  // animation frame (see pinBottom) paints one frame with that row pushed down
  // and the next one with it snapped back — the shake a streaming answer used to
  // give the pending bubble, which a phone's coarser redraw cadence turned into a
  // steady beat (measured: the bottom fell 22-80px behind on every chunk). So the
  // write happens here, in the same task as the change, for a reader the page is
  // already carrying — and for that test it is not enough to ask whether the view
  // sits exactly at the end (keyboard, a resize, a rounding can leave a few pixels
  // behind, and every chunk would then wait for the frame again, shaking as
  // before). What matters is whether the position is still the page's own: a
  // reader who moved it keeps the frame-write, where a gesture wins (their scroll
  // event may not have been dispatched yet).
  function keepBottom() {
    if (!following) { return; }
    if (pageScrollTop >= 0 && Math.abs(log.scrollTop - pageScrollTop) > 1) { pinBottom(); return; }
    writeBottom();
  }

  // ---- reader intent: what stops and what resumes the follow ----
  // The probes below are what lets a scroll-back hold: they run before the browser
  // applies the gesture, so an up-gesture wins over a pin that is already queued
  // for this frame. They are all passive — none of them changes what the gesture
  // itself does.
  function unpinView() {
    // Nothing above to scroll to (a transcript shorter than its box): an
    // up-gesture means nothing there, and unpinning would leave the reader
    // following nothing.
    if (log.scrollHeight - log.clientHeight > 1) { setFollowing(false); }
  }

  // A wheel turning up (deltaY < 0) is the desktop reader asking for history.
  log.addEventListener('wheel', function (e) {
    if (e.deltaY < 0) { unpinView(); }
  }, { passive: true });

  // On a touch screen the finger moves down the screen while the content moves
  // up: the same request. Momentum needs nothing of its own — the scroll events
  // it produces keep the view unpinned (see onLogScroll). A gesture that starts
  // with two fingers (a pinch) is not a scroll and is left alone.
  var touchY = 0;
  var touchDrag = false;

  log.addEventListener('touchstart', function (e) {
    touchDrag = e.touches.length === 1;
    touchY = touchDrag ? e.touches[0].clientY : 0;
  }, { passive: true });
  log.addEventListener('touchmove', function (e) {
    if (!touchDrag || e.touches.length !== 1) { return; }
    var y = e.touches[0].clientY;
    if (y - touchY > 1) { unpinView(); }
    touchY = y;
  }, { passive: true });

  // Keyboard scrolling: PageUp/Home/ArrowUp (and Shift+Space) when the log holds
  // the focus or nothing in particular does. A key typed into the composer — or
  // into a form field of the config editor — is that field's own business.
  function isFormField(el) {
    if (!el || !el.tagName) { return false; }
    var tag = el.tagName;
    return tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || !!el.isContentEditable;
  }

  document.addEventListener('keydown', function (e) {
    if (isFormField(e.target)) { return; }
    if (e.key === 'ArrowUp' || e.key === 'PageUp' || e.key === 'Home' || (e.key === ' ' && e.shiftKey)) {
      unpinView();
    }
  });

  // The scroll event is where the reader's own scrollbar drag and the page's own
  // pin both land (the pin always lands on the exact end), so it is the one place
  // that decides to follow again: reaching the very end means "keep the newest row
  // in view", and a scroll that moved the view up means the reader is reading
  // history — whichever gesture produced it, a scrollbar drag having no wheel or
  // touch event of its own.
  var lastScrollTop = 0;

  function onLogScroll() {
    var top = log.scrollTop;
    var moved = top - lastScrollTop;
    lastScrollTop = top;
    if (pinned()) {
      // The reader reached the very end: the page carries the view again, so the
      // next change may write it in its own task (see keepBottom).
      pageScrollTop = top;
      setFollowing(true);
    }
    else if (moved < 0) { setFollowing(false); }
    // Came back into the bottom band on the way down: the reader is heading for
    // the bottom, and the next output should pull them the rest of the way. Not
    // right after an unpin though — that move is the one being read.
    else if (moved > 0 && atBottom() && Date.now() - unpinnedAt > REPIN_GRACE_MS) { setFollowing(true); }
  }

  log.addEventListener('scroll', onLogScroll, { passive: true });

  // A resize moves the bottom: the composer wraps, and a phone's URL bar or soft
  // keyboard changes the viewport height while the answer streams. A following view
  // is put back on the newest row in this same task — waiting for the animation
  // frame (pinBottom) paints the frame the viewport changed in with the bottom far
  // off screen (measured: the pending row swung by 112px with 39 direction changes
  // while a URL-bar animation ran), which is the trembling a phone showed and a
  // desktop, whose window never changes size, never did. The position is written
  // whatever it was: a viewport that grew makes the browser clamp it, and the
  // "the page wrote this position" test would send that write back to the frame.
  window.addEventListener('resize', function () {
    if (following) { writeBottom(); }
  });
  // The same viewport, seen the other way: on a phone that resizes its visual
  // viewport (a keyboard that shrinks the page rather than the layout viewport, a
  // pinch) the layout box can stay put while what the reader sees moves. Writing
  // the bottom is idempotent, so listening to both costs nothing.
  if (window.visualViewport) {
    window.visualViewport.addEventListener('resize', function () {
      if (following) { writeBottom(); }
    });
  }

  // ---- the transcript's jump buttons ----
  // Two floating buttons: up walks back through the messages the reader wrote —
  // the click also stops the follow, because reading history and following the
  // newest output are opposites — and down goes to the newest row and follows
  // again. The "latest" one is faded out exactly while the view follows, so it
  // only ever offers the way back when it is needed (see updateJump).
  var jumpPrevEl = document.getElementById('jumpPrev');
  var jumpLatestEl = document.getElementById('jumpLatest');
  // JUMP_PAD is the gap a jump leaves above the message it lands on, JUMP_EDGE
  // the line the search looks at (a message "passed" by the reader starts above
  // it); the two differ on purpose, so a row the previous click aligned is not
  // offered twice.
  var JUMP_PAD = 10;
  var JUMP_EDGE = 14;
  // lastJumpRow is the message the last click landed on. The search skips it, so
  // clicking again walks one message further up whatever rounding did to where
  // that row ended up.
  var lastJumpRow = null;

  // updateJump keeps the buttons in step with the follow state. "Latest" is
  // faded out (.off) rather than removed: it keeps its place in the stack, so
  // only the opacity changes when the reader scrolls away and back.
  function updateJump() {
    if (jumpLatestEl) { jumpLatestEl.classList.toggle('off', following); }
  }

  // prevUserRow is the newest message the reader wrote that starts above the top
  // edge of the transcript: the one a click on "Previous" goes to. A message the
  // page is still waiting to send (a pending row) counts like any other — it is
  // a message the reader wrote.
  function prevUserRow() {
    var edge = log.getBoundingClientRect().top + JUMP_EDGE;
    var found = null;
    for (var i = 0; i < log.children.length; i++) {
      var row = log.children[i];
      if (!row.classList.contains('user')) { continue; }
      if (row.getBoundingClientRect().top >= edge) { break; }
      // The row the previous click landed on is not the next target: a click on
      // top of that one means "one message further up".
      if (row !== lastJumpRow) { found = row; }
    }
    return found;
  }

  // scrollRowToTop puts one row at the top edge of the transcript box, leaving
  // JUMP_PAD above it so its role line is not flush against the header.
  function scrollRowToTop(row) {
    var box = log.getBoundingClientRect();
    var top = log.scrollTop + (row.getBoundingClientRect().top - box.top) - JUMP_PAD;
    log.scrollTop = top > 0 ? top : 0;
  }

  // jumpToPrevMessage goes to that message and stops following, so the output
  // that keeps arriving below cannot pull the view away from what is being read.
  // With nothing above to go to it does nothing at all and leaves the view alone.
  function jumpToPrevMessage() {
    var row = prevUserRow();
    if (!row) { return; }
    setFollowing(false);
    scrollRowToTop(row);
    lastJumpRow = row;
  }

  // jumpToLatest pins the view to the newest row and follows again.
  function jumpToLatest() {
    setFollowing(true);
    pinBottom();
  }

  if (jumpPrevEl) { jumpPrevEl.onclick = jumpToPrevMessage; }
  if (jumpLatestEl) { jumpLatestEl.onclick = jumpToLatest; }

  // ---- folding a row to its first line ----
  // The model's thinking, its tool calls and their results are the long, repetitive
  // part of a transcript. Each of those rows folds to its first line — the
  // "thinking" label, the tool's name, one line of a result — and the reader folds
  // or unfolds one of them by clicking that line. The messages themselves are never
  // folded: what the user wrote, what the agent answered and the compressed-context
  // summary are the transcript's content.
  //
  // The +/- switch between the jump buttons folds or unfolds every such row at once,
  // and its state is what a new row is born with: a thinking row that arrives while
  // the transcript is folded stays folded until the reader opens it. Each row keeps
  // that state itself, so the switch is a one-off action — a row the reader opens by
  // hand stays open whatever the rest of the transcript does. Nothing is stored: a
  // reload or a reconnect rebuilds the rows from the switch's state.
  var foldAll = false;

  // foldRow folds or unfolds one row. A row that cannot be folded is left alone, so
  // the switch can walk the whole transcript.
  function foldRow(row, on) {
    if (!row.classList || !row.classList.contains('foldable')) { return; }
    row.classList.toggle('folded', !!on);
  }

  // markFoldable makes a row foldable, and folds it when that is the current state.
  function markFoldable(row) {
    row.classList.add('foldable');
    if (foldAll) { foldRow(row, true); }
  }

  // eachFoldableRow visits every row on screen, the batch of a snapshot being
  // replayed included: its rows are already built, so a fold has to reach them too.
  function eachFoldableRow(visit) {
    var lists = replayBatch ? [log.children, replayBatch.children] : [log.children];
    for (var l = 0; l < lists.length; l++) {
      for (var i = 0; i < lists[l].length; i++) { visit(lists[l][i]); }
    }
  }

  // firstLineHit reports whether a click landed on the line a row keeps when it is
  // folded: the label of a thinking row, the name line of a tool call, or the first
  // line of a result. A folded row is nothing but that line, so any click in it
  // counts; on an unfolded one the click must be inside the first line's own band —
  // measured from the text's top edge with its padding counted off, since a result's
  // text sits in a padded block (see the folded rules in app.css). A click above the
  // text (the label line) is inside the band too, which is what makes the label the
  // handle of a thinking row.
  function firstLineHit(row, e) {
    if (row.classList.contains('folded')) { return true; }
    var span = rowText(row);
    if (!span) { return false; }
    var box = span.getBoundingClientRect();
    var pad = parseFloat(window.getComputedStyle(span).paddingTop) || 0;
    var line = parseFloat(window.getComputedStyle(row).lineHeight) || 20;
    return (e.clientY - box.top - pad) < line;
  }

  // A click on that line folds or unfolds the row under it. The switch never locks
  // the rows, so this stays available however the transcript is folded. The copy
  // icon rides in the first line of the rows that carry one and stops its own
  // clicks (see copyControl), so it is never read as a fold.
  log.addEventListener('click', function (e) {
    if (e.target && e.target.closest && e.target.closest('.row-actions')) { return; }
    var row = copyRowOf(e.target);
    if (!row || !row.classList.contains('foldable')) { return; }
    if (!firstLineHit(row, e)) { return; }
    var release = holdView();
    foldRow(row, !row.classList.contains('folded'));
    if (release) { release(); }
    keepBottom();
  });

  // The switch's glyph and word say what a click does, the way "Latest" is only
  // offered while it has somewhere to go: a bar alone ("−") folds the transcript to
  // its first lines, a crossed bar ("+") opens them all again.
  var foldEl = document.getElementById('foldToggle');
  var foldLabelEl = document.getElementById('foldLabel');
  var FOLD_WHAT = 'every thinking, tool call and tool result';

  function updateFoldSwitch() {
    if (!foldEl) { return; }
    foldEl.classList.toggle('folded', foldAll);
    if (foldLabelEl) { foldLabelEl.textContent = foldAll ? 'Expand' : 'Fold'; }
    foldEl.title = foldAll ? 'expand ' + FOLD_WHAT + ' again' : 'fold ' + FOLD_WHAT + ' to its first line';
    foldEl.setAttribute('aria-label', foldEl.title);
  }

  // toggleAllRows is the switch: it takes every foldable row on screen with it,
  // however the reader left them, and leaves the rest of the transcript alone.
  function toggleAllRows() {
    foldAll = !foldAll;
    // Folding changes the height of the rows above the view — the same kind of change
    // a markdown pass makes, so a reader who scrolled back keeps their row (see
    // holdView) and a following one is put back on the newest row (see keepBottom).
    var release = holdView();
    eachFoldableRow(function (row) { foldRow(row, foldAll); });
    if (release) { release(); }
    updateFoldSwitch();
    keepBottom();
  }

  if (foldEl) { foldEl.onclick = toggleAllRows; }
  updateFoldSwitch();

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

  // queuedText is the queued part of the running pill: the count and, where there
  // is room for it, the word. A phone-width banner keeps the number alone — its
  // badges are pills of nowrap text and this one is the widest of them, so the
  // word is what makes the header wider than the screen the moment a message is
  // pending (a wider header drags the transcript's right edge with it, see .app in
  // app.css). The whole phrase stays in the pill's title, on every layout (see
  // tickTurn), the way the context badge keeps the words a phone drops.
  function queuedText() {
    if (queued <= 0) { return ''; }
    if (phoneQuery && phoneQuery.matches) { return ' · ' + queued; }
    return ' · ' + queued + ' queued';
  }

  function elapsedText(ms) {
    var s = ms / 1000;
    if (s < 60) { return s.toFixed(1) + 's'; }
    if (s < 3600) { return Math.floor(s / 60) + 'm' + pad2(Math.floor(s) % 60) + 's'; }
    return Math.floor(s / 3600) + 'h' + pad2(Math.floor(s / 60) % 60) + 'm';
  }

  function tickTurn() {
    if (elapsedEl) { elapsedEl.textContent = elapsedText(Date.now() - turnStart) + queuedText(); }
    // The pill's title spells the queued count out whatever the layout shows (the
    // CLI prints the same sentence when it has to queue a message).
    runEl.title = queued > 0 ? queued + ' queued; it joins the conversation after the current reply' : '';
  }

  // The clock's cadence: the terminal redraws its spinner every 100ms and a
  // visible desktop mirrors that. A phone only needs whole seconds (tenths of a
  // second are not worth a redraw on a small screen, nor a wake-up in a pocket),
  // and a page in the background ticks once a second for as long as it keeps
  // working (see the power section below).
  var TURN_TICK_MS = 100;
  var TURN_TICK_MS_PHONE = 1000;
  var TURN_TICK_MS_HIDDEN = 1000;

  // A steering message joins the running turn, so an already-ticking clock is
  // left alone (the CLI does the same). The tick mirrors its 100ms animation, or
  // a coarser one on a phone or in the background.
  function startTurnTimer() {
    if (turnTimer) { return; }
    turnStart = Date.now();
    tickTurn();
    turnTimer = setInterval(tickTurn, turnTickMs());
  }

  // turnTickMs is the clock's cadence: the terminal's 100ms on a visible desktop,
  // whole seconds on a phone and while the page is in the background.
  function turnTickMs() {
    if (!visible()) { return TURN_TICK_MS_HIDDEN; }
    return onPhone() ? TURN_TICK_MS_PHONE : TURN_TICK_MS;
  }

  // restartTurnTimer re-arms a running clock at the cadence the page's new state
  // asks for; it does nothing when no turn is running.
  function restartTurnTimer() {
    if (!turnTimer) { return; }
    clearInterval(turnTimer);
    turnTimer = setInterval(tickTurn, turnTickMs());
  }

  function stopTurnTimer() {
    if (turnTimer) { clearInterval(turnTimer); turnTimer = null; }
    turnStart = 0;
    if (elapsedEl) { elapsedEl.textContent = ''; }
    runEl.title = '';
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
  // window fills up). The badge is split into the "ctx" word, the percentage and
  // the token counts so each layout keeps what fits: a phone shows the
  // percentage alone (the other two nodes are hidden, see app.css) while a wide
  // header shows the full tag, and the title always carries the full numbers.
  // The badge stays hidden (.on) until a report arrives, and every target is
  // optional so the page keeps working without it.
  function setUsage(tokens, win) {
    var fill = document.getElementById('meterFill');
    var pctEl = document.getElementById('meterPct');
    var noteEl = document.getElementById('meterNote');
    var pctSpan = document.getElementById('usagePct');
    var tokSpan = document.getElementById('usageTokens');
    if (!win) {
      usageEl.className = 'usage';
      usageEl.title = '';
      if (pctSpan) { pctSpan.textContent = ''; }
      if (tokSpan) { tokSpan.textContent = ''; }
      if (fill) { fill.style.width = '0%'; fill.className = ''; }
      if (pctEl) { pctEl.textContent = '—'; }
      if (noteEl) { noteEl.textContent = 'waiting for usage…'; }
      return;
    }
    var pct = win > 0 ? (tokens * 100 / win) : 0;
    // The full tag mirrors the CLI prompt's [ctx 12.3%] label; the word sits in
    // its own span so a phone can drop it.
    var value = pct.toFixed(1) + '%';
    var counts = ' (' + tokens + '/' + win + ' tokens)';
    if (pctSpan) { pctSpan.textContent = value; }
    if (tokSpan) { tokSpan.textContent = counts; }
    if (!pctSpan) { usageEl.textContent = 'ctx ' + value + counts; }
    usageEl.title = 'context ' + pct.toFixed(1) + '% (' + tokens + '/' + win + ' tokens)';
    usageEl.className = 'usage on' + (pct >= 90 ? ' hot' : (pct >= 70 ? ' warm' : ''));
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
    if (!MARKDOWN) { return null; }
    return markdownToHTML(text);
  }

  // markdownToHTML is the one markdown pipeline: the formulas are lifted out of
  // the text first (marked would read _x_y_ as emphasis and eat the backslash of
  // \%, \{ and \;), the rest goes through marked, the MathML is put back where
  // the placeholders are and the whole result is sanitized. math.js owns both
  // ends of that (protect/inject).
  function markdownToHTML(text) {
    if (typeof marked === 'undefined') { return null; }
    try {
      var lifted = (typeof MathTex !== 'undefined') ? MathTex.protect(text) : null;
      var html = marked.parse(lifted ? lifted.text : text, { gfm: true, breaks: true });
      if (lifted) { html = MathTex.inject(html, lifted.items); }
      if (typeof DOMPurify !== 'undefined') {
        // DOMPurify's MathML attribute list spells columnalign "columnsalign", so
        // the alignment a formula table asks for is added back explicitly.
        html = DOMPurify.sanitize(html, { ADD_ATTR: ['columnalign'] });
      }
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
  // (a turn that is streaming) still render immediately; a page coming back to the
  // foreground drains what queued up while nobody was looking in one batch (see
  // flushMarkdownNow), and no pass moves a reader who scrolled back (see holdView).
  var mdPending = new Map(); // span -> latest text waiting for its markdown pass
  var mdFlushScheduled = false;
  var MD_SLICE_MS = 8;

  // rowSource remembers the text a row was drawn from, keyed by that row's text
  // span: the copy control hands a row back exactly as it was written, whatever
  // the /markdown switch is rendering on screen. Every update goes through
  // setSpan — the live stream and a replayed snapshot alike — so a row restored
  // by a reconnect copies like a fresh one.
  var rowSource = new WeakMap();

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

  // topVisibleRow is the row the reader's view starts at: the first one whose
  // bottom edge is below the transcript's own top edge, and so the one a row that
  // grows above it pushes down the screen. The rows are stacked blocks, so their
  // bottoms only ever increase — the cost of finding that row in a log of
  // thousands is a handful of measurements rather than one per row.
  function topVisibleRow() {
    var top = log.getBoundingClientRect().top;
    var lo = 0, hi = log.children.length - 1, found = null;
    while (lo <= hi) {
      var mid = (lo + hi) >> 1;
      if (log.children[mid].getBoundingClientRect().bottom > top) {
        found = log.children[mid];
        hi = mid - 1;
      } else {
        lo = mid + 1;
      }
    }
    return found;
  }

  // holdView keeps the row the reader is reading exactly where it is across one
  // batch of markdown upgrades, and returns the function that writes it back.
  //
  // An upgraded row grows — markdown brings paragraph margins, tables, code blocks
  // and headings where a replayed row was one plain block — and the transcript has
  // the browser's own scroll anchoring switched off (see #log in app.css: it pulls
  // against the page's bottom writes). A pass that lands on rows above the reader
  // therefore drags that reader along: the rows being read walk down the screen and
  // older ones come in at the top, pass after pass, for as long as the queue lasts,
  // and pulling back up only adds more of the same. That is the page that looked
  // like it scrolled away on its own after a spell in the background, where the
  // queue is at its longest (a hidden page draws nothing, so its idle slices do not
  // run and every pass queues up).
  //
  // A reader who follows needs none of this: the batch ends at the bottom instead
  // (see upgradeMarkdown).
  function holdView() {
    if (following) { return null; }
    var row = topVisibleRow();
    if (!row) { return null; }
    var top = row.getBoundingClientRect().top;
    return function () {
      // The log can be rebuilt under the batch (a snapshot arrived with it).
      if (row.parentNode !== log) { return; }
      log.scrollTop += row.getBoundingClientRect().top - top;
    };
  }

  // flushMarkdown is one idle slice of upgrades. A callback that finds an empty
  // queue — one flushMarkdownNow already drained — does nothing at all, so the
  // batch cannot be charged for a view write of its own.
  function flushMarkdown(deadline) {
    mdFlushScheduled = false;
    if (mdPending.size === 0) { return; }
    upgradeMarkdown(deadline, false);
    scheduleMarkdownFlush();
  }

  // flushMarkdownNow drains the whole queue in one task, with no slice budget: the
  // page is coming back to the foreground and has to be finished before it is
  // looked at again. Left to the idle slices, the same queue upgrades rows over the
  // frames that follow — the pass after pass holdView exists for — and it is at its
  // longest exactly here, since nothing queued for a page nobody was drawing. Rows
  // too large to render (MAX_MD_CHARS) stay plain text and bound the batch.
  function flushMarkdownNow() {
    if (mdPending.size === 0) { return; }
    upgradeMarkdown(null, true);
  }

  // upgradeMarkdown upgrades the queued rows and leaves the reader where they
  // belong. The Map keeps insertion order and the lowest text wins per row (a row
  // that was re-streamed while waiting is parsed once, with its final text). A row
  // still sitting in the batch fragment counts too: only rows whose log was
  // replaced (no parent left) are skipped. One slice stops when its time is up; a
  // whole pass (whole) takes the queue as it stands.
  function upgradeMarkdown(deadline, whole) {
    var release = holdView();
    var started = Date.now();
    var entries = mdPending.entries();
    for (var next = entries.next(); !next.done; next = entries.next()) {
      var span = next.value[0];
      var text = next.value[1];
      mdPending.delete(span);
      if ((span.isConnected || span.parentNode) && text.length <= MAX_MD_CHARS) {
        applyMarkdown(span, text);
      }
      if (!whole && sliceIsUp(deadline, started)) { break; }
    }
    if (release) { release(); }
    // A reader who follows ends the pass on the newest row, exactly like every
    // other content change (see keepBottom): the rows above them grew, and the pass
    // can pull the view back itself instead of waiting for the next output to. A
    // snapshot still being replayed is left alone — those rows are not in the log
    // yet, and the end of the replay pins the view once (see endHistory).
    if (!replaying) { keepBottom(); }
  }

  // sliceIsUp reports a slice's budget being spent: the browser's own deadline when
  // it gave one (it knows what is left of the frame), MD_SLICE_MS otherwise.
  function sliceIsUp(deadline, started) {
    if (deadline && typeof deadline.timeRemaining === 'function') {
      return deadline.timeRemaining() <= 1;
    }
    return Date.now() - started >= MD_SLICE_MS;
  }

  function setSpan(span, text, renderMD) {
    // The source text is kept aside before anything is drawn: the copy control
    // needs it in every branch below.
    if (span) { rowSource.set(span, text); }
    // While a snapshot is being replayed the markdown pass is deferred: the row is
    // drawn as plain text now and upgraded in an idle slice (see queueMarkdown).
    // Parsing thousands of rows as one long task is what froze the tab (and hid the
    // log behind "Waiting for messages…") on a long conversation. The one pass that
    // does run in a single task is the catch-up on the way back from the background
    // (see flushMarkdownNow): a page nobody was drawing got no further than its
    // queue, so that pass is bounded by one snapshot.
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

  // A streamed row is redrawn by parsing its whole text as markdown again, so it
  // is redrawn at most once per interval — and that interval is about what the
  // reader can see, not about saving work at their expense:
  //   * a visible desktop redraws as the chunks arrive (an interval of 0, which
  //     still coalesces the chunks of one burst into a single redraw),
  //   * a visible phone caps it at RENDER_LIVE_PHONE_MS: re-parsing the whole row
  //     costs much more there, and a tenth of a second is not visible in a stream,
  //   * a page in the background (which keeps working unless it was stopped)
  //     redraws once a second, since nobody is watching it.
  // Both streams end with a full redraw (the final assistant event and
  // finishReasoning), so a pending pass can never leave text unrendered.
  var RENDER_LIVE_PHONE_MS = 100;
  var RENDER_HIDDEN_MS = 1000;
  // answerRenderedAt/reasoningRenderedAt remember when each streamed row was last
  // drawn.
  var answerRenderedAt = 0;
  var reasoningRenderedAt = 0;

  function renderIntervalMs() {
    if (!visible()) { return RENDER_HIDDEN_MS; }
    return onPhone() ? RENDER_LIVE_PHONE_MS : 0;
  }

  // streamDelay reports how long the next redraw of a streamed row waits, given
  // how long ago its last pass was.
  function streamDelay(sinceLast) {
    var wait = renderIntervalMs() - sinceLast;
    return wait > 0 ? wait : 0;
  }

  // scheduleRender re-renders the streaming row on that cadence.
  function scheduleRender() {
    if (pendingRender) { return; }
    pendingRender = setTimeout(function () {
      pendingRender = null;
      answerRenderedAt = Date.now();
      setRow(current, currentText, MARKDOWN);
    }, streamDelay(Date.now() - answerRenderedAt));
  }

  // scheduleReasoningRender paces the thinking row the same way, through the
  // same markdown path as the visible answer.
  function scheduleReasoningRender() {
    if (pendingReasoningRender) { return; }
    pendingReasoningRender = setTimeout(function () {
      pendingReasoningRender = null;
      reasoningRenderedAt = Date.now();
      setRow(reasoningRow, reasoningText, MARKDOWN);
    }, streamDelay(Date.now() - reasoningRenderedAt));
  }

  // finishReasoning commits the streamed thinking row (if any) so the visible
  // answer starts in its own row. The commit is a full redraw, so the paced
  // state of the row goes with it.
  function finishReasoning() {
    if (!reasoningRow) { return; }
    if (pendingReasoningRender) { clearTimeout(pendingReasoningRender); pendingReasoningRender = null; }
    setRow(reasoningRow, reasoningText, MARKDOWN);
    reasoningRow = null;
    reasoningText = '';
    reasoningRenderedAt = 0;
  }

  // stampOf renders a message's start time for the role line: the clock time,
  // with the date in front once the message is not from today (a page left open
  // overnight shows "00:10" and "01-02 00:10" side by side, so the reader can
  // tell the two days apart). It returns '' for a missing or unparsable time,
  // which is what a row restored from a session file carries — the row is then
  // drawn without a stamp.
  function stampOf(t) {
    if (!t) { return ''; }
    var d = new Date(t);
    if (isNaN(d.getTime())) { return ''; }
    var clock = pad2(d.getHours()) + ':' + pad2(d.getMinutes()) + ':' + pad2(d.getSeconds());
    var now = new Date();
    var sameDay = d.getFullYear() === now.getFullYear() &&
      d.getMonth() === now.getMonth() && d.getDate() === now.getDate();
    return sameDay ? clock : pad2(d.getMonth() + 1) + '-' + pad2(d.getDate()) + ' ' + clock;
  }

  // timeSpan is the gray stamp of a message, drawn inside the role line.
  function timeSpan(stamp) {
    var el = document.createElement('span');
    el.className = 'time';
    el.textContent = stamp;
    return el;
  }

  // stampRow adds the gray start stamp of a message to a row that was drawn
  // without one (a pending row the agent has just sent). It does nothing when
  // the row already carries the stamp.
  function stampRow(row, stamp) {
    if (!row || !stamp) { return; }
    var label = row.querySelector('.role');
    if (!label || label.querySelector('.time')) { return; }
    label.appendChild(timeSpan(stamp));
  }

  // buildRow creates one transcript row. attachments are the files a message
  // carried (the page draws them under the text; see mediaList). foldable marks a row
  // the reader may fold to its first line (see the fold section). stamp is the
  // message's gray start time, or '' when it has none.
  function buildRow(cls, role, text, renderMD, attachments, foldable, stamp) {
    var row = document.createElement('div');
    row.className = 'row ' + cls;
    if (foldable) { markFoldable(row); }
    var label = null;
    if (role) {
      label = document.createElement('span');
      label.className = 'role';
      label.textContent = role;
      row.appendChild(label);
    }
    var t = document.createElement('span');
    setSpan(t, text, renderMD);
    row.appendChild(t);
    var media = mediaList(attachments);
    if (media) { row.appendChild(media); }
    // The stamp sits on the role line, next to the label it belongs to: to the
    // right of "agent", to the left of "you". A user message hugs the right edge
    // of the log, so its stamp is mirrored to stay on the message's side — it is
    // added after the copy control, which the CSS then orders ahead of it, and
    // the label follows (see .user .role .time in app.css).
    var user = cls === 'user' || cls === 'user pending';
    if (label && stamp && !user) { label.appendChild(timeSpan(stamp)); }
    // The messages the user wrote and the replies the agent produced can be
    // copied away: their control rides in the role line, next to the label it
    // belongs to (app.css keeps it out of the flow, so the line is untouched
    // until the icon is asked for).
    if (copyableRow(cls)) {
      if (label) { label.classList.add('with-actions'); }
      (label || row).appendChild(copyControl(row));
    }
    if (label && stamp && user) { label.appendChild(timeSpan(stamp)); }
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
    if (anchor) { log.insertBefore(row, anchor); } else { log.appendChild(row); }
    // The bottom follows a row that landed while the page was carrying the view
    // (see keepBottom).
    keepBottom();
  }

  function addRow(cls, role, text, renderMD, attachments, foldable, stamp) {
    var row = buildRow(cls, role, text, renderMD, attachments, foldable, stamp);
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
  // its place, which is where the message entered the conversation. attachments
  // are the files the agent reports for it: the row was drawn from the composer's
  // text alone, so the files it carried are added here. stamp is the message's
  // start time, which the row could not show while it was still pending.
  function settlePendingRow(text, attachments, stamp) {
    for (var i = 0; i < pendingRows.length; i++) {
      if (pendingRows[i].text !== text) { continue; }
      var row = pendingRows[i].el;
      pendingRows.splice(i, 1);
      // The pending mark rides on the class alone; the text is already the final
      // one, since it is the message the agent recorded.
      row.className = 'row user';
      stampRow(row, stamp);
      setRowMedia(row, attachments);
      return true;
    }
    return false;
  }

  // appendRow puts a row at the very end of the log (the pending messages it
  // queues behind), following it while the reader is following. A row raised while
  // a snapshot is being replayed joins the batch instead: the transcript on screen
  // is the one the snapshot replaces, and a held-back rebuild swaps the new one in
  // whole (see flushReplayBatch/endHistory), so a row added to the log itself would
  // be wiped away with it.
  function appendRow(row) {
    if (replayBatch) { replayBatch.appendChild(row); return; }
    log.appendChild(row);
    // The pending row a message adds is at the very bottom, so the bottom follows
    // it the same way (see keepBottom).
    keepBottom();
  }

  // rowText returns the row's own text span. It is not simply the last child: a
  // row that carries files keeps them after its text (see mediaList).
  function rowText(row) {
    if (!row) { return null; }
    for (var i = 0; i < row.children.length; i++) {
      if (row.children[i].classList.contains('text')) { return row.children[i]; }
    }
    return row.lastChild;
  }

  function setRow(el, text, renderMD) {
    if (!el) { return; }
    // A replayed row sits in the batch fragment (not in the log yet) and the view
    // is pinned once, when the snapshot ends: there is nothing to follow and no
    // layout to read here.
    if (replaying) { setSpan(rowText(el), text, renderMD); return; }
    // A streamed re-render can grow the row, so the bottom follows it — in this
    // same task while the page is carrying the view, which is what keeps a growing
    // answer from shaking the pending row below it (see keepBottom).
    setSpan(rowText(el), text, renderMD);
    keepBottom();
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
    // A tool call folds to its name: the arguments are the bulk of it (see the fold
    // section).
    markFoldable(row);
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

  // addResultRow draws one tool result. A result is the one foldable row whose first
  // line stays readable while folded, so its text is built as that line plus the rest
  // of it (splitFoldLine) — see the folded rules in app.css for why a height clamp is
  // not enough there.
  function addResultRow(isError, text) {
    var row = buildRow(isError ? 'error' : 'result', '', '[result] ' + text, false, null, true);
    splitFoldLine(rowText(row));
    placeRow(row);
    return row;
  }

  // splitFoldLine splits a plain-text row into the line it keeps while folded and the
  // rest of the text. Both halves stay inline spans, so while the row is unfolded the
  // text reads exactly as it was written (the second half opens with the newline that
  // separated them).
  function splitFoldLine(span) {
    if (!span) { return; }
    var text = span.textContent || '';
    var cut = text.indexOf('\n');
    var head = document.createElement('span');
    head.className = 'fold-head';
    head.textContent = cut < 0 ? text : text.slice(0, cut);
    span.textContent = '';
    span.appendChild(head);
    if (cut < 0) { return; }
    var tail = document.createElement('span');
    tail.className = 'fold-tail';
    tail.textContent = text.slice(cut);
    span.appendChild(tail);
  }

  // ---- copy ----
  // Every user message and every agent reply can leave the page on the clipboard
  // in three flavours: markdown (the message as it was written), HTML (the
  // rendered block, so a rich editor keeps the formatting) and text (what the row
  // reads as on screen). The control is a transparent icon tucked into the row's
  // role line, next to the label it belongs to, and it stays out of sight until
  // the reader asks for it — a desktop asks with a hover (app.css), a touch
  // screen, which has no hover, with a long press (see the long press section).
  // COPY_DONE_MS is how long the "copied" note (and the open menu) stays.
  var COPY_DONE_MS = 900;
  // copyTarget is the row the open menu acts on, copyButton the control it was
  // opened from and copyTimer the note's own clock.
  var copyTarget = null;
  var copyButton = null;
  var copyTimer = null;

  // copyableRow reports whether a row carries a copy control: the messages the
  // user wrote (a pending one too — its text is the message), the replies the agent
  // produced, and the compressed-context summary — the only copy of the messages
  // that were cut out of the context, so it has to be copyable too.
  function copyableRow(cls) {
    return cls === 'user' || cls === 'user pending' || cls === 'assistant' || cls === 'summary';
  }

  // copyGlyph draws the control's icon: two overlapping sheets, stroked with the
  // page's own colour (currentColor), so it reads as a transcript affordance and
  // not as a button with a label of its own.
  function copyGlyph() {
    var NS = 'http://www.w3.org/2000/svg';
    var svg = document.createElementNS(NS, 'svg');
    svg.setAttribute('viewBox', '0 0 16 16');
    svg.setAttribute('aria-hidden', 'true');
    svg.setAttribute('focusable', 'false');
    var back = document.createElementNS(NS, 'path');
    back.setAttribute('d', 'M10.4 5.9V4.1a1.6 1.6 0 0 0-1.6-1.6H4.1a1.6 1.6 0 0 0-1.6 1.6v4.7a1.6 1.6 0 0 0 1.6 1.6h1.8');
    var front = document.createElementNS(NS, 'path');
    front.setAttribute('d', 'M7.6 5.9h4.3a1.6 1.6 0 0 1 1.6 1.6v4.4a1.6 1.6 0 0 1-1.6 1.6H7.6A1.6 1.6 0 0 1 6 11.9V7.5a1.6 1.6 0 0 1 1.6-1.6z');
    svg.appendChild(back);
    svg.appendChild(front);
    return svg;
  }

  // copyControl builds one row's copy control: a transparent icon button that
  // opens the shared menu on the row it belongs to. It carries no text of its
  // own — the role line of the message is where it lives, and the menu names the
  // three flavours.
  function copyControl(row) {
    var box = document.createElement('span');
    box.className = 'row-actions';
    var btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'copy';
    btn.title = 'copy this message as markdown, HTML or text';
    btn.setAttribute('aria-label', 'copy this message');
    btn.setAttribute('aria-haspopup', 'menu');
    btn.setAttribute('aria-expanded', 'false');
    btn.appendChild(copyGlyph());
    btn.onclick = function (e) {
      // The popup lives outside the row: this click must not reach the document
      // handler that closes it.
      e.stopPropagation();
      if (copyPop && !copyPop.hidden && copyTarget === row) { closeCopyMenu(); return; }
      openCopyMenu(row, btn);
    };
    box.appendChild(btn);
    return box;
  }

  // ---- revealing the icon without a hover ----
  // A touch screen has no hover, so the icon is brought up by a long press on the
  // row itself (and by a plain tap, which does nothing else on a message): the
  // revealed row keeps it until the reader taps somewhere else. The press is
  // measured the way every long press is — a finger that drifts (a scroll) or
  // lifts early is not one.
  var LONG_PRESS_MS = 450;
  var LONG_PRESS_SLOP = 10; // pixels a finger may drift while the press runs
  var longPressTimer = null;
  var longPressRow = null;
  var longPressFrom = null;
  var longPressAt = 0;
  var revealedRow = null;

  // touchScreen reports a device whose pointer has no hover: there app.css cannot
  // reveal the icon, so the page has to.
  function touchScreen() {
    return !!(coarseQuery && coarseQuery.matches);
  }

  // copyRowOf walks up from an event target to the transcript row it belongs to.
  function copyRowOf(node) {
    while (node && node !== log) {
      if (node.classList && node.classList.contains('row')) { return node; }
      node = node.parentNode;
    }
    return null;
  }

  function revealCopyControl(row) {
    if (!row || revealedRow === row) { return; }
    hideCopyControl();
    revealedRow = row;
    row.classList.add('show-actions');
  }

  function hideCopyControl() {
    if (!revealedRow) { return; }
    revealedRow.classList.remove('show-actions');
    revealedRow = null;
  }

  function cancelLongPress() {
    if (longPressTimer) { clearTimeout(longPressTimer); longPressTimer = null; }
    longPressRow = null;
    longPressFrom = null;
    longPressAt = 0;
  }

  function onCopyTouchStart(e) {
    if (e.touches.length !== 1) { cancelLongPress(); return; }
    var row = copyRowOf(e.target);
    if (!row || !row.querySelector('.row-actions')) { cancelLongPress(); return; }
    longPressRow = row;
    longPressFrom = { x: e.touches[0].clientX, y: e.touches[0].clientY };
    longPressAt = Date.now();
    // The press reveals the icon as soon as it is long enough; the press record
    // stays for touchend, which is what suppresses the click such a press would
    // otherwise turn into (see onCopyTouchEnd).
    longPressTimer = setTimeout(function () {
      longPressTimer = null;
      revealCopyControl(longPressRow);
    }, LONG_PRESS_MS);
  }

  // onCopyTouchEnd finishes the press. A press that lasted long enough reveals
  // the icon even if its timer was late — a page in the background has its timers
  // throttled while the events still arrive on time. The click the browser
  // synthesizes for that press is dropped: the icon it just brought up must not
  // be toggled away by it. This is why this listener is not passive.
  function onCopyTouchEnd(e) {
    var row = longPressRow;
    var held = longPressAt ? Date.now() - longPressAt : 0;
    cancelLongPress();
    if (!row || held < LONG_PRESS_MS) { return; }
    if (e.cancelable) { e.preventDefault(); }
    revealCopyControl(row);
  }

  function onCopyTouchMove(e) {
    if (!longPressFrom || e.touches.length !== 1) { return; }
    var dx = e.touches[0].clientX - longPressFrom.x;
    var dy = e.touches[0].clientY - longPressFrom.y;
    if (dx * dx + dy * dy > LONG_PRESS_SLOP * LONG_PRESS_SLOP) { cancelLongPress(); }
  }

  // openCopyMenu shows the menu next to the button that opened it. The button
  // keeps the focus: the entries are one Tab away, Escape comes back here, and a
  // phone must not raise its keyboard for a menu tap.
  function openCopyMenu(row, btn) {
    if (!copyPop) { return; }
    if (copyTimer) { clearTimeout(copyTimer); copyTimer = null; }
    // The marks of the previous copy go first: a menu always opens with its
    // entries in their idle colour.
    clearCopyMarks();
    if (copyNote) { copyNote.textContent = ''; }
    copyTarget = row;
    copyButton = btn;
    btn.setAttribute('aria-expanded', 'true');
    copyPop.hidden = false;
    placeCopyMenu(btn);
    try { btn.focus(); } catch (err) { /* focus is a nicety */ }
  }

  // closeCopyMenu hides the menu and forgets the row it acted on.
  function closeCopyMenu() {
    if (copyTimer) { clearTimeout(copyTimer); copyTimer = null; }
    if (copyButton) {
      copyButton.setAttribute('aria-expanded', 'false');
      copyButton = null;
    }
    copyTarget = null;
    clearCopyMarks();
    if (!copyPop || copyPop.hidden) { return; }
    copyPop.hidden = true;
    if (copyNote) { copyNote.textContent = ''; }
  }

  // clearCopyMarks puts the entries back to their idle colour. The green says
  // "this is the flavour you just copied" and is only meant to last as long as
  // the menu that copied it — otherwise the next row's menu would open with one
  // entry still green, which reads as the state of the page rather than as a
  // remark about the copy that is over.
  function clearCopyMarks() {
    if (!copyPop) { return; }
    var done = copyPop.querySelectorAll('button.done');
    for (var i = 0; i < done.length; i++) { done[i].classList.remove('done'); }
  }

  // placeCopyMenu puts the popup next to the button that opened it. It lives
  // inside .app — the popup's containing block — so it is never clipped by the
  // log's own scroll box; it hangs under the button and flips above it (which
  // also turns its entrance animation around, see the .copy-pop rules) when the
  // bottom of the page is close.
  function placeCopyMenu(btn) {
    var host = copyPop.offsetParent; // .app, while the popup is on screen
    if (!host) { return; }
    var box = btn.getBoundingClientRect();
    var base = host.getBoundingClientRect();
    var left = box.left - base.left;
    var rightmost = host.clientWidth - copyPop.offsetWidth - 8;
    if (left > rightmost) { left = rightmost; }
    if (left < 8) { left = 8; }
    var top = box.bottom - base.top + 6;
    var flip = top + copyPop.offsetHeight > host.clientHeight - 8;
    if (flip) { top = box.top - base.top - copyPop.offsetHeight - 6; }
    if (top < 8) { top = 8; }
    copyPop.classList.toggle('flip', flip);
    copyPop.style.left = Math.round(left) + 'px';
    copyPop.style.top = Math.round(top) + 'px';
  }

  // copyRow hands one row to the clipboard. mode is the flavour the user picked;
  // an HTML copy carries the plain text too, so a paste into a plain editor stays
  // clean.
  function copyRow(row, mode, item) {
    var span = rowText(row);
    if (!span) { return; }
    var source = rowSourceText(span);
    var text = source;
    var html = '';
    if (mode === 'text') { text = rowPlainText(span, source); }
    else if (mode === 'html') {
      text = rowPlainText(span, source);
      html = rowHTML(span, source);
    }
    writeClipboard(text, html).then(function (ok) { noteCopied(ok, item); });
  }

  // rowSourceText is the text the row was drawn from; a span that was never
  // filled falls back to what it shows.
  function rowSourceText(span) {
    var source = rowSource.get(span);
    return typeof source === 'string' ? source : (span.textContent || '');
  }

  // rowPlainText is the row as it reads on screen: a markdown-rendered row is
  // taken from the browser's own rendering of it (the markers are gone), a row
  // drawn as plain text already is plain text.
  function rowPlainText(span, source) {
    if (!span.classList.contains('md')) { return source; }
    var text = span.innerText;
    return (typeof text === 'string' && text !== '') ? text : (span.textContent || source);
  }

  // rowHTML renders a row as HTML for the clipboard: the row's own markup when it
  // is already rendered, otherwise the source parsed with the same libraries
  // (which works while the /markdown switch is off too), and escaped text as the
  // last resort — so the entry is never empty.
  function rowHTML(span, source) {
    if (span.classList.contains('md')) { return span.innerHTML; }
    var html = markdownToHTML(source);
    if (html !== null) { return html; }
    return escapeHTML(source).replace(/\n/g, '<br>');
  }

  function escapeHTML(text) {
    return String(text).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
  }

  // writeClipboard puts text — and, when asked for, an HTML flavour — on the
  // clipboard. The async Clipboard API exists only in a secure context, and the
  // mirror is normally reached over plain http (a LAN address, from a phone), so
  // the older path is the one those pages take. Both are started inside the click
  // that asked for the copy, which is what iOS Safari requires.
  function writeClipboard(text, html) {
    var api = navigator.clipboard;
    if (window.isSecureContext && api && api.write && typeof ClipboardItem === 'function') {
      var parts = { 'text/plain': new Blob([text], { type: 'text/plain' }) };
      if (html) { parts['text/html'] = new Blob([html], { type: 'text/html' }); }
      return api.write([new ClipboardItem(parts)]).then(function () { return true; },
        function () { return legacyCopy(text, html); });
    }
    return Promise.resolve(legacyCopy(text, html));
  }

  // legacyCopy copies through a selection, which is how a page without the
  // Clipboard API does it: an off-screen textarea holds the text, the copy event
  // carries the HTML flavour (a rich editor takes it, a plain one the text), and
  // the reader's own selection is put back afterwards. A textarea rather than a
  // range, because iOS Safari only copies out of a form field.
  function legacyCopy(text, html) {
    var holder = document.createElement('textarea');
    holder.value = text;
    holder.setAttribute('readonly', 'readonly');
    holder.setAttribute('aria-hidden', 'true');
    holder.setAttribute('tabindex', '-1');
    holder.style.position = 'absolute';
    holder.style.left = '-9999px';
    holder.style.top = '0';
    holder.style.width = '1px';
    holder.style.height = '1px';
    document.body.appendChild(holder);
    var selection = window.getSelection();
    var saved = selection && selection.rangeCount > 0 ? selection.getRangeAt(0) : null;
    holder.select();
    holder.setSelectionRange(0, text.length);
    var onCopy = function (e) {
      if (!e.clipboardData) { return; }
      e.clipboardData.setData('text/plain', text);
      if (html) { e.clipboardData.setData('text/html', html); }
      e.preventDefault();
    };
    document.addEventListener('copy', onCopy);
    var ok = false;
    try { ok = document.execCommand('copy'); } catch (err) { ok = false; }
    document.removeEventListener('copy', onCopy);
    if (holder.parentNode) { holder.parentNode.removeChild(holder); }
    if (selection) {
      selection.removeAllRanges();
      // The saved range can point into a row that is gone by now.
      if (saved) {
        try { selection.addRange(saved); } catch (err) { /* nothing to restore */ }
      }
    }
    return ok;
  }

  // noteCopied reports the outcome in a small pill under the menu and closes it;
  // a failed copy is the one case worth a hint, and the full advice lives in the
  // pill's tooltip so the line itself stays short.
  function noteCopied(ok, item) {
    if (copyButton) { try { copyButton.focus(); } catch (err) { /* focus is a nicety */ } }
    if (!ok) {
      if (copyNote) {
        copyNote.textContent = 'copy failed';
        copyNote.title = 'select the text and press Ctrl+C';
      }
      return;
    }
    if (item && item.classList) { item.classList.add('done'); }
    if (copyNote) {
      copyNote.textContent = 'copied';
      copyNote.title = '';
    }
    copyTimer = setTimeout(closeCopyMenu, COPY_DONE_MS);
  }

  if (copyPop) {
    // An entry copies the row the menu was opened on.
    copyPop.addEventListener('click', function (e) {
      var item = e.target && e.target.closest ? e.target.closest('button[data-copy]') : null;
      if (!item || !copyTarget) { return; }
      copyRow(copyTarget, item.getAttribute('data-copy'), item);
    });
    // A click on the transcript ends the reveal a long press started, and on a
    // touch screen it brings up the icon of the row that was tapped instead —
    // tapping that row again puts it away (the icon itself never gets here: it
    // stops the event). Any click that moves a row out from under an open menu
    // closes that too.
    document.addEventListener('click', function (e) {
      var row = copyRowOf(e.target);
      var wasRevealed = revealedRow;
      hideCopyControl();
      if (touchScreen() && row && row !== wasRevealed && row.querySelector('.row-actions')) {
        revealCopyControl(row);
      }
      if (copyPop.hidden) { return; }
      if (copyPop.contains(e.target)) { return; } // an entry handles itself
      closeCopyMenu();
    });
    document.addEventListener('keydown', function (e) {
      if (!copyPop.hidden && (e.key === 'Escape' || e.keyCode === 27)) { closeCopyMenu(); }
    });
    log.addEventListener('scroll', closeCopyMenu, { passive: true });
    window.addEventListener('resize', closeCopyMenu);
    // The long press that brings the icon up where there is no hover: the two
    // listeners that follow a gesture are passive (scrolling and text selection
    // stay the browser's own gestures), the two that end it are not — touchend
    // drops the click such a press would turn into.
    log.addEventListener('touchstart', onCopyTouchStart, { passive: true });
    log.addEventListener('touchmove', onCopyTouchMove, { passive: true });
    log.addEventListener('touchend', onCopyTouchEnd);
    log.addEventListener('touchcancel', cancelLongPress, { passive: true });
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

  // ---- the rail as a drawer (phones) ----
  // A phone lays the page out in one column, so the rail is folded off-canvas
  // under the banner and the mark in that banner is the handle that slides it in
  // over the content (the drawer itself is in app.css). The breakpoint is the
  // stylesheet's own, read the other way round: what a viewport narrower than the
  // desktop grid gets is the drawer, and the same query tells this script when the
  // rail has left the screen for good — there the mark is decoration and the rail
  // is a column of the grid.
  var RAIL_SHOW = 'show the session panel';
  var RAIL_HIDE = 'hide the session panel';
  // What a tap inside the rail is allowed to answer with "and now show me the
  // page": a command (it runs on the connection) and the editor or the voice panel
  // (they come up over the page).
  var RAIL_ACTION = '[data-cmd],[data-config-open],[data-tts-open]';
  var wideQuery = window.matchMedia ? window.matchMedia('(min-width: 1000px)') : null;
  var appEl = document.querySelector('.app');
  var railEl = document.getElementById('sideRail');
  var railToggleEl = document.getElementById('railToggle');
  var railBackdropEl = document.getElementById('railBackdrop');

  // railOpen reports whether the drawer is out. The class on .app is the state
  // itself — the stylesheet moves the rail and the scrim from it — so there is
  // never a second copy of it to drift.
  function railOpen() { return !!appEl && appEl.classList.contains('rail-open'); }

  // setRailOpen slides the drawer in or out. A desktop is always the closed state:
  // the rail is a column there, and the class would ask the stylesheet for a scrim
  // over a rail that is in view anyway.
  function setRailOpen(open) {
    if (!appEl || !railEl || !railToggleEl) { return; }
    var drawer = !(wideQuery && wideQuery.matches);
    open = !!open && drawer;
    var wasOpen = railOpen();
    // Read where the focus stands before the class moves: a rail that becomes
    // hidden by the stylesheet drops the focus it held, so the answer has to be
    // taken while it is still on screen.
    var focusInsideRail = railEl.contains(document.activeElement);
    appEl.classList.toggle('rail-open', open);
    if (drawer) {
      railToggleEl.setAttribute('aria-expanded', open ? 'true' : 'false');
      railToggleEl.title = open ? RAIL_HIDE : RAIL_SHOW;
      railToggleEl.removeAttribute('aria-hidden');
    } else {
      // On a desktop the rail never leaves the screen, so the mark is plain
      // decoration again: nothing to expand, no tooltip, no tab stop (a button
      // that does nothing is worse than no button) and no name for a screen
      // reader to announce — the stylesheet lets the pointer through it.
      railToggleEl.removeAttribute('aria-expanded');
      railToggleEl.removeAttribute('title');
      railToggleEl.setAttribute('aria-hidden', 'true');
    }
    railToggleEl.tabIndex = drawer ? 0 : -1;
    if (!open && focusInsideRail) {
      // Nothing may keep the focus inside a rail the layout is folding away, so it
      // goes back to the handle that owns it.
      railToggleEl.focus();
    }
    if (open && !wasOpen) {
      // ...and a fresh drawer takes it, so a keyboard lands inside what it just
      // opened instead of walking the banner again (the rail itself is a
      // tabindex="-1" stop, see index.html).
      railEl.focus();
    }
  }

  if (railToggleEl) {
    railToggleEl.onclick = function () { setRailOpen(!railOpen()); };
  }
  if (railBackdropEl) {
    railBackdropEl.onclick = function () { setRailOpen(false); };
  }
  // One action inside the rail brings the page back: a command runs on the
  // connection, the editor and the voice panel open over everything, and the
  // reader is looking at the transcript again either way. The rail's own controls
  // stay where they are — the Commands fold and the read-aloud switch are rail
  // state, not an action somewhere else.
  if (railEl && railEl.addEventListener) {
    railEl.addEventListener('click', function (e) {
      var node = e.target;
      while (node && node !== railEl && !(node.matches && node.matches(RAIL_ACTION))) { node = node.parentNode; }
      if (node && node !== railEl) { setRailOpen(false); }
    });
  }
  // Escape closes the drawer, the way it closes the panels (config.js and the
  // read-aloud one). A panel above the rail owns the key while it is up: it is what
  // covers the page, and one key closing both would be a surprise.
  document.addEventListener('keydown', function (e) {
    if (e.key !== 'Escape' || !railOpen()) { return; }
    if (document.querySelector('.modal:not([hidden])')) { return; }
    setRailOpen(false);
  });
  // Crossing the breakpoint (a rotation, a resized window) leaves the drawer state
  // behind: the rail is a column on the wide side of it, so a stale "open" would
  // bring a scrim over the layout the moment the narrow side came back.
  if (wideQuery) {
    var onRailLayoutChange = function () { setRailOpen(false); };
    if (wideQuery.addEventListener) { wideQuery.addEventListener('change', onRailLayoutChange); }
    else if (wideQuery.addListener) { wideQuery.addListener(onRailLayoutChange); }
  }
  setRailOpen(false);

  var ws = null;
  // historyVersion is the transcript version the log on screen was built from. It
  // rides back on the next connection (?since=), so a page that reconnects after
  // nothing happened is answered with history_same and does not rebuild its log.
  var historyVersion = 0;
  // replayVersion is the version of the snapshot being replayed. It is committed to
  // historyVersion only once the snapshot reached the log (see endHistory): a
  // snapshot that died on the way leaves the page asking for the transcript it
  // really has, instead of reporting one it never received.
  var replayVersion = 0;
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
    // event or replayed from the history frame. It carries the moment of the
    // compaction like any other message.
    if (kind === 'summary') { addRow('summary', SUMMARY_ROLE, ev.text || '', MARKDOWN, null, false, stampOf(ev.time)); return; }
    if (kind === 'user') {
      setRunning(true);
      // A message this page sent while the turn was running is being sent now:
      // Its pending row becomes an ordinary one, in place (it already sits after
      // everything the interrupted reply produced), picking up the files the
      // message carried. The files can make the row taller, so the bottom follows
      // it the same way (see keepBottom) — only ever while the page is live: a
      // replayed row reads no layout and writes nothing (see the replay path in
      // setRow/placeRow), and pendingRows is empty during one, so the settle below
      // misses anyway.
      if (settlePendingRow(ev.text || '', ev.attachments, stampOf(ev.time))) {
        if (queued) { queued--; tickTurn(); }
        if (!replaying) { keepBottom(); }
        return;
      }
    }
    if (kind === 'turn_done' || kind === 'interrupted') { setRunning(false); }
    if (kind === 'reasoning_delta') {
      if (!reasoningRow) {
        reasoningRow = addRow('reasoning', 'thinking', '', MARKDOWN, null, true);
        // A fresh row has nothing drawn yet, so its first pass is immediate.
        reasoningRenderedAt = 0;
      }
      reasoningText += ev.text || '';
      scheduleReasoningRender();
      return;
    }
    if (kind === 'assistant_delta') {
      if (!current) {
        // The message's start time is the first chunk of the reply — its
        // thinking included, so a reply that begins with thinking is stamped
        // where it began, not where its visible text did.
        current = addRow('assistant', 'agent', '', false, null, false, stampOf(ev.time));
        answerRenderedAt = 0;
      }
      currentText += ev.text || '';
      if (MARKDOWN) { scheduleRender(); } else { setRow(current, currentText, false); }
      return;
    }
    var streamed = current;
    var streamedText = currentText;
    current = null;
    currentText = '';
    // The streamed row is finished: a paced redraw still waiting would only redraw
    // it with the text the branches below render in full.
    if (pendingRender) { clearTimeout(pendingRender); pendingRender = null; }
    if (kind === 'turn_done') { return; }
    if (kind === 'user') { addRow('user', 'you', ev.text || '', false, ev.attachments, false, stampOf(ev.time)); }
    else if (kind === 'assistant') {
      var text = ev.text || streamedText;
      if (streamed) { setRow(streamed, text, true); }
      else { addRow('assistant', 'agent', text, true, null, false, stampOf(ev.time)); }
    }
    else if (kind === 'tool_call') { addToolRow(ev.name, ev.args); }
    else if (kind === 'tool_result') {
      // Match the CLI: an empty result is only surfaced when it failed.
      if (!ev.text) {
        if (ev.is_error) { addRow('error', '', '[result] (error)', false); }
        return;
      }
      // A result folds to its first line (see addResultRow); an info row, which
      // shares the class, is not a tool result and stays whole.
      addResultRow(!!ev.is_error, ev.text);
    }
    else if (kind === 'info') { addRow('result', '', '[info] ' + (ev.text || ''), false); syncResults(ev.text || ''); }
    else if (kind === 'compacted') {
      // The info row counts what was compressed away; the summary block shows
      // what replaced it. The mirror records the same block at this point of its
      // scrollback, so a reload replays it exactly here.
      addRow('result', '', '[info] ' + (ev.text || ''), false);
      if (ev.summary) { render('summary', { text: ev.summary, time: ev.time }); }
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
  // snapshot ends). A held-back rebuild (swapInAtEnd) keeps its rows in the
  // fragment instead, and the log is replaced in one insert when the snapshot is
  // complete (see endHistory): the reader keeps looking at the transcript they had
  // until the new one is whole, rather than at the top of the new one while the
  // rest of it is still on its way.
  function flushReplayBatch() {
    if (!replayBatch || swapInAtEnd) { return; }
    if (replayBatch.childNodes.length > 0) { log.appendChild(replayBatch); }
    replayBatch = document.createDocumentFragment();
  }

  // beginHistory starts the replay of the snapshot the header announces. A
  // snapshot always describes the full conversation, so it replaces the log
  // instead of appending (otherwise reconnects duplicate every message). A
  // transcript already on screen is replaced only when the new one is complete
  // (swapInAtEnd, see flushReplayBatch): wiping the log here would drop the view to
  // the top of an empty box, and a long conversation takes a visible while to
  // arrive — the reader would watch the top of the new transcript for all of it and
  // then see it snap to the bottom. A first load has no old transcript to hold on
  // to and paints as its batches arrive.
  function beginHistory(ev) {
    // The version is only remembered here: it is committed when the whole snapshot
    // reached the log (see endHistory).
    if (typeof ev.version === 'number') { replayVersion = ev.version; }
    swapInAtEnd = log.childNodes.length > 0;
    // A snapshot rebuilds the transcript from scratch: the page starts over at
    // the newest row (the replay ends pinned there) and follows it again, and the
    // rows the jump buttons pointed at are gone with the old ones.
    setFollowing(true);
    lastJumpRow = null;
    current = null;
    currentText = '';
    answerRenderedAt = 0;
    if (pendingRender) { clearTimeout(pendingRender); pendingRender = null; }
    // The rebuild replaces every row, so the open thinking row of a stream that
    // is gone with it is dropped too (the next chunk draws a fresh one). Pending
    // messages are gone as well: the agent draws their rows when it sends them.
    reasoningRow = null;
    reasoningText = '';
    reasoningRenderedAt = 0;
    if (pendingReasoningRender) { clearTimeout(pendingReasoningRender); pendingReasoningRender = null; }
    pendingRows = [];
    // The rows the voice was reading are gone: drop its buffers and silence it.
    TTS.reset();
    // The open copy menu points at a row that is being replaced.
    closeCopyMenu();
    hideCopyControl();
    // Nothing queued points at a row in the document any more.
    mdPending.clear();
    replaying = true;
    log.classList.add('replaying');
    replayBatch = document.createDocumentFragment();
    historyBusy = !!ev.busy;
    setUsage(ev.tokens || 0, ev.window || 0);
    applySettings(ev);
  }

  // recordHistoryVersion remembers which transcript the log was built from, so the
  // next connection can report it as ?since=. The replay path does not use it: a
  // snapshot commits its version only once it reached the log (see replayVersion
  // and endHistory).
  function recordHistoryVersion(ev) {
    if (typeof ev.version === 'number') { historyVersion = ev.version; }
  }

  // sameHistory applies the header of a "nothing new" reply: the mirror recorded
  // nothing since the version this page reports, so the transcript on screen is
  // still current. Only what can change without the rows changed — the usage
  // numbers, the running flag and the switches — and the log itself is left
  // exactly as it is. That is what keeps a phone coming back from the background
  // from rebuilding (and repainting) a long conversation that did not change.
  function sameHistory(ev) {
    recordHistoryVersion(ev);
    setUsage(ev.tokens || 0, ev.window || 0);
    applySettings(ev);
    setRunning(!!ev.busy);
  }

  // appendHistoryRows draws one batch of the snapshot and hands it to the log.
  // Batches keep the browser responsive: each frame the page receives is small, and
  // a batch of a first fill reaches the document in one insert. A held-back rebuild
  // keeps the batch in the replay fragment instead (see flushReplayBatch).
  function appendHistoryRows(ev) {
    if (!replayBatch) { beginHistory(ev); }
    (ev.messages || []).forEach(function (m) {
      if (m.role === 'user') { render('user', { text: m.content, attachments: m.attachments, time: m.time }); }
      else if (m.role === 'assistant' && m.content) { render('assistant', { text: m.content, time: m.time }); }
      else if (m.role === 'reasoning') { render('reasoning_delta', { text: m.content }); }
      else if (m.role === 'tool_call') { render('tool_call', { name: m.name, args: m.args }); }
      else if (m.role === 'tool_result') { render('tool_result', { text: m.content, is_error: m.is_error }); }
      else if (m.role === 'info') { render('info', { text: m.content }); }
      else if (m.role === 'summary') { render('summary', { text: m.content, time: m.time }); }
      else if (m.role === 'error') { render('error', { text: m.content }); }
      else if (m.role === 'interrupted') { render('interrupted', { text: m.content }); }
    });
    flushReplayBatch();
  }

  // endHistory closes the replay: the snapshot's rows reach the log, the view is
  // pinned to the bottom (the way a refreshed page sits) and the running indicator
  // picks up whatever the header reported. A held-back rebuild replaces the
  // transcript in one insert (see flushReplayBatch): an empty snapshot still has to
  // clear what is on screen, and the messages sent while the snapshot was arriving
  // are newer than everything it carries, so they go after the last replayed row.
  function endHistory() {
    if (!replaying) { return; }
    for (var i = 0; i < pendingRows.length; i++) {
      if (replayBatch && pendingRows[i].el.parentNode === replayBatch) { replayBatch.appendChild(pendingRows[i].el); }
    }
    var swapped = swapInAtEnd;
    if (swapped) {
      log.innerHTML = '';
      if (replayBatch) { log.appendChild(replayBatch); }
    } else {
      flushReplayBatch();
    }
    replayBatch = null;
    swapInAtEnd = false;
    // The log now shows the snapshot's transcript, so a reconnect may report its
    // version (see replayVersion).
    if (replayVersion) { historyVersion = replayVersion; replayVersion = 0; }
    replaying = false;
    log.classList.remove('replaying');
    // The compressed-context summary is not appended here: it is one of the rows
    // replayed above, recorded exactly where the context was cut, so a reload
    // rebuilds the truncation marker in the right place.
    setRunning(historyBusy);
    // A transcript swapped in whole is a change the page made to the view it is
    // carrying, so the bottom goes in with the same task: a write left to the
    // animation frame would paint the new rows once at the position the old ones
    // left behind.
    if (swapped && following) { writeBottom(); return; }
    pinBottom();
  }

  // dropReplayState discards an unfinished replay: a first fill that the socket
  // closed in the middle of stays half-built (a held-back rebuild never touched the
  // log, see flushReplayBatch), and the next connection replaces it with a fresh
  // snapshot anyway. The transcript a held-back snapshot was going to replace is
  // still the one on screen, so only the queued markdown upgrades of the discarded
  // rows have to go.
  function dropReplayState() {
    if (swapInAtEnd) { mdPending.clear(); }
    // The snapshot never reached the log: the transcript on screen keeps its own
    // version, so the next connection asks for a full one again (see replayVersion).
    replayVersion = 0;
    replayBatch = null;
    swapInAtEnd = false;
    replaying = false;
    log.classList.remove('replaying');
  }

  function wsURL() {
    var proto = location.protocol === 'https:' ? 'wss://' : 'ws://';
    // The session cookie rides on the handshake, so there is no token to add. The
    // version the log on screen was built from travels as ?since=: when the
    // mirror's own version still matches, it answers history_same instead of
    // replaying the rows (and the page keeps its log).
    var url = proto + location.host + '/ws';
    if (historyVersion > 0) { url += '?since=' + historyVersion; }
    return url;
  }

  var reconnectTimer = null;
  // The reconnect backoff. It doubles per failed attempt up to RECONNECT_MAX_MS
  // and a handshake that succeeds clears it, so a healthy page stays at the base
  // delay while a phone that lost its network (or was asleep) does not wake its
  // radio every 1.5s forever.
  var reconnectDelay = 0;
  var RECONNECT_MIN_MS = 1500;
  var RECONNECT_MAX_MS = 60000;

  // nextReconnectDelay advances the backoff and applies jitter, so pages that
  // lost the same network do not retry in lockstep. The cap also bounds the
  // jittered wait, so a sleeping phone never sees an attempt later than
  // RECONNECT_MAX_MS.
  function nextReconnectDelay() {
    reconnectDelay = reconnectDelay ? Math.min(reconnectDelay * 2, RECONNECT_MAX_MS) : RECONNECT_MIN_MS;
    return Math.min(Math.round(reconnectDelay * (0.75 + Math.random() * 0.5)), RECONNECT_MAX_MS);
  }

  // A restart replaces the process behind the mirror: the config panel
  // (config.js) reports an accepted one through window.MIRROR, so the header says
  // what the silence is — the new process binds the same address, and this page
  // reconnects to it — instead of "offline", which reads like a failure. The
  // retries also start from the base delay again, so a page whose earlier
  // attempts failed does not back off right when the mirror is coming back.
  var restarting = false;
  window.MIRROR = {
    restarting: function () {
      restarting = true;
      reconnectDelay = 0;
      statusEl.textContent = 'restarting…';
    },
    // onReconnect is installed by the panel that asked for the restart: it is
    // called once the page is connected again (see notifyReconnected), so the
    // control it disabled goes back into service without a reload.
    onReconnect: null
  };

  // notifyReconnected tells that panel the mirror is back.
  function notifyReconnected() {
    var hook = window.MIRROR && window.MIRROR.onReconnect;
    if (hook) { hook(); }
  }

  function connect() {
    if (!AUTH().ok()) { return; }
    if (ws && (ws.readyState === 0 || ws.readyState === 1)) { return; }
    ws = new WebSocket(wsURL());
    ws.onopen = function () {
      // The connection is healthy again: the next drop starts from the base
      // delay instead of continuing to back off.
      reconnectDelay = 0;
      var cameBack = restarting;
      restarting = false;
      dot.classList.add('on');
      dot.title = 'connected';
      statusEl.textContent = 'online';
      sendEl.disabled = false;
      if (cameBack) { notifyReconnected(); }
    };
    ws.onclose = function () {
      dot.classList.remove('on');
      dot.title = 'disconnected';
      statusEl.textContent = restarting ? 'restarting…' : 'offline';
      sendEl.disabled = true;
      // A socket that dropped in the middle of a snapshot leaves a half-built
      // log: drop the replay state before clearing the indicator, so the
      // indicator is not swallowed by the replay guard.
      dropReplayState();
      setRunning(false);
      if (!AUTH().ok()) { return; } // signed out: wait for a new session
      // A stopped page does not dial: it has nothing to draw, the socket would be
      // pinged awake for as long as the phone is asleep, and coming back reads a
      // fresh snapshot anyway (see goActive). A page that is merely in the
      // background keeps its mirror alive.
      if (stopped) { return; }
      if (reconnectTimer) { return; }
      reconnectTimer = setTimeout(function () {
        reconnectTimer = null;
        // A session can expire between reconnects, so check before dialing.
        ensureSession().then(function (ok) {
          if (ok) { connect(); } else { AUTH().prompt('Your session ended. Sign in again.'); }
        }).catch(function () { connect(); });
      }, nextReconnectDelay());
    };
    ws.onerror = function () { try { ws.close(); } catch (e) {} };
    ws.onmessage = function (e) {
      var ev;
      try { ev = JSON.parse(e.data); } catch (err) { return; }
      // The conversation snapshot arrives in three parts (see beginHistory):
      // header, row batches, terminator, so a long conversation paints while it
      // is still arriving instead of blocking on one giant frame.
      if (ev.type === 'history_start') { beginHistory(ev); return; }
      if (ev.type === 'history_same') { sameHistory(ev); return; }
      if (ev.type === 'history_rows') { appendHistoryRows(ev); return; }
      if (ev.type === 'history_end') { endHistory(); return; }
      // The switches the rail mirrors: no row, just state.
      if (ev.type === 'settings') { applySettings(ev); return; }
      render(ev.type, ev);
    };
  }

  // ---- what a hidden page keeps doing ----
  // The agent runs on the server, so a page nobody is looking at has nothing that
  // has to stay current — how far that goes depends on the device. A desktop
  // stays live (people switch tabs constantly and the machine is on mains), only
  // easing its redraws to one per second (see renderIntervalMs/turnTickMs). A
  // phone gets HIDDEN_GRACE_MS_PHONE of grace and is then stopped: its clock, its
  // socket (the server pings an idle connection every 30s, and every ping wakes a
  // radio), its idle markdown slices and its CSS animation all go. Coming back
  // from a stopped page dials again and rebuilds the log from the snapshot, which
  // is exactly how a reloaded page restores itself.
  var HIDDEN_GRACE_MS_PHONE = 60000;
  var hiddenStopTimer = null;
  // stopped is true between goIdle and the next goActive: the page has no socket
  // and does no work at all.
  var stopped = false;

  function goIdle() {
    if (stopped) { return; }
    stopped = true;
    document.documentElement.setAttribute('data-idle', '1');
    // The "running" state and its clock come back with the snapshot.
    stopTurnTimer();
    // The pending markdown passes are kept: the log may well survive the trip (a
    // reconnect with nothing to report is answered with history_same, which keeps
    // it), and the log has to be complete when the page is looked at again — it is
    // flushed in one batch as the page comes back (see flushMarkdownNow). No timer
    // of this page runs while it is hidden and stopped.
    if (reconnectTimer) { clearTimeout(reconnectTimer); reconnectTimer = null; }
    if (ws) { try { ws.close(); } catch (err) { /* already closed */ } }
  }

  // goActive puts the page back to work: the animation resumes, a stopped page
  // dials again (a fresh connection means a fresh snapshot), and the rhythm the
  // foreground asks for is armed. The markdown passes that queued up while nobody
  // was looking are done now, in one batch (see flushMarkdownNow): the page is
  // about to be read again, and a queue left to the idle slices would upgrade rows
  // under the reader's eyes for as long as it lasts.
  function goActive() {
    var wasStopped = stopped;
    stopped = false;
    document.documentElement.removeAttribute('data-idle');
    reconnectDelay = 0;
    // The dial comes first, so a long catch-up cannot delay it: a reconnect that
    // brings a fresh snapshot replaces the rows the pass upgrades anyway, and one
    // answered with history_same needs the log complete as it stands.
    if (wasStopped && AUTH().ok()) { connect(); }
    flushMarkdownNow();
    restartTurnTimer();
  }

  function onPageStateChange() {
    if (hiddenStopTimer) { clearTimeout(hiddenStopTimer); hiddenStopTimer = null; }
    if (visible()) { goActive(); return; }
    // In the background the page keeps working — a desktop for good, a phone
    // until its grace period is over — at the slow rhythm renderIntervalMs and
    // turnTickMs pick up from visible(); only a running clock has to be re-armed
    // here.
    restartTurnTimer();
    if (onPhone()) {
      hiddenStopTimer = setTimeout(goIdle, HIDDEN_GRACE_MS_PHONE);
    }
  }

  document.addEventListener('visibilitychange', onPageStateChange);
  // Safari and Chrome's back-forward cache hand a page over without a
  // visibilitychange, and a discarded tab is frozen before being dropped: those
  // paths mean the page is going away whatever the device, so they stop it.
  window.addEventListener('pagehide', goIdle);
  window.addEventListener('pageshow', goActive);
  window.addEventListener('freeze', goIdle);
  window.addEventListener('resume', goActive);

  // ---- attachments ----
  // An attached file is uploaded to the mirror as soon as it is picked (the
  // server stores it under .lightagent/uploads and answers with an id) and then
  // waits in the composer until a message carries it: the chip can be removed
  // again before sending, which also drops the stored file. The next message
  // travels with the ids of whatever is still attached, and the server hands
  // those files to the model together with the text.
  //
  // The control only exists when the model declares the media types it accepts
  // (openai.media_types); MEDIA carries what the server injected into the page.
  var MEDIA = CFG.media || {};
  var mediaEnabled = !!MEDIA.enabled;
  var MAX_MEDIA_BYTES = MEDIA.max_bytes || 0;
  // pendingFiles holds the chips in the order they were picked. An entry is
  // {name, type, size, state, id, error}: state is 'uploading', 'ready' or
  // 'error', and only a ready entry has an id to send.
  var pendingFiles = [];

  function formatSize(bytes) {
    if (!bytes) { return '0 B'; }
    var units = ['B', 'KiB', 'MiB', 'GiB'];
    var value = bytes;
    var i = 0;
    while (value >= 1024 && i < units.length - 1) { value /= 1024; i++; }
    return (i === 0 ? String(value) : value.toFixed(1)) + ' ' + units[i];
  }

  // renderAttachments redraws the chip row from pendingFiles.
  function renderAttachments() {
    if (!attachmentsEl) { return; }
    attachmentsEl.innerHTML = '';
    attachmentsEl.hidden = pendingFiles.length === 0;
    pendingFiles.forEach(function (item) {
      var li = document.createElement('li');
      li.className = 'attachment'
        + (item.state === 'uploading' ? ' busy' : '')
        + (item.state === 'error' ? ' bad' : '');
      var name = document.createElement('span');
      name.className = 'attachment-name';
      name.textContent = item.name;
      li.appendChild(name);
      var meta = document.createElement('span');
      meta.className = 'attachment-meta';
      if (item.state === 'uploading') { meta.textContent = 'uploading…'; }
      else if (item.state === 'error') { meta.textContent = item.error; }
      else { meta.textContent = (item.type ? item.type + ' · ' : '') + formatSize(item.size); }
      meta.title = meta.textContent;
      li.appendChild(meta);
      var x = document.createElement('button');
      x.type = 'button';
      x.className = 'attachment-x';
      x.textContent = '✕';
      x.title = 'remove this attachment';
      x.setAttribute('aria-label', 'remove ' + item.name);
      x.onclick = function () { cancelAttachment(item); };
      li.appendChild(x);
      attachmentsEl.appendChild(li);
    });
  }

  // ---- the files a message carried ----
  // A user message that brought files along is drawn with them: the picture
  // itself when the mirror serves it (a file in its upload directory, see
  // /api/media), a chip with the file's name otherwise — a file the mirror does
  // not store (the model's own upload_media of a file elsewhere), a type the
  // browser cannot render, or a picture whose file is gone (the <img> fails and
  // the chip takes its place).

  // mediaList builds the block of files for one row, or null when there are none.
  function mediaList(attachments) {
    if (!attachments || !attachments.length) { return null; }
    var list = document.createElement('ul');
    list.className = 'attachments';
    attachments.forEach(function (item) { list.appendChild(mediaItem(item)); });
    return list;
  }

  // mediaItem draws one attachment as a list item: a framed picture that opens
  // the file in its own tab, or a chip when there is nothing to show.
  function mediaItem(item) {
    if (!isImageAttachment(item)) { return mediaChip(item); }
    var li = document.createElement('li');
    li.className = 'media-image';
    var link = document.createElement('a');
    link.href = item.url;
    link.target = '_blank';
    link.rel = 'noopener';
    link.title = 'open ' + attachmentName(item);
    var img = document.createElement('img');
    img.src = item.url;
    img.alt = attachmentName(item);
    link.appendChild(img);
    li.appendChild(link);
    // The file may be gone by now (it was deleted after the row was recorded):
    // the page cannot show the picture, so the row falls back to naming it.
    img.onerror = function () {
      if (li.parentNode) { li.parentNode.replaceChild(mediaChip(item), li); }
    };
    return li;
  }

  // mediaChip names one attachment: its name, its type, and a link to the file
  // when the mirror serves it.
  function mediaChip(item) {
    var li = document.createElement('li');
    li.className = 'attachment';
    var label = document.createElement('span');
    label.className = 'attachment-name';
    label.textContent = attachmentName(item);
    if (item.url) {
      var link = document.createElement('a');
      link.href = item.url;
      link.target = '_blank';
      link.rel = 'noopener';
      link.title = 'open ' + attachmentName(item);
      link.appendChild(label);
      li.appendChild(link);
    } else {
      li.appendChild(label);
    }
    if (item.type) {
      var meta = document.createElement('span');
      meta.className = 'attachment-meta';
      meta.textContent = item.type;
      li.appendChild(meta);
    }
    return li;
  }

  // isImageAttachment reports whether the page can draw the file as a picture:
  // the mirror has to serve it and its type has to be an image.
  function isImageAttachment(item) {
    return !!item.url && (item.type || '').indexOf('image/') === 0;
  }

  function attachmentName(item) {
    return item.name || 'attachment';
  }

  // setRowMedia adds the files a message carried to a row that is already drawn
  // (a pending row the agent has just confirmed), and does nothing when the row
  // carries them already.
  function setRowMedia(row, attachments) {
    if (!row || !attachments || !attachments.length) { return; }
    if (row.querySelector('.attachments')) { return; }
    var list = mediaList(attachments);
    if (list) { row.appendChild(list); }
  }

  // readyAttachmentIds lists the stored uploads the next message carries.
  function readyAttachmentIds() {
    var ids = [];
    pendingFiles.forEach(function (item) {
      if (item.state === 'ready' && item.id) { ids.push(item.id); }
    });
    return ids;
  }

  // cancelAttachment removes a chip before sending. A file that already reached
  // the server is deleted there too, so a cancelled attachment does not pile up
  // in the upload directory.
  function cancelAttachment(item) {
    var at = pendingFiles.indexOf(item);
    if (at >= 0) { pendingFiles.splice(at, 1); }
    renderAttachments();
    if (item.id) { dropUpload(item.id); }
  }

  // dropUpload deletes one stored upload; a failure only costs a left-over file.
  function dropUpload(id) {
    fetch('/api/upload?id=' + encodeURIComponent(id), { method: 'DELETE', credentials: 'same-origin' })
      .catch(function () {});
  }

  // clearAttachments drops the chips a message just consumed, keeping the ones
  // that never made it (a failed upload is still worth showing).
  function clearAttachments() {
    pendingFiles = pendingFiles.filter(function (item) { return item.state !== 'ready'; });
    renderAttachments();
  }

  // uploadJSON resolves an upload response, turning a non-2xx reply into an error
  // carrying the server's own message (which is written for the user).
  function uploadJSON(res) {
    return res.json().catch(function () { return {}; }).then(function (body) {
      if (!res.ok) { throw new Error(body.error || ('HTTP ' + res.status)); }
      return body;
    });
  }


  function attachFiles(files) {
    for (var i = 0; i < files.length; i++) { uploadFile(files[i]); }
  }

  // uploadFile stores one picked file and turns its chip into a ready one.
  function uploadFile(file) {
    var item = { name: file.name, type: file.type || '', size: file.size, state: 'uploading', id: '', error: '' };
    if (MAX_MEDIA_BYTES && file.size > MAX_MEDIA_BYTES) {
      // The server would refuse it anyway; say so without the round trip.
      item.state = 'error';
      item.error = 'larger than ' + formatSize(MAX_MEDIA_BYTES);
      pendingFiles.push(item);
      renderAttachments();
      return;
    }
    pendingFiles.push(item);
    renderAttachments();
    var body = new FormData();
    body.append('file', file, file.name);
    fetch('/api/upload', { method: 'POST', credentials: 'same-origin', body: body })
      .then(uploadJSON)
      .then(function (doc) {
        if (pendingFiles.indexOf(item) < 0) {
          // Cancelled while it was still uploading: the stored file goes too.
          if (doc && doc.id) { dropUpload(doc.id); }
          return;
        }
        item.id = doc.id || '';
        if (doc.name) { item.name = doc.name; }
        if (doc.type) { item.type = doc.type; }
        if (typeof doc.size === 'number') { item.size = doc.size; }
        item.state = 'ready';
        renderAttachments();
      })
      .catch(function (err) {
        if (pendingFiles.indexOf(item) < 0) { return; }
        item.state = 'error';
        item.error = (err && err.message) || 'upload failed';
        renderAttachments();
      });
  }

  // The attach control is wired only when the server says the capability is on.
  // The picker offers exactly the accepted types (its accept attribute); the
  // server refuses anything else anyway.
  if (mediaEnabled && attachEl && attachFileEl) {
    attachEl.hidden = false;
    attachEl.title = 'attach a file'
      + (MEDIA.types && MEDIA.types.length ? ' (' + MEDIA.types.join(', ') + ')' : '');
    attachFileEl.accept = MEDIA.accept || '';
    attachEl.onclick = function () { attachFileEl.click(); };
    attachFileEl.addEventListener('change', function () {
      var files = attachFileEl.files || [];
      if (files.length) { attachFiles(files); }
      // Clearing makes the same file pickable again after a removal.
      attachFileEl.value = '';
    });
  }

  // send submits the composer's content: its text plus the attachments still
  // pending (their ids — the server reads the stored files and hands them to the
  // model with this very message). A command line is a command, not a prompt, so
  // it leaves the attachments where they are.
  function send() {
    var text = input.value.trim();
    if (!ws || ws.readyState !== 1) { return; }
    var isCommand = text.charAt(0) === '/';
    var ids = isCommand ? [] : readyAttachmentIds();
    // An attachment alone is a message too (the server writes a placeholder line
    // for the row); an empty command line is not.
    if (!text && ids.length === 0) { return; }
    input.value = '';
    autoGrow();
    // Sending is an explicit "show me what comes next" action: follow again and
    // re-pin the view (which also makes the pending user row count as
    // at-the-bottom) even when the reader had scrolled back through history.
    setFollowing(true);
    pinBottom();
    // A message sent while a turn is running joins it (steering): its row is drawn
    // right away, marked pending, and the running reply keeps streaming above it
    // until the agent sends it (addPendingRow / settlePendingRow).
    if (running) {
      addPendingRow(text);
      queued++;
      tickTurn();
    }
    var payload = { text: text };
    if (ids.length > 0) { payload.attachments = ids; }
    ws.send(JSON.stringify(payload));
    // The files just sent belong to the conversation now: their chips go.
    if (ids.length > 0) { clearAttachments(); }
  }
  // autoGrow keeps the composer one row tall until the message wraps, then it
  // grows up to the CSS max-height and scrolls.
  function autoGrow() {
    input.style.height = 'auto';
    input.style.height = input.scrollHeight + 'px';
  }
  // The composer's placeholder is short by nature of the box it sits in, but on
  // a phone even the short form is the whole line: a touch keyboard has no
  // Ctrl+Enter either, so the phone-width layout gets data-placeholder-short
  // ("Message…") and the full hint stays in the textarea's title. The shared
  // phoneQuery above follows the stylesheet's breakpoint (see the phone media
  // query in app.css), so a rotation or a resize swaps the hint back and forth.
  var PLACEHOLDER_LONG = input.placeholder;
  var PLACEHOLDER_SHORT = input.getAttribute('data-placeholder-short') || PLACEHOLDER_LONG;
  function setComposerPlaceholder() {
    input.placeholder = (phoneQuery && phoneQuery.matches) ? PLACEHOLDER_SHORT : PLACEHOLDER_LONG;
  }
  if (phoneQuery) {
    if (phoneQuery.addEventListener) { phoneQuery.addEventListener('change', setComposerPlaceholder); }
    else if (phoneQuery.addListener) { phoneQuery.addListener(setComposerPlaceholder); }
  }
  setComposerPlaceholder();
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

// Read-aloud (TTS) for the web mirror.
//
// The output is spoken by the browser itself: the Web Speech API
// (window.speechSynthesis + SpeechSynthesisUtterance, the "Chrome TTS API")
// turns text into audio locally, so nothing is uploaded and the server is not
// involved. Every switch lives in localStorage, which makes the feature a
// per-browser setting that survives a reload (a phone and a desktop can read
// differently, and neither disturbs the other).
//
// Two ways to pick what is read:
//
//   final  - one reading per turn: the finalized text of the last assistant
//            message, spoken when the turn ends (turn_done). That is the
//            summary of the round: what the reply ends up saying.
//   custom - read the transcript rows as they arrive, each one opted in:
//            the thinking block (read once it is finalized), the name of every
//            tool call, and the text of every assistant message.
//
// app.js feeds the live events in through window.TTS.event() and skips the
// replay of a history frame, so reloading the page never reads the whole
// conversation aloud; TTS.reset() stops the voice and drops the buffers when the
// log is rebuilt (a reconnect, or /new clearing it).
(function () {
  var STORE_KEY = 'lightagent.tts';

  // Text is spoken in sentence-sized chunks: some Chrome voices cut a long
  // utterance short, and a whole answer in one utterance cannot be interrupted
  // cleanly. MAX_QUEUE caps the backlog, dropping the oldest chunks, so a live
  // conversation stays close to what is on screen.
  var MAX_CHUNK = 180;
  var MAX_QUEUE = 60;
  // Chrome may ignore a speak() that shares a tick with cancel(), so the queue
  // waits a moment after a stop before it starts the next chunk.
  var CANCEL_QUIET_MS = 120;
  // Offered while the browser has not reported any voice yet (Chrome fills the
  // list asynchronously, see loadVoices).
  var FALLBACK_LANGS = ['zh-CN', 'zh-TW', 'en-US', 'en-GB', 'ja-JP', 'ko-KR', 'fr-FR', 'de-DE', 'es-ES', 'pt-BR', 'ru-RU', 'it-IT'];
  // Spoken by the panel's Test button, in whatever voice is selected.
  var TEST_PHRASE = 'lightagent read-aloud test.';

  // ---- settings: one set per browser ----

  var DEFAULTS = { on: false, lang: '', voice: '', mode: 'final', thinking: true, tools: true, text: true };
  var state = loadSettings();

  function loadSettings() {
    var out = {};
    for (var key in DEFAULTS) { out[key] = DEFAULTS[key]; }
    var raw = null;
    try { raw = window.localStorage.getItem(STORE_KEY); } catch (e) { raw = null; }
    if (!raw) { return out; }
    try {
      var saved = JSON.parse(raw);
      if (saved && typeof saved === 'object') {
        if (typeof saved.on === 'boolean') { out.on = saved.on; }
        if (typeof saved.lang === 'string') { out.lang = saved.lang; }
        if (typeof saved.voice === 'string') { out.voice = saved.voice; }
        if (saved.mode === 'custom' || saved.mode === 'final') { out.mode = saved.mode; }
        if (typeof saved.thinking === 'boolean') { out.thinking = saved.thinking; }
        if (typeof saved.tools === 'boolean') { out.tools = saved.tools; }
        if (typeof saved.text === 'boolean') { out.text = saved.text; }
      }
    } catch (e) { /* an unreadable entry falls back to the defaults */ }
    return out;
  }

  function saveSettings() {
    try { window.localStorage.setItem(STORE_KEY, JSON.stringify(state)); } catch (e) { /* private mode */ }
  }

  // ---- speech queue ----

  var synth = window.speechSynthesis || null;
  // Without the utterance constructor there is nothing to queue; the panel says
  // so instead of pretending to read.
  if (!synth || typeof window.SpeechSynthesisUtterance !== 'function') { synth = null; }
  // A browser without the API never reads: the switch is forced off whatever the
  // stored preference says. The preference itself is left alone, so a browser
  // that can speak still remembers the choice.
  if (!synth) { state.on = false; }
  var voices = [];
  var queue = [];
  var speaking = false;
  // gen invalidates the callbacks of cancelled utterances: only the handlers of
  // the current generation may advance the queue.
  var gen = 0;
  var stoppedAt = 0;

  function findVoice(uri) {
    for (var i = 0; i < voices.length; i++) {
      if ((voices[i].voiceURI || '') === uri) { return voices[i]; }
    }
    return null;
  }

  // stop clears the queue and cancels whatever is being spoken.
  function stop() {
    gen++;
    queue = [];
    speaking = false;
    stoppedAt = Date.now();
    if (synth) { try { synth.cancel(); } catch (e) { /* nothing to cancel */ } }
  }

  // speak queues text for reading. The event rules below and the panel's Test
  // button call it, so it does not check whether reading is enabled.
  function speak(text) {
    if (!synth) { return; }
    var chunks = chunkify(plainText(text));
    if (chunks.length === 0) { return; }
    var overflow = queue.length + chunks.length - MAX_QUEUE;
    if (overflow > 0) { queue.splice(0, overflow); }
    for (var i = 0; i < chunks.length; i++) { queue.push(chunks[i]); }
    pump();
  }

  // pump starts the next chunk unless one is already being spoken.
  function pump() {
    if (!synth || speaking || queue.length === 0) { return; }
    var wait = CANCEL_QUIET_MS - (Date.now() - stoppedAt);
    if (wait > 0) { setTimeout(pump, wait); return; }
    var mine = gen;
    var utterance = new SpeechSynthesisUtterance(queue.shift());
    applyVoice(utterance);
    utterance.onend = utterance.onerror = function () {
      if (mine !== gen) { return; }
      speaking = false;
      pump();
    };
    speaking = true;
    try { synth.speak(utterance); } catch (e) { speaking = false; }
  }

  // applyVoice points an utterance at the selected voice. Without one the
  // utterance carries a language only and the browser picks the voice for it.
  function applyVoice(utterance) {
    var voice = state.voice ? findVoice(state.voice) : null;
    if (voice) {
      utterance.voice = voice;
      if (voice.lang) { utterance.lang = voice.lang; }
    } else if (state.lang) {
      utterance.lang = state.lang;
    }
  }

  // ---- text preparation ----

  // plainText strips the markdown the transcript renders: words and punctuation
  // read fine, "**", "```" and table pipes do not. Fenced code blocks are
  // dropped whole (nobody wants a script read out loud).
  function plainText(markdown) {
    var text = String(markdown == null ? '' : markdown);
    text = text.replace(/```[\s\S]*?(?:```|$)/g, ' ');
    text = text.replace(/~~~[\s\S]*?(?:~~~|$)/g, ' ');
    text = text.replace(/`([^`]*)`/g, '$1');
    text = text.replace(/!\[[^\]]*\]\([^)]*\)/g, ' ');
    text = text.replace(/\[([^\]]*)\]\([^)]*\)/g, '$1');
    text = text.replace(/^\s{0,3}#{1,6}\s+/gm, '');
    text = text.replace(/^\s{0,3}>\s?/gm, '');
    text = text.replace(/^\s{0,3}(?:[-*+]|\d+[.)])\s+/gm, '');
    // An underscore between word characters separates words rather than marking
    // emphasis, so "max_lines" is read as two words.
    text = text.replace(/(\w)_(?=\w)/g, '$1 ');
    // Emphasis markers: the wrappers are dropped (so "**42**." keeps its full
    // stop attached) and whatever is left over — an unbalanced marker — goes
    // away without leaving a gap in the words.
    text = text.replace(/\*\*([^*]+)\*\*/g, '$1');
    text = text.replace(/__([^_]+)__/g, '$1');
    text = text.replace(/~~([^~]+)~~/g, '$1');
    text = text.replace(/\*([^*]+)\*/g, '$1');
    text = text.replace(/_([^_]+)_/g, '$1');
    text = text.replace(/[*_~]/g, '');
    // A table's separator row (|---|---|) has no words in it.
    text = text.replace(/^\s*\|?[-:\s|—]+\|[-\s|:]*$/gm, ' ');
    text = text.replace(/\|/g, ' ');
    text = text.replace(/^\s*(?:-{3,}|\*{3,}|_{3,})\s*$/gm, ' ');
    return text.replace(/[ \t]+/g, ' ').replace(/\n{2,}/g, '\n').trim();
  }

  // chunkify splits text into sentences (Chinese and Latin punctuation plus line
  // breaks) and cuts anything longer than MAX_CHUNK at the last space.
  function chunkify(text) {
    // A Latin full stop ends a sentence only when whitespace follows, so
    // "config.json" and "3.14" stay whole; the break becomes a newline, which
    // the tokenizer below already treats as one.
    var pieces = String(text).replace(/\.(\s)/g, '.\n').match(/[^。！？!?；;\n]+[。！？!?；;\n]*/g) || [];
    var chunks = [];
    var buffer = '';
    for (var i = 0; i < pieces.length; i++) {
      var piece = pieces[i].trim();
      if (!piece) { continue; }
      while (piece.length > MAX_CHUNK) {
        if (buffer) { chunks.push(buffer); buffer = ''; }
        var cut = piece.lastIndexOf(' ', MAX_CHUNK);
        if (cut <= 0) { cut = MAX_CHUNK; }
        chunks.push(piece.slice(0, cut).trim());
        piece = piece.slice(cut);
      }
      if (buffer && buffer.length + piece.length + 1 > MAX_CHUNK) { chunks.push(buffer); buffer = ''; }
      // Sentences are joined with a space: the tokenizer ate the whitespace the
      // boundary was written with.
      buffer = buffer ? buffer + ' ' + piece : piece;
    }
    if (buffer) { chunks.push(buffer); }
    return chunks;
  }

  // ---- reading rules ----
  //
  // The transcript's event stream drives the voice. thinking buffers a reasoning
  // block until it is finalized (the same rule app.js uses for its row: any
  // event that is not another reasoning chunk ends it) and lastText remembers
  // the newest assistant message, which is what a turn ends with.

  var thinking = '';
  var lastText = '';

  function readThinking() { return state.mode === 'custom' && state.thinking; }

  function flushThinking() {
    if (!thinking) { return; }
    var block = thinking;
    thinking = '';
    if (readThinking()) { speak(block); }
  }

  function onEvent(kind, ev) {
    if (!state.on) { return; } // off: nothing is buffered at all
    if (kind === 'reasoning_delta') {
      if (readThinking()) { thinking += (ev && ev.text) || ''; }
      return;
    }
    if (kind === 'user') {
      // A new turn (or a steering message) makes whatever is still queued stale.
      stop();
      thinking = '';
      lastText = '';
      return;
    }
    if (kind === 'interrupted') {
      stop();
      thinking = '';
      lastText = '';
      return;
    }
    // Any content event ends a thinking block; usage carries none, so it must
    // not cut one short.
    if (kind !== 'usage') { flushThinking(); }
    if (kind === 'assistant') {
      lastText = (ev && ev.text) || '';
      if (state.mode === 'custom' && state.text) { speak(lastText); }
      return;
    }
    if (kind === 'tool_call') {
      // Only the name is read; the arguments are for the eye.
      if (state.mode === 'custom' && state.tools && ev && ev.name) { speak(ev.name); }
      return;
    }
    if (kind === 'turn_done') {
      // The round's final text is the last assistant message of the turn.
      if (state.mode === 'final') { speak(lastText); }
      lastText = '';
    }
  }

  // reset drops the buffers and silences the voice: the log they described was
  // replaced by a history frame.
  function reset() {
    stop();
    thinking = '';
    lastText = '';
  }

  // app.js drives the feature through these two calls.
  window.TTS = { event: onEvent, reset: reset, stop: stop };

  // ---- panel ----
  //
  // The controls are pure UI over the state above: every change writes state,
  // saves it to localStorage and repaints, so a reload or a second tab always
  // shows what is stored.

  var modal = document.getElementById('ttsModal');
  var panel = document.getElementById('ttsPanel');
  var onEl = document.getElementById('ttsOn');
  var langEl = document.getElementById('ttsLang');
  var picksEl = document.getElementById('ttsPicks');
  var statusEl = document.getElementById('ttsStatus');
  var testEl = document.getElementById('ttsTest');
  var stopEl = document.getElementById('ttsStop');
  // The enable switch exists twice: in the rail card (the quick way in on
  // desktop) and in the panel. Both drive the same state and are painted
  // together, so neither can show something the other does not.
  var masterEls = [onEl, document.getElementById('ttsRailOn')];
  var modeEls = document.querySelectorAll('input[name=ttsMode]');
  var boxEls = {
    thinking: document.getElementById('ttsThinking'),
    tools: document.getElementById('ttsTools'),
    text: document.getElementById('ttsText')
  };
  var openers = document.querySelectorAll('[data-tts-open]');
  var closers = document.querySelectorAll('[data-tts-close]');
  // optionInfo maps a picker value (a voice URI, or a bare language tag while
  // the voice list is still empty) to the language and voice it stands for.
  var optionInfo = {};

  // loadVoices fills the voice list and rebuilds the language picker. Chrome
  // returns an empty list on the first call and announces the real one with
  // voiceschanged, so this runs again when that fires.
  function loadVoices() {
    if (synth && typeof synth.getVoices === 'function') { voices = synth.getVoices() || []; }
    else { voices = []; }
    if (langEl) { buildLanguageOptions(); }
  }

  function addLanguageOption(value, label) {
    var opt = document.createElement('option');
    opt.value = value;
    opt.textContent = label;
    langEl.appendChild(opt);
    optionInfo[value] = value ? { lang: value, voice: '' } : { lang: '', voice: '' };
  }

  function buildLanguageOptions() {
    optionInfo = {};
    langEl.innerHTML = '';
    addLanguageOption('', 'System default');
    if (voices.length === 0) {
      // No voice reported (yet): the language can still be chosen, and the
      // browser picks one of its own voices for it.
      for (var f = 0; f < FALLBACK_LANGS.length; f++) { addLanguageOption(FALLBACK_LANGS[f], FALLBACK_LANGS[f]); }
      return;
    }
    // Group the voices by language, so choosing a language and choosing the
    // exact voice are the same gesture (Chrome's "Google 普通话" shows up under
    // zh-CN, and an online-only voice is marked).
    var groups = {};
    var order = [];
    for (var i = 0; i < voices.length; i++) {
      var lang = voices[i].lang || 'unknown';
      if (!groups[lang]) { groups[lang] = []; order.push(lang); }
      groups[lang].push(voices[i]);
    }
    for (var g = 0; g < order.length; g++) {
      var group = document.createElement('optgroup');
      group.label = order[g];
      for (var v = 0; v < groups[order[g]].length; v++) {
        var voice = groups[order[g]][v];
        var value = voice.voiceURI || (voice.lang + '|' + voice.name);
        var opt = document.createElement('option');
        opt.value = value;
        opt.textContent = voice.name + (voice.localService === false ? ' (online)' : '');
        group.appendChild(opt);
        optionInfo[value] = { lang: voice.lang || '', voice: value };
      }
      langEl.appendChild(group);
    }
  }
  // restoredValue maps the stored setting onto the options at hand: the exact
  // voice when it is still installed, otherwise any voice of the stored
  // language, otherwise nothing (the browser default).
  function restoredValue() {
    if (state.voice && optionInfo[state.voice]) { return state.voice; }
    if (state.lang) {
      if (optionInfo[state.lang]) { return state.lang; }
      for (var value in optionInfo) {
        if (optionInfo[value].lang === state.lang) { return value; }
      }
    }
    return '';
  }

  // syncStateFromSelect turns the picker's value back into the stored language
  // and voice.
  function syncStateFromSelect() {
    var info = optionInfo[langEl.value];
    if (info) { state.lang = info.lang; state.voice = info.voice; return; }
    state.lang = langEl.value;
    state.voice = '';
  }

  // paintOpeners lights up the header and rail entries while reading is on: it
  // is the only cue outside the panel that the page will talk.
  function paintOpeners() {
    for (var i = 0; i < openers.length; i++) {
      if (state.on) { openers[i].classList.add('on'); } else { openers[i].classList.remove('on'); }
    }
  }

  // describe names the current selection in the panel's own words.
  function describe() {
    if (state.mode === 'final') { return 'the final text of each round'; }
    var parts = [];
    if (state.thinking) { parts.push('the thinking process'); }
    if (state.tools) { parts.push('tool names'); }
    if (state.text) { parts.push('reply text'); }
    if (parts.length === 0) { return 'nothing — tick at least one row below'; }
    return parts.join(', ');
  }

  function setStatus() {
    if (!statusEl) { return; }
    if (!synth) {
      statusEl.textContent = 'This browser has no speech synthesis (Web Speech API).';
      statusEl.className = 'modal-note bad';
      return;
    }
    if (!state.on) {
      statusEl.textContent = 'Off — nothing is read aloud. The switches stay in this browser.';
      statusEl.className = 'modal-note';
      return;
    }
    statusEl.textContent = 'Reading ' + describe() + '.';
    statusEl.className = 'modal-note ok';
  }

  // reflect paints every control from the state; it runs after each change and
  // once at startup.
  function reflect() {
    for (var s = 0; s < masterEls.length; s++) {
      if (masterEls[s]) { masterEls[s].checked = state.on; }
    }
    langEl.value = restoredValue();
    for (var i = 0; i < modeEls.length; i++) { modeEls[i].checked = (modeEls[i].value === state.mode); }
    if (boxEls.thinking) { boxEls.thinking.checked = state.thinking; }
    if (boxEls.tools) { boxEls.tools.checked = state.tools; }
    if (boxEls.text) { boxEls.text.checked = state.text; }
    if (picksEl) { picksEl.className = state.mode === 'custom' ? 'tts-picks' : 'tts-picks off'; }
    setStatus();
    paintOpeners();
  }

  function changed() {
    saveSettings();
    reflect();
  }

  function openPanel() {
    modal.hidden = false;
    if (panel) { panel.focus(); }
    reflect();
  }

  function closePanel() {
    if (!modal.hidden) { modal.hidden = true; }
  }

  // boxHandler stores one tick box of the custom selection.
  function boxHandler(key) {
    return function () {
      state[key] = boxEls[key].checked;
      changed();
    };
  }

  // setEnabled flips reading on or off from either master switch; switching off
  // also silences whatever is still queued.
  function setEnabled(on) {
    state.on = !!on;
    if (!state.on) { reset(); }
    changed();
  }

  function initPanel() {
    if (!synth) {
      // Nothing can be spoken here: the panel explains it and the controls stay
      // inert, with the switch off (see the API check above the panel).
      var inert = masterEls.concat([langEl, testEl, stopEl, boxEls.thinking, boxEls.tools, boxEls.text]);
      for (var d = 0; d < inert.length; d++) { if (inert[d]) { inert[d].disabled = true; } }
      for (var r = 0; r < modeEls.length; r++) { modeEls[r].disabled = true; }
    }
    for (var s = 0; s < masterEls.length; s++) {
      if (masterEls[s]) { masterEls[s].onchange = function () { setEnabled(this.checked); }; }
    }
    langEl.onchange = function () { syncStateFromSelect(); changed(); };
    for (var m = 0; m < modeEls.length; m++) {
      modeEls[m].onchange = function () { state.mode = this.value; changed(); };
    }
    for (var key in boxEls) {
      if (boxEls[key]) { boxEls[key].onchange = boxHandler(key); }
    }
    if (testEl) { testEl.onclick = function () { stop(); speak(TEST_PHRASE); }; }
    if (stopEl) { stopEl.onclick = stop; }
    for (var o = 0; o < openers.length; o++) { openers[o].onclick = openPanel; }
    for (var c = 0; c < closers.length; c++) { closers[c].onclick = closePanel; }
    // Escape closes the panel, like the config editor (its own listener only
    // reacts while that modal is open).
    document.addEventListener('keydown', function (e) {
      if (e.key === 'Escape') { closePanel(); }
    });
    reflect();
  }

  if (synth) {
    loadVoices();
    if (typeof synth.addEventListener === 'function') {
      synth.addEventListener('voiceschanged', loadVoices);
    } else if ('onvoiceschanged' in synth) {
      synth.onvoiceschanged = loadVoices;
    }
  }
  if (modal) { initPanel(); }
})();

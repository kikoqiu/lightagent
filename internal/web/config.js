// Config editor for the web mirror.
//
// config.json is edited in one of two modes, both working on the same document:
//
//   Form - one card per section, one row per option, with real controls (text,
//          number, slider, switch, select) and per-field help + validation.
//   JSON - the raw document in a textarea, for anything the form does not model
//          (and for copy/paste between machines).
//
// Treating the parsed document as the single source of truth is what keeps the
// modes in sync: a control writes into the document as soon as it changes, so
// switching to JSON always shows exactly what a save would send, and switching
// back re-reads the textarea (making JSON the authority for that edit).
//
// Saving never touches the running agent: /api/config only writes the file, so
// the panel always says that a restart is what applies a change.
(function () {
  var modal = document.getElementById('configModal');
  var form = document.getElementById('configForm');
  var json = document.getElementById('configText');
  var panel = document.getElementById('configPanel');
  var pathEl = document.getElementById('configPath');
  var statusEl = document.getElementById('configStatus');
  var saveEl = document.getElementById('configSave');
  var reloadEl = document.getElementById('configReload');
  var tabForm = document.getElementById('modeForm');
  var tabJSON = document.getElementById('modeJSON');
  if (!modal || !form || !json) { return; }

  // Sessions live in an HttpOnly cookie, so every request is same-origin with no
  // token to add (see auth.js).

  // draft is the document being edited; mode is 'form' or 'json'; dirty marks
  // edits that have not been saved yet (reopening the panel keeps them).
  var draft = null;
  var mode = 'form';
  var dirty = false;
  var busy = false;
  var fields = [];

  // MASK is the placeholder the server replaces every secret with (config.
  // MaskedSecret): it means "leave what the file has" when it comes back.
  var MASK = '***';

  var HINT = 'edits are saved to config.json and apply after a restart';
  var SAVED = 'saved — restart lightagent to apply it';

  // The schema: one descriptor per option of the Go struct (camelCase keys are
  // the JSON paths, dots nest). type is one of text, password, number, slider,
  // bool, select, textarea, json. help is shown under the key; advanced marks
  // free-form JSON that has no useful control shape (extra_body, servers).
  var SCHEMA = [
    {
      title: 'OpenAI', note: 'endpoint, credentials, request shape', open: true,
      fields: [
        { path: 'openai.api_base', type: 'text', help: 'base URL; the client appends /chat/completions. empty = built-in default' },
        { path: 'openai.api_key', type: 'password', help: '*** keeps the stored key — paste a new one to replace it' },
        { path: 'openai.model', type: 'text', placeholder: 'gpt-4o-mini', help: 'model name' },
        { path: 'openai.stream', type: 'bool', help: 'stream the reply over SSE' },
        { path: 'openai.temperature', type: 'slider', min: 0, max: 2, step: 0.05, fallback: 0.0, help: 'sampling temperature; 0 falls back to the default (0.0)' },
        { path: 'openai.max_tokens', type: 'number', min: 1, help: 'cap per reply' },
        { path: 'openai.timeout_seconds', type: 'number', min: 0, help: 'idle timeout in seconds (waiting for headers or between stream chunks); 0 disables it' },
        { path: 'openai.extra_body', type: 'json', advanced: true, rows: 5, help: 'provider-specific request fields, merged into the request body (overrides built-ins such as temperature)' }
      ]
    },
    {
      title: 'Context', note: 'window size and compression threshold', open: true,
      fields: [
        { path: 'context.context_window', type: 'number', min: 1, help: 'model context window in tokens; drives compression and the usage badge' },
        { path: 'context.summarize_token_percent', type: 'slider', min: 1, max: 100, step: 1, fallback: 75, help: 'compress once usage reaches this % of the window' }
      ]
    },
    {
      title: 'Web mirror', note: 'this page; changes apply on restart', open: true,
      fields: [
        { path: 'web.host', type: 'text', help: 'bind address: empty or 127.0.0.1 = loopback only, 0.0.0.0 exposes the mirror on the network' },
        { path: 'web.port', type: 'number', min: 0, help: '0 disables the mirror; a busy port falls back to port+1 … +50' },
        { path: 'web.password', type: 'secret', help: 'login password: the server keeps it and hands the browser only a public salt, so what travels and what a browser stores is a salted digest (set here: active immediately, no restart)' }
      ]
    }
  ];

  // The remaining sections are appended to the same list so the schema stays one
  // readable literal.
  SCHEMA.push(
    {
      title: 'Tools · shell', note: 'exec_command / manage_session',
      fields: [
        { path: 'tools.exec.enabled', type: 'bool', help: 'register the shell tools' },
        { path: 'tools.exec.timeout_seconds', type: 'number', min: 1, help: 'hard limit for one command' },
        { path: 'tools.exec.wait_seconds', type: 'number', min: 1, help: 'how long to wait before a command moves to a background session' },
        { path: 'tools.exec.use_utf8', type: 'bool', help: 'Windows: let the child speak UTF-8; off = convert via the host code page' }
      ]
    },
    {
      title: 'Tools · files', note: 'read_file_lines / write_file / edit_file',
      fields: [
        { path: 'tools.read_file_lines.enabled', type: 'bool', help: 'register the line reader' },
        { path: 'tools.read_file_lines.max_read_file_size', type: 'number', min: 1, help: 'file size cap in bytes' },
        { path: 'tools.read_file_lines.max_read_file_lines', type: 'number', min: 1, help: 'line cap per request' },
        { path: 'tools.write_file.enabled', type: 'bool', help: 'register write_file' },
        { path: 'tools.write_file.max_lines', type: 'number', min: 1, help: 'line cap per write; the rest is left for a follow-up call' },
        { path: 'tools.write_file.auto_split', type: 'bool', help: 'spread an oversized write over several calls instead of truncating it' },
        { path: 'tools.edit_file.enabled', type: 'bool', help: 'register edit_file' }
      ]
    },
    {
      title: 'Tools · discovery', note: 'locked tools: search, unlock, dynamic call',
      fields: [
        { path: 'tools.discovery.enabled', type: 'bool', help: 'register tool_search_tool_bm25, unlock_tool and dynamic_call' },
        { path: 'tools.discovery.mode', type: 'select', options: [{ value: 'unlock', label: 'unlock — visibility decoupled from execution' }], help: 'unlock is the only supported mode' },
        { path: 'tools.discovery.ttl', type: 'number', min: 1, help: 'how many tool calls an unlocked tool stays usable' },
        { path: 'tools.discovery.max_search_results', type: 'number', min: 1, help: 'hits reported per search' },
        { path: 'tools.discovery.min_match_rate', type: 'slider', min: 0.05, max: 1, step: 0.05, fallback: 0.5, help: 'share of the query keywords a function must contain to be reported' },
        { path: 'tools.discovery.use_bm25', type: 'bool', help: 'required by MCP: find/unlock needs the BM25 search' }
      ]
    },
    {
      title: 'Tools · MCP', note: 'external servers',
      fields: [
        { path: 'tools.mcp.enabled', type: 'bool', help: 'connect the enabled servers on startup; their tools stay locked' },
        { path: 'tools.mcp.servers', type: 'json', advanced: true, rows: 8, help: 'one entry per server: name → {enabled, type (stdio|http|sse), command, args, url, env_file, env, headers}' }
      ]
    },
    {
      title: 'Agent', note: 'turn loop and system prompt',
      fields: [
        { path: 'agent.max_tool_iterations', type: 'number', min: 1, help: 'tool rounds allowed inside one turn' },
        { path: 'agent.system_prompt', type: 'textarea', rows: 5, help: 'custom prompt; an agent.md next to config.json overrides it, and the runtime line is appended automatically' },
        { path: 'agent.include_working_dir', type: 'bool', help: 'inject the working directory listing into the system prompt' },
        { path: 'agent.summary_in_system_prompt', type: 'bool', help: 'where a request carries the compressed context summary: off (default) = as the first user message, on = in the system prompt' }
      ]
    },
    {
      title: 'UI', note: 'presentation',
      fields: [
        { path: 'ui.markdown', type: 'bool', help: 'render replies and thinking as markdown (CLI → ANSI, web → marked + DOMPurify)' }
      ]
    }
  );

  // ---- document helpers ----

  function isArray(value) { return Object.prototype.toString.call(value) === '[object Array]'; }

  function isObject(value) { return value !== null && typeof value === 'object' && !isArray(value); }

  function normalize(doc) { return isObject(doc) ? doc : {}; }

  function getPath(obj, path) {
    var parts = path.split('.');
    var cur = obj;
    for (var i = 0; i < parts.length; i++) {
      if (!isObject(cur)) { return undefined; }
      cur = cur[parts[i]];
    }
    return cur;
  }

  function setPath(obj, path, value) {
    var parts = path.split('.');
    var cur = obj;
    for (var i = 0; i < parts.length - 1; i++) {
      if (!isObject(cur[parts[i]])) { cur[parts[i]] = {}; }
      cur = cur[parts[i]];
    }
    cur[parts[parts.length - 1]] = value;
  }

  // deletePath drops a key so the built-in default applies again: that is what an
  // empty control means ("leave it unset"), which works because the server layers
  // its defaults under whatever the file declares.
  function deletePath(obj, path) {
    var parts = path.split('.');
    var cur = obj;
    for (var i = 0; i < parts.length - 1; i++) {
      if (!isObject(cur)) { return; }
      cur = cur[parts[i]];
    }
    if (isObject(cur)) { delete cur[parts[parts.length - 1]]; }
  }

  function documentText() { return JSON.stringify(draft, null, 2); }

  // parseDocument parses the raw editor text, rejecting anything that is not a
  // single JSON object (the server rejects it too, with the same wording).
  function parseDocument(text) {
    if (!String(text).trim()) { throw new Error('the document is empty'); }
    var parsed = JSON.parse(text);
    if (!isObject(parsed)) { throw new Error('the document must be a JSON object'); }
    return parsed;
  }

  // ---- HTTP ----

  // readJSON resolves a response body, turning a non-2xx reply into an error that
  // carries the server's own message (validation errors are meant for the user).
  // A 401 means the session is gone: the page asks for a new sign-in.
  function readJSON(res) {
    return res.json().catch(function () { return {}; }).then(function (body) {
      if (res.status === 401 && window.AUTH) { window.AUTH.unauthorized(); }
      if (!res.ok) { throw new Error(body.error || ('HTTP ' + res.status)); }
      return body;
    });
  }

  // friendly explains the failures a user can fix from here.
  function friendly(message) {
    var unknown = /unknown field "([^"]+)"/.exec(message);
    if (unknown) { return message + ' — open JSON mode and delete ' + unknown[1]; }
    if (message === 'openai.api_key is empty; edit the config file and try again') {
      return 'openai.api_key is empty: paste a key in the OpenAI section';
    }
    return message;
  }

  function setStatus(text, kind) {
    if (!statusEl) { return; }
    statusEl.textContent = text || '';
    statusEl.className = 'modal-note' + (kind ? ' ' + kind : '');
  }

  // ---- form rendering (built once, then only populated) ----

  function buildControl(def, id) {
    var el, out = null;
    if (def.type === 'bool') {
      el = document.createElement('input');
      el.type = 'checkbox';
      el.className = 'switch';
    } else if (def.type === 'select') {
      el = document.createElement('select');
      for (var i = 0; i < def.options.length; i++) {
        var option = document.createElement('option');
        option.value = def.options[i].value;
        option.textContent = def.options[i].label;
        el.appendChild(option);
      }
    } else if (def.type === 'json' || def.type === 'textarea') {
      el = document.createElement('textarea');
      el.rows = def.rows || 4;
      el.spellcheck = false;
      el.className = def.type === 'json' ? 'json-box' : 'text-box';
    } else if (def.type === 'slider') {
      el = document.createElement('input');
      el.type = 'range';
      out = document.createElement('output');
      out.className = 'slider-out';
    } else {
      el = document.createElement('input');
      el.type = def.type === 'password' ? 'password' : (def.type === 'number' ? 'number' : 'text');
      if (def.placeholder) { el.placeholder = def.placeholder; }
    }
    el.id = id;
    if (def.min !== undefined) { el.min = def.min; }
    if (def.max !== undefined) { el.max = def.max; }
    if (def.step !== undefined) { el.step = def.step; }
    return { el: el, out: out };
  }

  function setError(field, message) {
    field.bad = true;
    field.wrap.className = 'field invalid';
    field.err.textContent = message;
  }

  function clearError(field) {
    field.bad = false;
    field.wrap.className = 'field';
    field.err.textContent = '';
  }

  function renderField(def) {
    var id = 'cfg-' + def.path;
    var wrap = document.createElement('div');
    wrap.className = 'field';

    var label = document.createElement('label');
    label.className = 'field-label';
    label.setAttribute('for', id);
    var key = document.createElement('span');
    key.className = 'field-key';
    key.textContent = def.path;
    label.appendChild(key);
    if (def.advanced) {
      var tag = document.createElement('span');
      tag.className = 'tag';
      tag.textContent = 'json';
      label.appendChild(tag);
    }
    if (def.help) {
      var help = document.createElement('span');
      help.className = 'field-help';
      help.textContent = def.help;
      label.appendChild(help);
    }

    var control = document.createElement('div');
    control.className = 'field-control';
    var err = document.createElement('span');
    err.className = 'field-err';

    var record = { def: def, wrap: wrap, err: err, bad: false };
    if (def.type === 'secret') {
      // The credential is written by its own endpoint (the server keeps the
      // plaintext and hands the browser a salt, so there is no document field to
      // edit here).
      record.el = buildSecretControl(control, record, id);
    } else {
      var built = buildControl(def, id);
      record.el = built.el;
      record.out = built.out;
      control.appendChild(built.el);
      if (built.out) { control.appendChild(built.out); }
      // Both events: change covers selects, switches and sliders, input covers
      // typing.
      built.el.addEventListener('input', function () { applyField(record); });
      built.el.addEventListener('change', function () { applyField(record); });
    }
    control.appendChild(err);

    wrap.appendChild(label);
    wrap.appendChild(control);
    fields.push(record);
    return wrap;
  }

  // buildSecretControl renders the password row: a status line, a new-password
  // box and the buttons that call /api/password. It returns the input so focus
  // helpers keep working.
  function buildSecretControl(control, record, id) {
    var state = document.createElement('div');
    state.className = 'secret-state';
    var row = document.createElement('div');
    row.className = 'secret-row';
    var input = document.createElement('input');
    input.type = 'password';
    input.id = id;
    input.autocomplete = 'new-password';
    input.placeholder = 'new password';
    var set = document.createElement('button');
    set.type = 'button';
    set.className = 'ghost';
    set.textContent = 'Set';
    var clear = document.createElement('button');
    clear.type = 'button';
    clear.className = 'ghost';
    clear.textContent = 'Remove';
    row.appendChild(input);
    row.appendChild(set);
    row.appendChild(clear);
    control.appendChild(state);
    control.appendChild(row);
    record.state = state;
    set.onclick = function () { savePassword(record, false); };
    clear.onclick = function () { savePassword(record, true); };
    input.addEventListener('keydown', function (event) {
      if (event.key === 'Enter') { event.preventDefault(); savePassword(record, false); }
    });
    return input;
  }

  // savePassword posts a new password (or an empty one to turn the login off) and
  // applies it at once: unlike the rest of the document, a credential takes
  // effect immediately.
  function savePassword(field, remove) {
    var value = remove ? '' : String(field.el.value || '');
    if (!remove && !value) {
      setError(field, 'enter a new password');
      field.el.focus();
      return;
    }
    clearError(field);
    setStatus(remove ? 'removing the password…' : 'updating the password…');
    fetch('/api/password', {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ password: value })
    }).then(readJSON).then(function (doc) {
      field.el.value = '';
      draft = normalize(doc.config);
      syncForm();
      json.value = documentText();
      dirty = false;
      setStatus(doc.message || 'password updated; it is active now', 'ok');
      // The salt rotated with the password: keep this browser able to sign in by
      // itself with the value it just typed.
      if (window.AUTH && window.AUTH.remember) { window.AUTH.remember(value, doc.salt || ''); }
    }).catch(function (err) {
      setError(field, friendly(err.message));
      setStatus('password update failed: ' + friendly(err.message), 'bad');
    });
  }

  function renderForm() {
    for (var s = 0; s < SCHEMA.length; s++) {
      var section = SCHEMA[s];
      var details = document.createElement('details');
      details.className = 'sect';
      if (section.open) { details.open = true; }
      var summary = document.createElement('summary');
      var title = document.createElement('span');
      title.className = 'sect-title';
      title.textContent = section.title;
      summary.appendChild(title);
      if (section.note) {
        var note = document.createElement('span');
        note.className = 'sect-note';
        note.textContent = section.note;
        summary.appendChild(note);
      }
      details.appendChild(summary);
      var body = document.createElement('div');
      body.className = 'sect-body';
      for (var f = 0; f < section.fields.length; f++) { body.appendChild(renderField(section.fields[f])); }
      details.appendChild(body);
      form.appendChild(details);
    }
  }

  // ---- document ⇄ controls ----

  function hasOption(select, value) {
    for (var i = 0; i < select.options.length; i++) {
      if (select.options[i].value === value) { return true; }
    }
    return false;
  }

  // addOption keeps a value the schema does not offer visible instead of silently
  // dropping it: saving then fails with the server's explanation.
  function addOption(select, value) {
    var option = document.createElement('option');
    option.value = value;
    option.textContent = value + ' (unsupported)';
    select.appendChild(option);
  }

  function rangeError(def, value) {
    if (def.min !== undefined && value < def.min) { return 'must be at least ' + def.min; }
    if (def.max !== undefined && value > def.max) { return 'must be at most ' + def.max; }
    return '';
  }

  // syncForm shows the document in the controls. It runs after a load and after a
  // successful save, never while the user is typing.
  function syncForm() {
    for (var i = 0; i < fields.length; i++) {
      var field = fields[i];
      var value = getPath(draft, field.def.path);
      var empty = value === undefined || value === null;
      if (field.def.type === 'bool') {
        field.el.checked = value === true;
      } else if (field.def.type === 'secret') {
        // The plaintext never comes back from the server, so the control only
        // reports whether a login is configured and the salt it is bound to.
        if (field.state) {
          var configured = value === MASK && value !== '';
          field.state.className = 'secret-state' + (configured ? ' on' : '');
          field.state.textContent = configured
            ? 'password is set (salt ' + String(getPath(draft, 'web.password_salt') || '').slice(0, 8) + '…); browsers sign in with a salted digest'
            : 'no password: anyone who can reach this port can use the agent';
        }
        field.el.value = '';
      } else if (field.def.type === 'json') {
        field.el.value = empty ? '' : JSON.stringify(value, null, 2);
      } else if (field.def.type === 'select') {
        var want = empty ? '' : String(value);
        if (want && !hasOption(field.el, want)) { addOption(field.el, want); }
        if (want) { field.el.value = want; }
      } else if (field.def.type === 'slider') {
        field.el.value = empty ? field.def.fallback : String(value);
      } else {
        field.el.value = empty ? '' : String(value);
      }
      if (field.out) { field.out.textContent = field.el.value === '' ? 'default' : String(field.el.value); }
      clearError(field);
    }
  }

  // applyField writes one control back into the document, so a mode switch always
  // shows the edit and a save sends exactly what the form displays.
  function applyField(field) {
    var def = field.def;
    if (def.type === 'bool') {
      setPath(draft, def.path, field.el.checked);
      clearError(field);
    } else if (def.type === 'json') {
      var text = field.el.value.trim();
      if (text === '') {
        deletePath(draft, def.path);
        clearError(field);
      } else {
        try {
          var parsed = JSON.parse(text);
          if (!isObject(parsed)) { throw new Error('expected a JSON object'); }
          setPath(draft, def.path, parsed);
          clearError(field);
        } catch (err) {
          setError(field, err.message);
        }
      }
    } else if (def.type === 'number') {
      var raw = field.el.value.trim();
      if (raw === '') {
        deletePath(draft, def.path);
        clearError(field);
      } else {
        var n = Number(raw);
        var problem = !isFinite(n) ? 'enter a number'
          : (Math.floor(n) !== n ? 'enter a whole number' : rangeError(def, n));
        if (problem) { setError(field, problem); } else { setPath(draft, def.path, n); clearError(field); }
      }
    } else if (def.type === 'slider') {
      setPath(draft, def.path, Number(field.el.value));
      clearError(field);
      if (field.out) { field.out.textContent = String(field.el.value); }
    } else if (def.type === 'select') {
      setPath(draft, def.path, field.el.value);
      clearError(field);
    } else {
      setPath(draft, def.path, field.el.value);
      clearError(field);
    }
    dirty = true;
    setStatus('unsaved changes — Save writes them to config.json');
  }

  // firstBad reports the first control the server would reject, so a save never
  // sends a document the editor already knows is broken.
  function firstBad() {
    for (var i = 0; i < fields.length; i++) {
      if (fields[i].bad) { return fields[i]; }
    }
    return null;
  }

  // ---- modes ----

  function applyMode(next) {
    mode = next;
    var isForm = mode === 'form';
    form.hidden = !isForm;
    json.hidden = isForm;
    tabForm.className = isForm ? 'tab on' : 'tab';
    tabJSON.className = isForm ? 'tab' : 'tab on';
    tabForm.setAttribute('aria-selected', isForm ? 'true' : 'false');
    tabJSON.setAttribute('aria-selected', isForm ? 'false' : 'true');
  }

  function setMode(next) {
    if (next === mode) { return; }
    if (next === 'json') {
      // The form already wrote every edit into the document, so the textarea
      // shows exactly what a save would send.
      json.value = documentText();
      applyMode('json');
      setStatus(HINT);
      return;
    }
    // Back to the form: the textarea is the authority for this edit, so a broken
    // document keeps the JSON mode open instead of losing what was typed.
    try {
      draft = parseDocument(json.value);
    } catch (err) {
      setStatus('invalid JSON: ' + err.message, 'bad');
      return;
    }
    syncForm();
    applyMode('form');
    setStatus(HINT);
  }

  // ---- load / save ----

  function showDocument(doc, pending) {
    draft = normalize(doc);
    syncForm();
    json.value = documentText();
    dirty = false;
    setStatus(pending ? SAVED : HINT);
  }

  function load() {
    setStatus('loading…');
    fetch('/api/config', { credentials: 'same-origin' }).then(readJSON).then(function (doc) {
      if (pathEl) { pathEl.textContent = doc.path || ''; }
      showDocument(doc.config, doc.pending_restart);
    }).catch(function (err) { setStatus('load failed: ' + friendly(err.message), 'bad'); });
  }

  function save() {
    if (busy) { return; }
    var body;
    if (mode === 'json') {
      // Send the raw text: what is in front of the user is what gets written.
      try {
        draft = parseDocument(json.value);
      } catch (err) {
        setStatus('invalid JSON: ' + err.message, 'bad');
        return;
      }
      body = json.value;
    } else {
      var bad = firstBad();
      if (bad) {
        setStatus('fix ' + bad.def.path + ' first: ' + bad.err.textContent, 'bad');
        bad.el.focus();
        return;
      }
      body = documentText();
    }
    busy = true;
    if (saveEl) { saveEl.disabled = true; }
    setStatus('saving…');
    fetch('/api/config', {
      method: 'PUT',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: body
    }).then(readJSON).then(function (doc) {
      // The reply is the document as written (defaults filled in, api key masked),
      // so the panel ends up showing exactly what the next start will read.
      if (pathEl && doc.path) { pathEl.textContent = doc.path; }
      showDocument(doc.config, true);
      setStatus(SAVED, 'ok');
    }).catch(function (err) {
      setStatus('save failed: ' + friendly(err.message), 'bad');
    }).then(function () {
      busy = false;
      if (saveEl) { saveEl.disabled = false; }
    });
  }

  function open() {
    modal.hidden = false;
    if (draft === null) { load(); }
    else if (dirty) { setStatus('unsaved changes — Save writes them to config.json'); }
    if (panel) { panel.focus(); }
  }

  function close() {
    if (!modal.hidden) { modal.hidden = true; }
  }

  // ---- wiring ----

  var openers = document.querySelectorAll('[data-config-open]');
  for (var o = 0; o < openers.length; o++) { openers[o].onclick = open; }
  var closers = document.querySelectorAll('[data-config-close]');
  for (var c = 0; c < closers.length; c++) { closers[c].onclick = close; }
  tabForm.onclick = function () { setMode('form'); };
  tabJSON.onclick = function () { setMode('json'); };
  if (saveEl) { saveEl.onclick = save; }
  if (reloadEl) { reloadEl.onclick = load; }
  json.addEventListener('input', function () {
    dirty = true;
    setStatus('unsaved changes — Save writes them to config.json');
  });
  document.addEventListener('keydown', function (e) {
    if (e.key === 'Escape') { close(); }
  });

  renderForm();
  applyMode('form');
})();

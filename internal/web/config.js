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
  var restartEl = document.getElementById('configRestart');
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

  // The panel offers Restart only when the mirror can restart the program at all
  // (the server injects that as window.__LIGHTAGENT__.restart); an embedder that
  // runs the mirror inside its own process cannot, and then the button goes away
  // instead of failing on every click.
  var CAN_RESTART = !!((window.__LIGHTAGENT__ || {}).restart);

  // The schema: one descriptor per option of the Go struct (camelCase keys are
  // the JSON paths, dots nest). type is one of text, password, number, slider,
  // bool, select, textarea, json, list, providers. help is shown under the key;
  // advanced marks free-form JSON that has no useful control shape (extra_body,
  // servers); a list is entered as a comma separated string and saved as a JSON
  // array; providers is the ordered interface array, which the flat path model
  // cannot express and a dedicated repeater owns (see the providers repeater
  // below).
  var SCHEMA = [
    {
      title: 'Providers', note: 'one entry per endpoint; the first enabled one is used at startup', open: true,
      fields: [
        { path: 'providers', type: 'providers' }
      ]
    },
    {
      title: 'Context', note: 'compression threshold and retention', open: true,
      fields: [
        { path: 'context.summarize_token_percent', type: 'slider', min: 1, max: 100, step: 1, fallback: 75, help: 'compress once usage reaches this % of the window (the window itself is per interface — see LLM interfaces)' },
        { path: 'context.summarize_keep.auto.budget_percent', type: 'slider', min: 0, max: 100, step: 1, fallback: 0, help: 'share of the available input budget (window minus max_tokens) the automatic pass keeps raw; 0 keeps no raw message, which keeps the prompt cache valid across the rollback some engines cannot handle' },
        { path: 'context.summarize_keep.auto.turns', type: 'number', min: 0, help: 'complete turns the automatic pass keeps raw at most; 0 keeps none' },
        { path: 'context.summarize_keep.manual.budget_percent', type: 'slider', min: 0, max: 100, step: 1, fallback: 0, help: 'same for /compact' },
        { path: 'context.summarize_keep.manual.turns', type: 'number', min: 0, help: 'same for /compact' }
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
        { path: 'tools.exec.max_lines', type: 'number', min: 1, help: 'default of the max_lines parameter of exec_command and manage_session: lines one answer may carry' },
        { path: 'tools.exec.max_lines_max', type: 'number', min: 1, help: 'upper bound of that parameter: a call that asks for more lines is truncated to it' },
        { path: 'tools.exec.use_utf8', type: 'bool', help: 'Windows: let the child speak UTF-8; off = convert via the host code page' }
      ]
    },
    {
      title: 'Tools · files', note: 'read_file / write_file / edit_file',
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
      title: 'Tools · web', note: 'webfetch: page → markdown',
      fields: [
        { path: 'tools.webfetch.enabled', type: 'bool', help: 'register webfetch' },
        { path: 'tools.webfetch.mode', type: 'select', options: [
          { value: 'auto', label: 'auto — visible browser, else the HTTP source' },
          { value: 'chrome-headful', label: 'chrome-headful — render in a visible browser window' },
          { value: 'chrome-headless', label: 'chrome-headless — render in a headless browser' },
          { value: 'chrome-attached', label: 'chrome-attached — render through the running browser' },
          { value: 'http', label: 'http — the source only, never a browser' }
        ], help: 'how a page is obtained' },
        { path: 'tools.webfetch.timeout_seconds', type: 'number', min: 1, help: 'seconds allowed for one fetch; the default of the tool timeout argument. Below the built-in floor of 30 seconds the floor is used instead' },
        { path: 'tools.webfetch.max_lines', type: 'number', help: 'lines of markdown fed back; a longer page is answered with its main content (or its middle) and saved under .lightagent. 0 = built-in 100, negative = no limit' },
        { path: 'tools.webfetch.browser_path', type: 'text', placeholder: 'auto-detected', help: 'browser executable to render with; empty discovers an installed one' },
        { path: 'tools.webfetch.user_agent', type: 'text', placeholder: 'path default', help: 'user agent of both paths; empty keeps each path default' },
        { path: 'tools.webfetch.max_bytes', type: 'number', min: 0, help: 'body cap of the HTTP path in bytes; 0 = built-in 8 MiB' },
        { path: 'tools.webfetch.attach_address', type: 'text', placeholder: '127.0.0.1:9222', help: 'chrome-attached: DevTools endpoint of the running browser — a port ("9222"), host:port, or an http:// / ws:// URL; empty = 127.0.0.1:9222' }
      ]
    },
    {
      title: 'Tools · media', note: 'upload_media: hand a file to the model',
      fields: [
        { path: 'tools.upload_media.enabled', type: 'bool', help: 'register upload_media, which lets the model upload a local file of an accepted type as an attachment. Needs openai.media_types as well: both have to be set for the tool to exist' },
        { path: 'tools.upload_media.max_bytes', type: 'number', min: 0, help: 'largest file one upload may carry, in bytes (the web composer enforces the same cap); 0 = built-in 20 MiB' }
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
        { path: 'agent.include_working_dir', type: 'bool', help: 'inject the working directory path into the system prompt' },
        { path: 'agent.summary_in_system_prompt', type: 'bool', help: 'where a request carries the compressed context summary: off (default) = as the first user message, on = in the system prompt' },
        { path: 'agent.include_only_think', type: 'bool', help: 'keep an assistant reply that carries only thinking (no visible text, no tool calls) in the history; off drops it' },
        { path: 'agent.continue_only_think', type: 'bool', help: 'when a reply carried only thinking, ask the model again instead of ending the turn (only applies while include_only_think is on)' }
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
    if (/api_key is empty/.test(message)) {
      return message + ' — paste a key under LLM interfaces';
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
    } else if (def.type === 'list') {
      // A list of strings is typed as one comma separated line; the document
      // keeps a real JSON array (see listValue/splitList).
      el = document.createElement('input');
      el.type = 'text';
      if (def.placeholder) { el.placeholder = def.placeholder; }
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

  // setError / clearError toggle only the "invalid" class instead of rewriting
  // className: a field's base class is not always "field" (the providers
  // repeater is a "providers-block"), and syncForm clears the error of every
  // field — a plain className reset there would demote the repeater back to a
  // .field grid and squeeze its cards into the narrow key column.
  function setError(field, message) {
    field.bad = true;
    field.wrap.classList.add('invalid');
    field.err.textContent = message;
  }

  function clearError(field) {
    field.bad = false;
    field.wrap.classList.remove('invalid');
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
    if (def.type === 'providers') {
      // The provider array owns its own controls (see the providers repeater).
      // It is not an ordinary field row: no key label, no label column and no
      // grid, so the cards take the whole width instead of half of it.
      wrap.className = 'providers-block';
      control.className = 'providers-control';
      buildLLMControl(control, record);
    } else if (def.type === 'secret') {
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

    // The providers repeater is named by its section, so it carries no key label.
    if (def.type !== 'providers') { wrap.appendChild(label); }
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

  // ---- the providers repeater ----
  //
  // The interfaces are an ordered array, which the flat path/field model cannot
  // express, so this control owns the whole array: one card per entry, the
  // arrows reorder them and the buttons add/remove. Every change writes straight
  // into draft.providers, so the JSON mode always shows exactly what a save sends.
  // The card edits the stored object in place, so a key the fields below do not
  // cover (anything a future version adds) is left untouched.

  // Each entry carries the same help the flat form used to show under the key;
  // the card renders it in the row's label column (see renderLLMField).
  var LLM_FIELDS = [
    { key: 'name', label: 'name', type: 'text', placeholder: 'default', help: 'name of this interface: the sidebar model picker and /switchapi refer to it' },
    { key: 'type', label: 'type', type: 'select', options: ['openai'], help: 'request shape; "openai" covers any /chat/completions-compatible endpoint' },
    { key: 'enabled', label: 'enabled', type: 'bool', help: 'offer this interface in the model picker; a run starts on the first enabled one' },
    { key: 'api_base', label: 'api_base', type: 'text', placeholder: 'https://api.openai.com/v1', help: 'base URL; the client appends /chat/completions. empty = built-in default' },
    { key: 'api_key', label: 'api_key', type: 'password', placeholder: '*** keeps the stored key', help: '*** keeps the stored key — paste a new one to replace it' },
    { key: 'model', label: 'model', type: 'text', placeholder: 'gpt-4o-mini', help: 'model name' },
    { key: 'context_window', label: 'context_window', type: 'number', placeholder: '81960', help: 'model context window in tokens; drives compression and the usage badge' },
    { key: 'temperature', label: 'temperature', type: 'range', min: -0.05, max: 2, step: 0.05, fallback: -0.05, omit: { below: 0, value: -1, label: 'omitted (provider default)' }, help: 'sampling temperature; the leftmost position omits the field so the provider uses its own default, 0 is deterministic, 2 is maximum randomness' },
    { key: 'max_tokens', label: 'max_tokens', type: 'number', placeholder: '40960', help: 'cap per reply' },
    { key: 'timeout_seconds', label: 'timeout_seconds', type: 'number', placeholder: '4800', help: 'idle timeout in seconds (waiting for headers or between stream chunks); 0 disables it' },
    { key: 'stream', label: 'stream', type: 'bool', help: 'stream the reply over SSE' },
    { key: 'media_types', label: 'media_types', type: 'text', placeholder: 'image/png, image/jpeg, audio/wav', help: 'media types this model accepts as attachments (comma separated), e.g. image/png, image/*, audio/wav, application/pdf. A family name means the whole family ("image" = "image/*"). Configuring it enables attachments: the web composer can send files and, with tools.upload_media.enabled, the model gets the upload_media tool' },
    { key: 'extra_body', label: 'extra_body', type: 'json', advanced: true, rows: 5, help: 'provider-specific request fields, merged into the request body (overrides built-ins such as temperature)' }
  ];

  // buildLLMControl creates the container and the Add button once; the cards
  // themselves are (re)built by syncLLMRepeater.
  function buildLLMControl(control, record) {
    var list = document.createElement('div');
    list.className = 'llm-list';
    record.listEl = list;
    control.appendChild(list);
    var add = document.createElement('button');
    add.type = 'button';
    add.className = 'ghost llm-add';
    add.textContent = 'Add provider';
    add.onclick = function () {
      var llms = ensureLLMs();
      // The new card is the one about to be filled in, so it opens. The flags are
      // padded first, so appending lands on the new card's position.
      syncOpenFlags(llms.length);
      llms.push({ name: 'api-' + (llms.length + 1), type: 'openai', enabled: true });
      llmOpen.push(true);
      touchLLM(record);
    };
    control.appendChild(add);
  }

  function ensureLLMs() {
    if (!isArray(draft.providers)) { draft.providers = []; }
    return draft.providers;
  }

  // touchLLM marks an edit as unsaved and rebuilds the cards. It is used after a
  // structural change (add / move / remove); a keystroke only writes the
  // document, so the focus is never lost.
  function touchLLM(record) {
    dirty = true;
    setStatus('unsaved changes — Save writes them to config.json');
    syncLLMRepeater(record);
  }

  // badLLM holds the card rows a save would have to reject (a free-form box whose
  // text does not parse). The rows are not top-level fields, so firstBad consults
  // this list too; every rebuild of the cards clears it, because the rows are new
  // elements by then.
  var badLLM = [];

  function trackBad(row, bad) {
    var at = badLLM.indexOf(row);
    if (bad && at < 0) { badLLM.push(row); }
    else if (!bad && at >= 0) { badLLM.splice(at, 1); }
  }

  // llmOpen mirrors draft.providers: one flag per card, so that a rebuild (a
  // reorder, an add) keeps the cards the reader opened. load clears it, which is
  // the folded default of a freshly opened panel.
  var llmOpen = [];

  function syncOpenFlags(count) {
    while (llmOpen.length < count) { llmOpen.push(false); }
    llmOpen.length = count;
  }

  // syncLLMRepeater rebuilds the cards from the document.
  function syncLLMRepeater(record) {
    var list = record.listEl;
    if (!list) { return; }
    badLLM = []; // the rows below are new elements
    list.textContent = '';
    var llms = isArray(draft.providers) ? draft.providers : [];
    if (llms.length === 0) {
      var empty = document.createElement('div');
      empty.className = 'llm-empty';
      empty.textContent = 'no providers — add one';
      list.appendChild(empty);
      return;
    }
    syncOpenFlags(llms.length);
    for (var i = 0; i < llms.length; i++) {
      list.appendChild(renderLLMCard(record, i));
    }
  }

  function renderLLMCard(record, index) {
    var entry = draft.providers[index];
    var open = llmOpen[index] === true;
    var card = document.createElement('div');
    card.className = open ? 'llm-card' : 'llm-card collapsed';
    var head = document.createElement('div');
    head.className = 'llm-head';
    // The bar is the fold: the caret and the interface name sit on a button that
    // opens and closes the card, while the tool buttons next to it stay put (they
    // are siblings, so their clicks never reach the toggle).
    var title = document.createElement('button');
    title.type = 'button';
    title.className = 'llm-title';
    title.setAttribute('aria-expanded', open ? 'true' : 'false');
    var caret = document.createElement('span');
    caret.className = 'llm-caret';
    caret.textContent = '▾';
    title.appendChild(caret);
    var name = document.createElement('span');
    name.className = 'llm-name';
    name.textContent = '#' + (index + 1) + ' · ' + (entry.name || '(unnamed)');
    title.appendChild(name);
    title.onclick = function () {
      var folded = card.classList.toggle('collapsed');
      llmOpen[index] = !folded;
      title.setAttribute('aria-expanded', folded ? 'false' : 'true');
    };
    head.appendChild(title);
    var tools = document.createElement('span');
    tools.className = 'llm-tools';
    tools.appendChild(llmButton('↑', index <= 0, 'move up', function () { moveLLM(record, index, -1); }));
    tools.appendChild(llmButton('↓', index >= draft.providers.length - 1, 'move down', function () { moveLLM(record, index, 1); }));
    tools.appendChild(llmButton('✕', false, 'remove this provider', function () { removeLLM(record, index); }));
    head.appendChild(tools);
    card.appendChild(head);

    var grid = document.createElement('div');
    grid.className = 'llm-grid';
    for (var f = 0; f < LLM_FIELDS.length; f++) {
      grid.appendChild(renderLLMField(entry, LLM_FIELDS[f], index));
    }
    card.appendChild(grid);
    return card;
  }

  function llmButton(label, disabled, title, onclick) {
    var b = document.createElement('button');
    b.type = 'button';
    b.className = 'ghost llm-btn';
    b.textContent = label;
    b.title = title;
    b.setAttribute('aria-label', title);
    b.disabled = !!disabled;
    b.onclick = onclick;
    return b;
  }

  // Every card carries the per-field explanations the flat form had; the cards
  // start folded (see syncForm), so a long list of providers shows only its title
  // bars until one is opened.
  function renderLLMField(entry, def, index) {
    var wrap = document.createElement('label');
    wrap.className = 'llm-field';
    // The key and its help share the left column, exactly like a .field row.
    var cell = document.createElement('span');
    cell.className = 'llm-field-label';
    var key = document.createElement('span');
    key.className = 'llm-field-key';
    key.textContent = def.label;
    cell.appendChild(key);
    if (def.advanced) {
      var tag = document.createElement('span');
      tag.className = 'tag';
      tag.textContent = 'json';
      cell.appendChild(tag);
    }
    if (def.help) {
      var help = document.createElement('span');
      help.className = 'llm-field-help';
      help.textContent = def.help;
      cell.appendChild(help);
    }
    wrap.appendChild(cell);

    var el, out = null;
    if (def.type === 'bool') {
      el = document.createElement('input');
      el.type = 'checkbox';
      el.className = 'switch';
      el.checked = entry[def.key] === true;
    } else if (def.type === 'select') {
      el = document.createElement('select');
      (def.options || []).forEach(function (value) {
        var opt = document.createElement('option');
        opt.value = value;
        opt.textContent = value;
        el.appendChild(opt);
      });
      el.value = entry[def.key] || (def.options && def.options[0]) || '';
    } else if (def.type === 'range') {
      el = document.createElement('input');
      el.type = 'range';
      el.min = def.min;
      el.max = def.max;
      el.step = def.step;
      var stored = entry[def.key];
      // A value below the omit threshold (the negative sentinel) is shown by the
      // slider's lowest position instead.
      el.value = (typeof stored === 'number' && !(def.omit && stored < def.omit.below)) ? stored : def.fallback;
      out = document.createElement('span');
      out.className = 'llm-field-out';
      out.textContent = llmRangeText(def, Number(el.value));
    } else if (def.type === 'json') {
      // Free-form JSON (extra_body): the whole object is the control, so it is a
      // textarea showing the stored value pretty-printed.
      el = document.createElement('textarea');
      el.className = 'json-box';
      el.rows = def.rows || 4;
      el.spellcheck = false;
      var raw = entry[def.key];
      el.value = (raw === undefined || raw === null) ? '' : JSON.stringify(raw, null, 2);
    } else {
      el = document.createElement('input');
      el.type = def.type === 'password' ? 'password' : (def.type === 'number' ? 'number' : 'text');
      if (def.step !== undefined) { el.step = def.step; }
      if (def.placeholder) { el.placeholder = def.placeholder; }
      // A stored secret comes back as the mask; show an empty box instead (the
      // placeholder says the mask keeps it), while a value the user typed shows
      // through so switching to JSON shows the same document.
      el.value = (def.type === 'password' && entry[def.key] === MASK) ? '' : llmFieldValue(entry[def.key]);
    }

    // The control (and a slider's value badge) share one cell of the row, so the
    // key stays in the left column.
    var ctl = document.createElement('span');
    ctl.className = 'llm-field-control';
    ctl.appendChild(el);
    if (out) { ctl.appendChild(out); }
    wrap.appendChild(ctl);

    // The row's error line: a card row carries the same parts as a top-level
    // field (wrap / err / bad and the control), so setError, clearError and the
    // save guard treat it the same way. It spans both columns under the row.
    var err = document.createElement('span');
    err.className = 'llm-field-err';
    wrap.appendChild(err);
    var row = { def: { path: 'providers[' + (index + 1) + '].' + def.key }, wrap: wrap, err: err, el: el, bad: false };

    var write = function () {
      try {
        writeLLMField(entry, def, el);
      } catch (parseErr) {
        // The text stays out of the document (the stored value is kept) and the
        // row is marked, so a save is refused until it is fixed.
        setError(row, friendly(parseErr.message));
        trackBad(row, true);
        return;
      }
      clearError(row);
      trackBad(row, false);
      if (out) { out.textContent = llmRangeText(def, Number(el.value)); }
    };
    el.addEventListener('input', write);
    el.addEventListener('change', write);
    return wrap;
  }

  // llmRangeText is the badge next to a slider: the "omit" label at the lowest
  // position, the plain number everywhere else.
  function llmRangeText(def, value) {
    if (def.omit && value < def.omit.below) { return def.omit.label; }
    return String(value);
  }

  function llmFieldValue(value) {
    if (isArray(value)) { return value.join(', '); }
    return value === undefined || value === null ? '' : String(value);
  }

  // writeLLMField stores one control's value into the entry: an empty box drops
  // the key so the server's default (or the stored secret) applies again. A
  // free-form JSON box may throw (the value does not parse): the caller reports
  // it and leaves the document alone.
  function writeLLMField(entry, def, el) {
    var value;
    if (def.type === 'bool') {
      value = !!el.checked;
    } else if (def.type === 'range') {
      var slider = Number(el.value);
      // The lowest position maps to the stored sentinel (e.g. -1 = omit).
      value = (def.omit && slider < def.omit.below) ? def.omit.value : slider;
    } else if (def.type === 'number') {
      if (String(el.value).trim() === '' || isNaN(Number(el.value))) { value = undefined; }
      else { value = Number(el.value); }
    } else if (def.type === 'json') {
      var text = String(el.value).trim();
      if (text === '') {
        value = undefined;
      } else {
        var parsed = JSON.parse(text);
        if (!isObject(parsed)) { throw new Error('expected a JSON object'); }
        value = parsed;
      }
    } else if (def.key === 'media_types') {
      var items = splitList(el.value);
      value = items.length ? items : undefined;
    } else {
      value = String(el.value) === '' ? undefined : String(el.value);
    }
    if (value === undefined) { delete entry[def.key]; }
    else { entry[def.key] = value; }
    dirty = true;
    setStatus('unsaved changes — Save writes them to config.json');
  }

  function moveLLM(record, index, delta) {
    var llms = ensureLLMs();
    var to = index + delta;
    if (to < 0 || to >= llms.length) { return; }
    var item = llms.splice(index, 1)[0];
    llms.splice(to, 0, item);
    // The fold follows its card: a reorder must not open the card that moved into
    // the old position.
    if (index < llmOpen.length) { llmOpen.splice(to, 0, llmOpen.splice(index, 1)[0]); }
    touchLLM(record);
  }

  function removeLLM(record, index) {
    ensureLLMs().splice(index, 1);
    if (index < llmOpen.length) { llmOpen.splice(index, 1); }
    touchLLM(record);
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

  // listValue renders a stored list as the single line its control shows.
  function listValue(value) {
    if (isArray(value)) { return value.join(', '); }
    return value === undefined || value === null ? '' : String(value);
  }

  // splitList turns the control's line back into the array the document stores:
  // entries are trimmed, empties dropped and duplicates removed, and an empty
  // line deletes the key so the built-in default applies again.
  function splitList(text) {
    var out = [];
    String(text).split(',').forEach(function (entry) {
      var item = entry.trim();
      if (item && out.indexOf(item) < 0) { out.push(item); }
    });
    return out;
  }

  // outText is the badge shown next to a control: a slider whose lowest step is
  // reserved for an "omit" sentinel names that state instead of the raw number,
  // every other control shows its value (or "default" when a text field is empty).
  function outText(field) {
    if (field.def.type === 'slider') {
      if (field.def.omit && Number(field.el.value) < field.def.omit.below) {
        return field.def.omit.label;
      }
      return String(field.el.value);
    }
    return field.el.value === '' ? 'default' : String(field.el.value);
  }

  // syncForm shows the document in the controls. It runs after a load and after a
  // successful save, never while the user is typing.
  function syncForm() {
    for (var i = 0; i < fields.length; i++) {
      var field = fields[i];
      var value = getPath(draft, field.def.path);
      var empty = value === undefined || value === null;
      if (field.def.type === 'providers') {
        // The interface array rebuilds its own cards from the document.
        syncLLMRepeater(field);
      } else if (field.def.type === 'bool') {
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
      } else if (field.def.type === 'list') {
        field.el.value = empty ? '' : listValue(value);
      } else if (field.def.type === 'select') {
        var want = empty ? '' : String(value);
        if (want && !hasOption(field.el, want)) { addOption(field.el, want); }
        if (want) { field.el.value = want; }
      } else if (field.def.type === 'slider') {
        var shown = empty ? field.def.fallback : value;
        // A slider may reserve its lowest step for a sentinel (the temperature
        // control's "omit"). A stored sentinel shows as that lowest position.
        if (field.def.omit && Number(shown) < field.def.omit.below) { shown = field.el.min; }
        field.el.value = String(shown);
      } else {
        field.el.value = empty ? '' : String(value);
      }
      if (field.out) { field.out.textContent = outText(field); }
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
    } else if (def.type === 'list') {
      var items = splitList(field.el.value);
      if (items.length === 0) {
        deletePath(draft, def.path);
      } else {
        setPath(draft, def.path, items);
      }
      clearError(field);
    } else if (def.type === 'slider') {
      var sliderValue = Number(field.el.value);
      // The reserved lowest step maps to the stored sentinel (e.g. -1 = omit).
      if (def.omit && sliderValue < def.omit.below) { sliderValue = def.omit.value; }
      setPath(draft, def.path, sliderValue);
      clearError(field);
      if (field.out) { field.out.textContent = outText(field); }
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
  // sends a document the editor already knows is broken. A card row's free-form
  // JSON registers in badLLM (the repeater owns those rows, see renderLLMField).
  function firstBad() {
    for (var i = 0; i < fields.length; i++) {
      if (fields[i].bad) { return fields[i]; }
    }
    return badLLM.length ? badLLM[0] : null;
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
      // The raw editor takes the height the form is showing, so switching modes
      // never resizes (or jumps) the panel. A form taller than the visible
      // editor is capped at that height — the textarea scrolls on its own.
      var body = form.parentNode;
      var target = form.offsetHeight;
      if (body) {
        // The editor's usable height is the visible box minus its padding, so the
        // textarea fits exactly and the scrolling pane never grows a second
        // scrollbar.
        var cs = window.getComputedStyle(body);
        var inner = body.clientHeight - (parseFloat(cs.paddingTop) || 0) - (parseFloat(cs.paddingBottom) || 0);
        if (inner > 0 && target > inner) { target = inner; }
      }
      if (target > 0) { json.style.height = target + 'px'; }
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
      // Opening the panel folds every provider card to its title bar (llmOpen);
      // from there the reader opens the ones they work on, and a later save keeps
      // that (the state is per panel, not per document).
      llmOpen = [];
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
    // An unconfirmed restart lapses with the panel: a button that still says
    // "Confirm restart" after a reopened panel would be a trap.
    disarmRestart();
    if (!modal.hidden) { modal.hidden = true; }
  }

  // ---- restart ----
  // Restart is the panel's other action, and the way a saved document takes
  // effect: it asks the mirror to save the session and start a fresh lightagent,
  // which loads that session again (POST /api/restart, see restart.go). This page
  // loses its connection for a moment and reconnects to the new process on the
  // same address, so the transcript comes back by itself; window.MIRROR (app.js)
  // is told about it and says so in the header.
  //
  // The control asks once: the first click explains what a restart does and turns
  // the button into the confirmation, which lapses after a moment so a click much
  // later cannot end the run by accident.
  var RESTART_LABEL = restartEl ? restartEl.textContent : 'Restart';
  var RESTART_ASK_MS = 10000;
  var restartArmed = false;
  var restartAskTimer = null;
  var restartPending = false;

  function disarmRestart() {
    restartArmed = false;
    if (restartAskTimer) { clearTimeout(restartAskTimer); restartAskTimer = null; }
    if (restartEl) { restartEl.textContent = RESTART_LABEL; }
  }

  function armRestart() {
    restartArmed = true;
    if (restartEl) { restartEl.textContent = 'Confirm restart'; }
    setStatus('Restart lightagent? The session is saved first, then a fresh process starts and loads it again — click "Confirm restart" to go ahead.', 'warn');
    if (restartAskTimer) { clearTimeout(restartAskTimer); }
    restartAskTimer = setTimeout(function () {
      disarmRestart();
      setStatus(HINT);
    }, RESTART_ASK_MS);
  }

  function restart() {
    if (restartPending) { return; }
    if (!restartArmed) { armRestart(); return; }
    disarmRestart();
    restartPending = true;
    if (restartEl) { restartEl.disabled = true; }
    setStatus('saving the session and restarting…');
    fetch('/api/restart', { method: 'POST', credentials: 'same-origin' })
      .then(readJSON)
      .then(function (doc) {
        // The accepted restart ends this process in a moment; the note stays so a
        // user looking at the panel knows what the silence means.
        if (window.MIRROR) { window.MIRROR.restarting(); }
        setStatus('restarting — the session was saved to ' + (doc.saved || 'the session file') + '; this page reconnects by itself.', 'ok');
      })
      .catch(function (err) {
        // A refused restart leaves the run — and this page — as it was.
        restartPending = false;
        if (restartEl) { restartEl.disabled = false; }
        setStatus('restart failed: ' + friendly(err.message), 'bad');
      });
  }

  // onRestarted puts the control back in service once the page is connected to the
  // process this panel started (app.js calls it through window.MIRROR). Without it
  // a second restart would need a page reload.
  function onRestarted() {
    restartPending = false;
    if (restartEl) { restartEl.disabled = false; }
    setStatus(HINT);
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
  if (restartEl && CAN_RESTART) {
    restartEl.onclick = restart;
    // The page reports the reconnect that follows a restart back to this panel
    // (see onRestarted), so the button is usable again without a reload.
    if (window.MIRROR) { window.MIRROR.onReconnect = onRestarted; }
  } else if (restartEl) {
    restartEl.hidden = true;
  }
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

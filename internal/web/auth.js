// Sign-in for the web mirror.
//
// The password never leaves the browser: the page asks /api/session for the
// public salt, digests what the user typed with a salted SHA-256 and posts only
// the digest. "Stay signed in on this browser" stores that digest — not the
// password — so a browser whose session cookie is gone can sign in by itself;
// the server rotates the salt whenever the password changes, which invalidates
// it. The browser's own password manager still gets a normal form to save.
//
// Other scripts use window.AUTH: ensure() resolves once the session state is
// known (signing in silently when a digest is stored), onChange(fn) fires when a
// browser signs in or out, unauthorized() says that a request came back 401, and
// prompt() asks for a login (the config editor uses it).
(function () {
  var STORE_KEY = 'lightagent.signin';

  var modal = document.getElementById('loginModal');
  var form = document.getElementById('loginForm');
  var passwordEl = document.getElementById('loginPassword');
  var rememberEl = document.getElementById('loginRemember');
  var statusEl = document.getElementById('loginStatus');
  var submitEl = document.getElementById('loginSubmit');
  var signOutEl = document.getElementById('signOut');

  // state.required: the mirror has a password; state.ok: this browser is signed
  // in (always true when no password is configured); state.salt: the public salt
  // the digest is bound to.
  var state = { required: false, ok: true, salt: '' };
  var listeners = [];
  var checking = null;

  // ---- salted SHA-256 ----
  // WebCrypto is only available in a secure context and the mirror is normally
  // reached over plain HTTP on the LAN, so SHA-256 is implemented here. It must
  // match passwd.Digest in Go exactly: hex(SHA256(salt + utf8(password))).

  var K = [
    0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
    0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
    0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
    0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
    0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
    0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
    0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
    0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2
  ];

  function rotr(value, bits) { return (value >>> bits) | (value << (32 - bits)); }

  // utf8Bytes encodes text the way Go encodes a string: UTF-8, no BOM.
  function utf8Bytes(text) {
    if (window.TextEncoder) { return new TextEncoder().encode(text); }
    var bytes = [];
    for (var i = 0; i < text.length; i++) {
      var code = text.charCodeAt(i);
      if (code < 0x80) {
        bytes.push(code);
      } else if (code < 0x800) {
        bytes.push(0xc0 | (code >> 6), 0x80 | (code & 0x3f));
      } else if (code < 0xd800 || code >= 0xe000) {
        bytes.push(0xe0 | (code >> 12), 0x80 | ((code >> 6) & 0x3f), 0x80 | (code & 0x3f));
      } else {
        // A surrogate pair: encode the code point it stands for.
        var next = text.charCodeAt(++i);
        var point = 0x10000 + (((code & 0x3ff) << 10) | (next & 0x3ff));
        bytes.push(0xf0 | (point >> 18), 0x80 | ((point >> 12) & 0x3f),
                   0x80 | ((point >> 6) & 0x3f), 0x80 | (point & 0x3f));
      }
    }
    return new Uint8Array(bytes);
  }

  function hex(bytes) {
    var out = '';
    for (var i = 0; i < bytes.length; i++) {
      out += (bytes[i] < 16 ? '0' : '') + bytes[i].toString(16);
    }
    return out;
  }

  // digest is the value sent instead of the password; passwd_test.go pins the
  // same expression on the Go side.
  function digest(salt, password) {
    return hex(sha256Bytes(utf8Bytes(salt + password)));
  }

  function sha256Bytes(bytes) {
    var h = [0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19];
    var len = bytes.length;
    var total = (len + 9 + 63) & ~63; // 0x80, then padding, then the 8 length bytes
    var msg = new Uint8Array(total);
    msg.set(bytes);
    msg[len] = 0x80;
    var bitsHi = Math.floor(len / 536870912); // len*8 / 2^32
    var bitsLo = (len * 8) >>> 0;
    msg[total - 8] = (bitsHi >>> 24) & 0xff;
    msg[total - 7] = (bitsHi >>> 16) & 0xff;
    msg[total - 6] = (bitsHi >>> 8) & 0xff;
    msg[total - 5] = bitsHi & 0xff;
    msg[total - 4] = (bitsLo >>> 24) & 0xff;
    msg[total - 3] = (bitsLo >>> 16) & 0xff;
    msg[total - 2] = (bitsLo >>> 8) & 0xff;
    msg[total - 1] = bitsLo & 0xff;

    var w = new Uint32Array(64);
    for (var offset = 0; offset < total; offset += 64) {
      for (var i = 0; i < 16; i++) {
        w[i] = (msg[offset + i * 4] << 24) | (msg[offset + i * 4 + 1] << 16) |
               (msg[offset + i * 4 + 2] << 8) | msg[offset + i * 4 + 3];
      }
      for (i = 16; i < 64; i++) {
        var s0 = rotr(w[i - 15], 7) ^ rotr(w[i - 15], 18) ^ (w[i - 15] >>> 3);
        var s1 = rotr(w[i - 2], 17) ^ rotr(w[i - 2], 19) ^ (w[i - 2] >>> 10);
        w[i] = (w[i - 16] + s0 + w[i - 7] + s1) | 0;
      }
      var a = h[0], b = h[1], c = h[2], d = h[3], e = h[4], f = h[5], g = h[6], hh = h[7];
      for (i = 0; i < 64; i++) {
        var S1 = rotr(e, 6) ^ rotr(e, 11) ^ rotr(e, 25);
        var ch = (e & f) ^ (~e & g);
        var t1 = (hh + S1 + ch + K[i] + w[i]) | 0;
        var S0 = rotr(a, 2) ^ rotr(a, 13) ^ rotr(a, 22);
        var maj = (a & b) ^ (a & c) ^ (b & c);
        var t2 = (S0 + maj) | 0;
        hh = g; g = f; f = e; e = (d + t1) | 0; d = c; c = b; b = a; a = (t1 + t2) | 0;
      }
      h[0] = (h[0] + a) | 0; h[1] = (h[1] + b) | 0; h[2] = (h[2] + c) | 0; h[3] = (h[3] + d) | 0;
      h[4] = (h[4] + e) | 0; h[5] = (h[5] + f) | 0; h[6] = (h[6] + g) | 0; h[7] = (h[7] + hh) | 0;
    }
    var out = new Uint8Array(32);
    for (i = 0; i < 8; i++) {
      out[i * 4] = (h[i] >>> 24) & 0xff;
      out[i * 4 + 1] = (h[i] >>> 16) & 0xff;
      out[i * 4 + 2] = (h[i] >>> 8) & 0xff;
      out[i * 4 + 3] = h[i] & 0xff;
    }
    return out;
  }

  // ---- dialog ----

  function setStatus(text, kind) {
    if (!statusEl) { return; }
    statusEl.textContent = text || '';
    statusEl.className = 'login-status' + (kind ? ' ' + kind : '');
  }

  function notify() {
    if (signOutEl) { signOutEl.hidden = !(state.required && state.ok); }
    for (var i = 0; i < listeners.length; i++) { listeners[i](state.ok); }
  }

  function open(text, kind) {
    if (!modal) { return; }
    modal.hidden = false;
    setStatus(text || '', kind || '');
    if (passwordEl && !text) { passwordEl.focus(); }
  }

  function close() {
    if (modal) { modal.hidden = true; }
  }

  // ---- stored digest ----
  // Only the salted digest is kept, never the password, and only when the user
  // asked to stay signed in.

  function loadStore() {
    try {
      var raw = window.localStorage.getItem(STORE_KEY);
      if (!raw) { return null; }
      var saved = JSON.parse(raw);
      if (saved && typeof saved.salt === 'string' && typeof saved.digest === 'string') { return saved; }
    } catch (err) { /* a blocked or corrupt store just means no auto sign-in */ }
    return null;
  }

  function saveStore(salt, value) {
    try {
      window.localStorage.setItem(STORE_KEY, JSON.stringify({ salt: salt, digest: value }));
    } catch (err) { /* ignore */ }
  }

  function clearStore() {
    try { window.localStorage.removeItem(STORE_KEY); } catch (err) { /* ignore */ }
  }

  // ---- requests ----

  function readJSON(res) {
    return res.json().catch(function () { return {}; }).then(function (body) {
      if (!res.ok) { throw new Error(body.error || ('HTTP ' + res.status)); }
      return body;
    });
  }

  // check asks /api/session and caches the in-flight request, so the mirror and
  // the config editor can both await it.
  function check() {
    if (checking) { return checking; }
    checking = fetch('/api/session', { credentials: 'same-origin' })
      .then(readJSON)
      .then(function (body) {
        state.required = !!body.auth_required;
        state.salt = body.salt || '';
        state.ok = !!body.authenticated;
        checking = null;
        return state;
      })
      .catch(function (err) {
        checking = null;
        throw err;
      });
    return checking;
  }

  function post(path, payload) {
    return fetch(path, {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload || {})
    }).then(readJSON);
  }

  // signIn sends the digest; the password stays in this function's scope.
  function signIn(value, remember) {
    return post('/api/login', { digest: value, remember: !!remember });
  }

  // autoSignIn uses the stored digest when the salt still matches. A different
  // salt means the password was changed elsewhere, so the stored value is
  // dropped instead of being replayed.
  function autoSignIn() {
    var saved = loadStore();
    if (!saved) { return Promise.resolve(false); }
    if (!state.salt || saved.salt !== state.salt) {
      clearStore();
      return Promise.resolve(false);
    }
    return signIn(saved.digest, true).then(function () {
      state.ok = true;
      return true;
    }).catch(function () {
      clearStore();
      return false;
    });
  }

  // ensure resolves the session state, signing in silently when a digest is
  // stored. It is what the mirror waits for before opening the WebSocket.
  function ensure() {
    return check().then(function () {
      if (!state.required || state.ok) {
        notify();
        return state.ok;
      }
      return autoSignIn().then(function (ok) {
        if (ok) {
          passwordEl.value = '';
          setStatus('');
          close();
        } else {
          open(state.salt ? 'Enter the password for this mirror.' : '');
        }
        notify();
        return ok;
      });
    });
  }

  function submit(event) {
    if (event && event.preventDefault) { event.preventDefault(); }
    if (!passwordEl) { return; }
    var typed = passwordEl.value;
    if (!typed) { setStatus('enter the password', 'bad'); return; }
    if (!state.salt) {
      // No salt yet: refresh the session state and try again.
      check().then(function () {
        if (!state.required) { close(); notify(); } else { submit(); }
      }).catch(function (err) { setStatus(err.message, 'bad'); });
      return;
    }
    var value = digest(state.salt, typed);
    var remember = !!(rememberEl && rememberEl.checked);
    if (submitEl) { submitEl.disabled = true; }
    setStatus('signing in…');
    signIn(value, remember).then(function () {
      state.ok = true;
      if (remember) { saveStore(state.salt, value); } else { clearStore(); }
      passwordEl.value = '';
      close();
      setStatus('');
      notify();
    }).catch(function (err) {
      setStatus(err.message, 'bad');
      passwordEl.select();
      passwordEl.focus();
      notify();
    }).then(function () {
      if (submitEl) { submitEl.disabled = false; }
    });
  }

  function signOut() {
    return post('/api/logout', {}).catch(function () {
      // The cookie is cleared by the server anyway; the page just signs out.
    }).then(function () {
      clearStore();
      state.ok = false;
      notify();
      open('Signed out.');
    });
  }

  window.AUTH = {
    required: function () { return state.required; },
    ok: function () { return state.ok; },
    salt: function () { return state.salt; },
    digest: digest,
    ensure: ensure,
    signOut: signOut,
    // remember stores the digest of a password that was just set (the salt
    // rotates with it), so this browser can sign in by itself next time. An
    // empty password forgets the stored digest.
    remember: function (password, salt) {
      var useSalt = salt || state.salt;
      if (!useSalt || !password) { clearStore(); return; }
      state.salt = useSalt;
      saveStore(useSalt, digest(useSalt, password));
    },
    // prompt asks for a login (used when a request needs a session).
    prompt: function (text) { state.ok = false; notify(); open(text || ''); },
    // unauthorized reports a 401 from elsewhere in the page.
    unauthorized: function () { state.ok = false; notify(); open('Your session ended. Sign in again.'); },
    onChange: function (fn) { listeners.push(fn); },
  };

  if (form) { form.addEventListener('submit', submit); }
  if (signOutEl) { signOutEl.onclick = function () { signOut(); }; }
  ensure();
})();

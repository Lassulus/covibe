// Glue between the vendored xterm.js build and covibe's terminal WebSocket.
// Classic script, no modules: the dashboard loads it with a plain <script>.
(function () {
  'use strict';

  var FIT_DEBOUNCE_MS = 150;
  // btoa() takes a binary string, so chunk the UTF-8 bytes to stay under the
  // argument limit of String.fromCharCode.apply for pastes.
  var CHUNK = 0x8000;
  var encoder = new TextEncoder();

  function b64(text) {
    var bytes = encoder.encode(text);
    var bin = '';
    for (var i = 0; i < bytes.length; i += CHUNK) {
      bin += String.fromCharCode.apply(null, bytes.subarray(i, i + CHUNK));
    }
    return btoa(bin);
  }

  function fitCtor() {
    var f = window.FitAddon;
    return f && f.FitAddon ? f.FitAddon : f;
  }

  // xterm's DOM renderer injects two <style> elements and exposes no nonce
  // hook, and CSP validates a nonce at insertion time — so stamp the page
  // nonce on style elements while xterm builds its DOM, then unhook.
  function pageNonce() {
    var s = document.querySelector('script[nonce]');
    return s ? (s.nonce || s.getAttribute('nonce') || '') : '';
  }

  function withNonce(fn) {
    var nonce = pageNonce();
    if (!nonce) return fn();
    var orig = document.createElement;
    document.createElement = function (tag) {
      var el = orig.apply(document, arguments);
      if (String(tag).toLowerCase() === 'style') el.setAttribute('nonce', nonce);
      return el;
    };
    try { return fn(); } finally { document.createElement = orig; }
  }

  function covibeTerminal(container, sessionId, opts) {
    opts = opts || {};

    // The terminal gets its own child so FitAddon measures a box that holds
    // nothing but the grid; the read-only note sits beside it, not inside.
    var screen = document.createElement('div');
    screen.className = 'covibe-term-screen';
    container.appendChild(screen);

    var term = new window.Terminal({
      allowProposedApi: true,
      convertEol: false,
      cursorBlink: true,
      fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace',
      fontSize: 13,
      scrollback: 5000,
      theme: { background: '#0b0e13', foreground: '#e6edf3' },
    });
    var fit = new (fitCtor())();
    term.loadAddon(fit);
    withNonce(function () {
      term.open(screen);
      safeFit();
    });

    var ws = null;
    var onData = null;
    var observer = null;
    var fitTimer = 0;
    var closed = false;

    function safeFit() {
      // fit() throws while the container is display:none or zero-sized.
      try { fit.fit(); } catch (e) { /* not laid out yet */ }
    }

    function send(obj) {
      if (!ws || ws.readyState !== WebSocket.OPEN) return;
      try { ws.send(JSON.stringify(obj)); } catch (e) { /* socket died mid-send */ }
    }

    function note(text) {
      var el = document.createElement('div');
      el.className = 'covibe-term-note';
      el.textContent = text;
      container.appendChild(el);
    }

    function dim(text) {
      // SGR 2 (faint) so the closing line reads as chrome, not program output.
      term.write('\r\n\x1b[2m' + text + '\x1b[0m\r\n');
    }

    var url = location.origin.replace(/^http/, 'ws') +
      '/api/v1/sessions/' + encodeURIComponent(sessionId) +
      '/terminal?cols=' + term.cols + '&rows=' + term.rows;
    ws = new WebSocket(url);
    ws.binaryType = 'arraybuffer';

    ws.onmessage = function (ev) {
      if (typeof ev.data !== 'string') {
        term.write(new Uint8Array(ev.data));
        return;
      }
      var msg;
      try { msg = JSON.parse(ev.data); } catch (e) { return; }
      if (msg.t === 'hello') {
        // The server's grid is authoritative: adopt the size it reports.
        if (msg.cols > 0 && msg.rows > 0 && (msg.cols !== term.cols || msg.rows !== term.rows)) {
          term.resize(msg.cols, msg.rows);
        }
        if (msg.write && opts.write !== false) {
          onData = term.onData(function (d) { send({ t: 'input', b64: b64(d) }); });
          term.focus();
        } else {
          term.options.disableStdin = true;
          note('read-only');
        }
      } else if (msg.t === 'exit') {
        dim('[session ended]');
        shutdown();
      } else if (msg.t === 'error') {
        dim('[error: ' + (msg.msg || 'terminal unavailable') + ']');
        shutdown();
      }
    };
    // No onerror notice: a failed connect fires onclose right after it.
    ws.onclose = function () { if (!closed) dim('[disconnected]'); shutdown(); };

    observer = new ResizeObserver(function () {
      clearTimeout(fitTimer);
      fitTimer = setTimeout(function () {
        var before = term.cols + 'x' + term.rows;
        safeFit();
        if (before !== term.cols + 'x' + term.rows) send({ t: 'resize', cols: term.cols, rows: term.rows });
      }, FIT_DEBOUNCE_MS);
    });
    observer.observe(screen);

    // Tear the socket down but leave the terminal readable: a session that
    // just ended should still show its last screen until the user closes.
    function shutdown() {
      if (ws) {
        ws.onmessage = ws.onclose = ws.onerror = null;
        try { ws.close(); } catch (e) { /* already closed */ }
        ws = null;
      }
      if (onData) { onData.dispose(); onData = null; }
    }

    return {
      focus: function () { term.focus(); },
      close: function () {
        if (closed) return;
        closed = true;
        clearTimeout(fitTimer);
        if (observer) { observer.disconnect(); observer = null; }
        shutdown();
        term.dispose();
      },
    };
  }

  window.covibeTerminal = covibeTerminal;
})();

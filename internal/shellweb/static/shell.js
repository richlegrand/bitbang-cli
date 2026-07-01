// shell.js — runs inside the device-frame iframe of bitbang's
// browser UI. Boots an xterm.js terminal and connects it to the
// magic `/__bitbang/shell` WebSocket. bootstrap.js bridges that to a
// SWSP shell stream, but as a *generic* transport — it shuttles raw
// bytes and doesn't know anything about shell-specific framing. All
// the per-cap logic (tag-byte prefix for stdin/stdout/stderr,
// resize/signal events, FIN payload parsing for exit codes) lives in
// this file.
//
// Wire shape (matches internal/streamtype/shell.go on the device):
//   SYN payload: {type:"shell", pty, cols, rows, …}
//                — bootstrap.js builds this from the URL we open;
//                  query params are JSON-parsed where possible.
//   DAT outbound (to device): [1B tag][bytes]
//                  tag 0 = stdin, tag 3 = signal name, tag 4 =
//                  [cols:u16][rows:u16] LE resize.
//   DAT inbound  (from device): [1B tag][bytes]
//                  tag 1 = stdout, tag 2 = stderr.
//   FIN payload (from device): {"exit_code": N, "signal": "..."}
//                  delivered to ws.onclose as the `reason` string.

(function () {
  if (typeof Terminal === "undefined" || typeof FitAddon === "undefined") {
    document.getElementById("boot-msg").textContent =
      "Failed to load xterm.js from CDN. Check internet connectivity.";
    return;
  }

  const container = document.getElementById("terminal");
  container.innerHTML = ""; // clear loading message

  const term = new Terminal({
    cursorBlink: true,
    fontFamily: 'Menlo, Monaco, "DejaVu Sans Mono", monospace',
    fontSize: 15,
    // Heavier stems so 1px-thin glyph features don't get antialiased away
    // (quick test before pinning a bundled webfont).
    fontWeight: 500,
    scrollback: 5000,
    convertEol: false, // remote PTY already emits CRLF
  });
  const fit = new FitAddon.FitAddon();
  term.loadAddon(fit);
  term.open(container);
  fit.fit();

  // SWSP shell SYN gets built by bootstrap.js from our URL: the
  // pathname picks the stream type ("shell"), and each query param
  // becomes a JSON-typed field in the SYN payload. So `pty=true`
  // arrives as a real bool, `cols=80` as a number.
  const wsUrl = new URL("ws://device/__bitbang/shell");
  wsUrl.searchParams.set("pty", "true");
  wsUrl.searchParams.set("cols", String(term.cols));
  wsUrl.searchParams.set("rows", String(term.rows));

  const ws = new WebSocket(wsUrl.toString());
  ws.binaryType = "arraybuffer";

  // Shell DAT tag bytes — first byte of every shell DAT frame.
  const TAG_STDIN = 0;
  const TAG_STDOUT = 1;
  const TAG_STDERR = 2;
  const TAG_SIGNAL = 3;
  const TAG_RESIZE = 4;

  ws.onopen = () => {
    fit.fit(); // recompute size now that the WS is up
    term.focus();
  };

  // Inbound bytes from the device: [tag][payload]. bootstrap.js
  // delivers them as raw ArrayBuffer; we strip the tag and route.
  // The device's early-error SYN arrives as a text WS message
  // (bootstrap.js's generic bridge sends those through as strings).
  ws.onmessage = (evt) => {
    if (typeof evt.data === "string") {
      // Mid-stream metadata, typically an early-spawn-failure error.
      try {
        const info = JSON.parse(evt.data);
        if (info && info.error) {
          term.write("\r\n[error: " + info.error + "]\r\n");
        }
      } catch (e) {
        // Not JSON — just display as-is.
        term.write(evt.data);
      }
      return;
    }
    if (!(evt.data instanceof ArrayBuffer)) return;
    const view = new Uint8Array(evt.data);
    if (view.byteLength < 1) return;
    const tag = view[0];
    const body = view.subarray(1);
    if (tag === TAG_STDOUT || tag === TAG_STDERR) {
      term.write(body);
    }
    // Unknown tags from the device are silently dropped.
  };

  ws.onclose = (e) => {
    // FIN payload from the device is in e.reason. Per the shell
    // protocol it's `{exit_code, signal?}` on normal exit, possibly
    // `{error: "..."}` on early failure. Translate to a one-line
    // status the user can see.
    let line = "[session ended";
    if (e.reason) {
      try {
        const info = JSON.parse(e.reason);
        if (info.error) {
          line = "[error: " + info.error;
        } else if (info.signal) {
          line = "[killed by " + info.signal;
        } else if (typeof info.exit_code === "number") {
          line = "[exit " + info.exit_code;
        }
      } catch (parseErr) {
        // Not JSON; use raw reason.
        line = "[" + e.reason;
      }
    }
    term.write("\r\n" + line + "]\r\n");
  };

  // User keystrokes → stdin bytes, prefixed with the stdin tag.
  term.onData((data) => {
    if (ws.readyState !== WebSocket.OPEN) return;
    const stdinBytes = new TextEncoder().encode(data);
    const buf = new Uint8Array(1 + stdinBytes.length);
    buf[0] = TAG_STDIN;
    buf.set(stdinBytes, 1);
    ws.send(buf);
  });

  // Terminal resize → tag-prefixed packed-u16 frame.
  term.onResize(({ cols, rows }) => {
    if (ws.readyState !== WebSocket.OPEN) return;
    const buf = new Uint8Array(5);
    buf[0] = TAG_RESIZE;
    new DataView(buf.buffer).setUint16(1, cols, true);
    new DataView(buf.buffer).setUint16(3, rows, true);
    ws.send(buf);
  });

  // Window resize → fit, which triggers term.onResize above.
  window.addEventListener("resize", () => fit.fit());

  container.addEventListener("click", () => term.focus());
})();

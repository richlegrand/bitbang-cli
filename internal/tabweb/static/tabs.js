// tabs.js — runs on the listener-served tab page. Two jobs:
//
//  1. Switch which cap's iframe is visible based on tab clicks,
//     persist the current tab in the URL hash so reloads / shared
//     URLs land on the right tab.
//
//  2. Relay ws-shim postMessages between nested cap iframes and the
//     bootstrap page that owns the WebRTC data channel. Bootstrap.js
//     only listens for messages from its direct child iframe (the
//     device-frame, which is this tab.html). Cap-specific code (e.g.
//     xterm in shell.js) opens WebSockets from a *nested* iframe, and
//     its ws-shim posts to *its* parent — us. We forward those up;
//     we forward bootstrap's replies back down.

(function () {
  const buttons = document.querySelectorAll("#tabs button");
  const iframes = document.querySelectorAll("main iframe");

  // -- ws-shim relay between nested iframes and the bootstrap page.
  //    Identify which window each message came from and forward to
  //    the appropriate side. Messages between two cap iframes (which
  //    shouldn't happen) are dropped.
  window.addEventListener("message", (event) => {
    if (event.source === window.parent) {
      // From bootstrap → broadcast to every cap iframe. ws-shim
      // implementations key off pathname/streamId, so the non-target
      // iframes silently ignore. (HTTP/SW messages don't take this
      // path; they go through the service worker.)
      iframes.forEach((f) => {
        try {
          f.contentWindow.postMessage(event.data, "*");
        } catch (e) {}
      });
      return;
    }
    // From one of the nested iframes → forward to bootstrap. Verify
    // event.source matches an iframe we own to avoid forwarding
    // arbitrary external messages.
    for (const f of iframes) {
      if (f.contentWindow === event.source) {
        parent.postMessage(event.data, "*");
        return;
      }
    }
  });

  function activate(name) {
    let matched = false;
    buttons.forEach((b) => {
      const is = b.dataset.cap === name;
      b.classList.toggle("active", is);
      if (is) matched = true;
    });
    iframes.forEach((f) => {
      // Use a class toggle (visibility-based, see tabs.css) instead
      // of the HTML `hidden` attribute, so inactive iframes keep
      // their full layout dimensions. xterm.js measures its
      // container at load time; collapsing to zero height would
      // make it size to 10x6 cells.
      f.classList.toggle("inactive", f.dataset.cap !== name);
    });
    if (matched) {
      // Keep the hash up to date so a copy-paste of the URL lands on
      // the same tab. We don't use `pushState` because the iframes
      // are sandboxed siblings — back/forward shouldn't navigate
      // them, just the tab strip.
      if (location.hash !== "#" + name) {
        history.replaceState(null, "", "#" + name);
      }
    }
  }

  buttons.forEach((b) => {
    b.addEventListener("click", () => activate(b.dataset.cap));
  });

  // Restore tab from URL hash on load, falling back to the first
  // button (also the default-active one rendered server-side).
  const initial = location.hash.replace(/^#/, "");
  if (initial) {
    activate(initial);
  }
})();

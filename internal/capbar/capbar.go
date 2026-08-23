// Package capbar renders the strip that lets a browser move between the
// capabilities a link grants.
//
// Shared because it belongs on every page that can be the one you land
// on, not only the shell. A link granting files and proxy used to show
// the files page with no way to reach the proxy: the strip existed, but
// it lived inside the shell launcher, and the launcher is only mounted
// when shell is granted.
package capbar

import (
	"fmt"
	"html"
	"strings"
)

// Item is one entry in the dropdown: what it is called, and the path the
// parent frame should open.
type Item struct {
	Label string
	Path  string
}

// Placeholder is the marker a page carries where the strip is spliced.
const Placeholder = "<!-- CAP_BAR -->"

// Inject splices the strip into a page and marks the body with
// .with-cap-bar so the page can leave room for it.
//
// The room-making rule stays with the page rather than here: the strip
// is 22px of fixed overlay, and what that costs depends on the layout
// under it. The shell shrinks a full-height terminal with a calc();
// an ordinary flowing page just needs padding-top. A rule here would be
// wrong for one of them. With no items the marker is removed and the page is
// unchanged -- a link granting one thing needs no way to move between
// things.
func Inject(page string, items []Item) string {
	if len(items) == 0 {
		return strings.Replace(page, Placeholder+"\n", "", 1)
	}
	out := strings.Replace(page, Placeholder, Render(items), 1)
	return strings.Replace(out, "<body>", `<body class="with-cap-bar">`, 1)
}

func Render(items []Item) string {
	var dropdown strings.Builder
	for _, it := range items {
		fmt.Fprintf(&dropdown,
			`<a href="#" data-path="%s">%s</a>`,
			html.EscapeString(it.Path), html.EscapeString(it.Label))
	}
	return fmt.Sprintf(`<style>
#bb-cap-bar {
  position: fixed; top: 0; left: 0; right: 0; height: 22px;
  /* border-box so the hairline lives inside the 22px every page offsets
     by -- otherwise the strip is 23px and covers a row of content. */
  box-sizing: border-box;
  background: #e8e8e8; border-bottom: 1px solid #d0d0d0;
  display: flex; align-items: center; padding: 0 8px 0 2px;
  font-family: -apple-system, "Segoe UI", Roboto, sans-serif;
  color: #333; z-index: 100;
}
#bb-cap-bar button {
  background: transparent; border: none; padding: 2px 6px;
  cursor: pointer; display: flex; align-items: center;
}
#bb-cap-bar button:hover { background: #d8d8d8; border-radius: 3px; }
#bb-cap-bar svg { display: block; }
#bb-cap-bar nav {
  position: absolute; top: 22px; left: 0;
  min-width: 160px; background: #f4f4f4;
  border: 1px solid #d0d0d0;
  box-shadow: 0 2px 6px rgba(0,0,0,0.18);
}
#bb-cap-bar nav[hidden] { display: none; }
#bb-cap-bar nav a {
  display: block; padding: 4px 14px;
  font-size: 14px; color: #333; text-decoration: none;
}
#bb-cap-bar nav a:hover { background: #e2e2e2; }
</style>
<div id="bb-cap-bar">
  <button id="bb-ham" aria-label="Capabilities menu">
    <svg width="10" height="6" viewBox="0 0 10 6" xmlns="http://www.w3.org/2000/svg">
      <path d="M0 0 L10 0 L5 6 Z" fill="#555"/>
    </svg>
  </button>
  <nav id="bb-menu" hidden>%s</nav>
</div>
<script>
(function(){
  var ham = document.getElementById('bb-ham');
  var menu = document.getElementById('bb-menu');
  ham.addEventListener('click', function(e){ e.stopPropagation(); menu.hidden = !menu.hidden; });
  document.addEventListener('click', function(e){
    if (!menu.contains(e.target) && e.target !== ham) menu.hidden = true;
  });
  menu.querySelectorAll('a').forEach(function(a){
    a.addEventListener('click', function(e){
      e.preventDefault();
      parent.postMessage({type:'bb-open-cap', path: a.dataset.path}, '*');
      menu.hidden = true;
    });
  });
})();
</script>`, dropdown.String())
}

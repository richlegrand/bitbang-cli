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

// Style is how the control presents itself, which depends on what it is
// sitting on.
type Style int

const (
	// Bar spans the width in black, the way the shell has always shown
	// it. The terminal is black too, so the strip reads as its title bar
	// rather than as something laid over the page.
	Bar Style = iota
	// Caret is the control alone, no band across the page. A light page
	// has its own header and background; a full-width strip over it is
	// furniture, and a black one looks like a rendering fault.
	Caret
)

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
func Inject(page string, items []Item, style Style) string {
	if len(items) == 0 {
		return strings.Replace(page, Placeholder+"\n", "", 1)
	}
	out := strings.Replace(page, Placeholder, Render(items, style), 1)
	return strings.Replace(out, "<body>", `<body class="with-cap-bar">`, 1)
}

func Render(items []Item, style Style) string {
	var dropdown strings.Builder
	for _, it := range items {
		fmt.Fprintf(&dropdown,
			`<a href="#" data-path="%s">%s</a>`,
			html.EscapeString(it.Path), html.EscapeString(it.Label))
	}
	// Shared structure; the two styles differ only in what the strip and
	// its menu are painted.
	theme := `
#bb-cap-bar { background: #000; color: #ccc; right: 0; }
#bb-cap-bar button:hover { background: #222; border-radius: 3px; }
#bb-cap-bar nav { background: #000; border: 1px solid #333;
                  box-shadow: 0 2px 6px rgba(0,0,0,0.4); }
#bb-cap-bar nav a { color: #ccc; }
#bb-cap-bar nav a:hover { background: #222; }
`
	fill := "#ccc"
	if style == Caret {
		theme = `
#bb-cap-bar { background: transparent; color: #333; }
#bb-cap-bar button:hover { background: rgba(0,0,0,0.08); border-radius: 3px; }
#bb-cap-bar nav { background: #fff; border: 1px solid #d0d0d0;
                  box-shadow: 0 2px 8px rgba(0,0,0,0.18); border-radius: 3px; }
#bb-cap-bar nav a { color: #333; }
#bb-cap-bar nav a:hover { background: #f0f0f0; }
`
		fill = "#666"
	}

	return fmt.Sprintf(`<style>
#bb-cap-bar {
  position: fixed; top: 0; left: 0; height: 22px;
  box-sizing: border-box;
  display: flex; align-items: center; padding: 0 8px 0 2px;
  font-family: -apple-system, "Segoe UI", Roboto, sans-serif;
  z-index: 100;
}
#bb-cap-bar button {
  background: transparent; border: none; padding: 2px 6px;
  cursor: pointer; display: flex; align-items: center;
}
#bb-cap-bar svg { display: block; }
#bb-cap-bar nav {
  position: absolute; top: 22px; left: 0;
  min-width: 160px;
}
#bb-cap-bar nav[hidden] { display: none; }
#bb-cap-bar nav a {
  display: block; padding: 4px 14px;
  font-size: 14px; text-decoration: none;
}
%s</style>
<div id="bb-cap-bar">
  <button id="bb-ham" aria-label="Capabilities menu">
    <svg width="10" height="6" viewBox="0 0 10 6" xmlns="http://www.w3.org/2000/svg">
      <path d="M0 0 L10 0 L5 6 Z" fill="%s"/>
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
</script>`, theme, fill, dropdown.String())
}

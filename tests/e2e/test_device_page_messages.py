"""The messages a device page exchanges with bootstrap.js.

A device page runs in a sandboxed iframe: it cannot move the top-level tab
and never sees the access code, so it asks bootstrap.js, the top frame, by
postMessage. And bootstrap.js reports upload progress back to it on a
BroadcastChannel. Those names are an API between the server and this CLI,
and the two ship separately -- an installed CLI has to keep working against
a new server. They were renamed on the server once (2026-09-30), the rest
of this suite still passed, and the cap bar, the proxy page's Go button and
the file browser's progress bar all silently stopped working.

So each test drives the real control and checks for the effect only
bootstrap.js can produce:

    bb-open-cap       cap-bar menu entry -> a new tab at /<uid>#<code><path>
    bb-open-cap       proxy page's Go    -> this tab moves to the target
    uploadProgress    upload -> the file browser's progress text advances
    (and friends)
"""

import os

import pytest


def code_of(url):
    """The access code: everything in the fragment before any device path."""
    return url.split('#', 1)[1].split('/', 1)[0]


@pytest.fixture(scope='module')
def serve_listener(listener, test_server, tmp_path_factory):
    """Shell plus the dynamic proxy: what `bitbang serve` gives with no
    arguments. Its cap bar has a Proxy entry, and /proxy/ is the landing
    page with the target form."""
    home = str(tmp_path_factory.mktemp('device-page-messages'))
    return listener('serve', '-server', test_server, home=home)


def test_cap_bar_entry_opens_a_new_tab(serve_listener, browser_context):
    page = browser_context.new_page()
    page.goto(serve_listener.url, wait_until='networkidle')
    frame = page.frame_locator('#device-frame')
    frame.locator('#bb-ham').click(timeout=20000)

    with page.expect_popup(timeout=15000) as popup_info:
        frame.locator('#bb-menu a', has_text='Proxy').click()
    popup = popup_info.value

    # Only bootstrap.js knows the code, so a URL carrying it means the
    # message was heard and acted on -- not just that a link was followed.
    code = code_of(serve_listener.url)
    assert popup.url.endswith('#' + code + '/proxy/'), popup.url

    # And the tab it opened is a working session showing that page.
    popup.frame_locator('#device-frame').locator('#target').wait_for(timeout=30000)
    popup.close()
    page.close()


def test_proxy_go_moves_this_tab(serve_listener, target_app, browser_context):
    page = browser_context.new_page()
    page.goto(serve_listener.url + '/proxy/', wait_until='networkidle')
    frame = page.frame_locator('#device-frame')
    frame.locator('#target').fill(target_app, timeout=30000)
    frame.locator('button', has_text='Go').click()

    code = code_of(serve_listener.url)
    page.wait_for_url('**#' + code + '/' + target_app + '/', timeout=15000)

    # The tab came back up as a session on the target app.
    heading = page.frame_locator('#device-frame').locator('#heading')
    heading.wait_for(timeout=30000)
    assert heading.text_content() == 'Hello from Proxy Target'
    page.close()


@pytest.fixture(scope='module')
def upload_listener(listener, test_server, tmp_path_factory):
    home = str(tmp_path_factory.mktemp('device-page-upload'))
    shared = os.path.join(home, 'shared')
    os.makedirs(shared)
    return listener('serve', 'files', shared, '-files-upload',
                    '-server', test_server, '-ephemeral', home=home), shared


def test_upload_progress_reaches_the_file_browser(upload_listener, browser_context,
                                                  tmp_path):
    l, shared = upload_listener
    page = browser_context.new_page()
    page.goto(l.url, wait_until='networkidle')
    frame_el = page.frame_locator('#device-frame')
    frame_el.locator('body[data-upload-enabled="1"]').wait_for(timeout=20000)
    # From the element, not by URL: xhr-shim strips /__device__/<sid> out
    # of the frame's address.
    frame = page.locator('#device-frame').element_handle().content_frame()

    # Record, from inside the device page, what the progress channel
    # carried and what the page did with it. The page's own reaction is the
    # contract: #progress-size only moves past "0 B" if browse.html
    # recognized the message by name.
    frame.evaluate("""() => {
        window.__seen = [];
        window.__sizes = [];
        new BroadcastChannel('bitbang-progress').onmessage =
            (e) => window.__seen.push(e.data && e.data.type);
        new MutationObserver(() => window.__sizes.push(
            document.getElementById('progress-size').textContent))
            .observe(document.getElementById('progress-size'),
                     { childList: true, characterData: true, subtree: true });
    }""")

    payload = tmp_path / 'progress.bin'
    payload.write_bytes(os.urandom(256 * 1024))
    frame_el.locator('#file-input').set_input_files(str(payload))

    # The upload is finished when the file lands in the share.
    landed = os.path.join(shared, 'progress.bin')
    for _ in range(150):
        if os.path.exists(landed) and os.path.getsize(landed) == 256 * 1024:
            break
        page.wait_for_timeout(200)
    assert os.path.getsize(landed) == 256 * 1024

    frame.wait_for_function("window.__seen.includes('uploadSuccess')", timeout=15000)
    seen = frame.evaluate('window.__seen')
    sizes = frame.evaluate('window.__sizes')
    assert 'uploadProgress' in seen, seen
    assert 'uploadComplete' in seen, seen
    assert any(not s.startswith('0 B /') for s in sizes), \
        f'the file browser never showed progress past 0 B: {sizes}'
    page.close()

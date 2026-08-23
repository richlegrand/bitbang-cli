"""Access links: a scoped link reaches what it was granted and nothing else.

This is the browser half of the scope gate. The Go tests assert which
handlers get built; these assert what that means to someone holding the
URL -- a files-scoped link renders its file browser, and cannot reach a
LAN host through the proxy the way an unscoped one can.

Runs against a listener in dispatcher mode (plain `bitbang serve`, no
-target), which is where the local UI and the LAN proxy share the `http`
stream type and so where getting the split wrong would be invisible.
"""

import os

import pytest
from playwright.sync_api import expect


@pytest.fixture(scope='module')
def scoped(listener, test_server, target_app, tmp_path_factory):
    """An all-caps listener with one files-scoped link beside its own.

    Not -ephemeral: the link table lives next to a persistent identity, so
    this also covers the on-disk path -- load, mint, write back.
    """
    home = str(tmp_path_factory.mktemp('scoped-home'))
    shared = os.path.join(home, 'shared')
    os.makedirs(shared)
    with open(os.path.join(shared, 'notes.txt'), 'w') as f:
        f.write('secret plans\n')

    l = listener('serve', '-server', test_server, '-files', shared, home=home,
                 links=[{'label': 'contractor', 'scope': ['files']}])
    urls = l.await_links(['contractor'])
    assert 'owner' in urls
    return l, urls


def test_scoped_link_renders_its_own_ui(scoped, browser_context):
    """A files-scoped link still gets a browser UI. The listener's own web
    front rides on every link -- it is the shell the scopes act through --
    so scoping files must not leave the holder with a blank page."""
    _, urls = scoped
    page = browser_context.new_page()
    page.goto(urls['contractor'], wait_until='networkidle')
    frame = page.frame_locator('#device-frame')
    frame.locator('body').wait_for(timeout=20000)
    assert 'notes.txt' in frame.locator('body').inner_text()
    page.close()


def test_scoped_link_cannot_reach_a_lan_host(scoped, browser_context, target_app):
    """The proxy branch is the half `proxy` scope gates. Both branches
    report Type() == "http", so nothing but an end-to-end check proves the
    files-scoped link did not inherit the LAN proxy along with its UI."""
    _, urls = scoped
    page = browser_context.new_page()
    page.goto(f"{urls['contractor']}/{target_app}/", wait_until='networkidle')
    frame = page.frame_locator('#device-frame')
    frame.locator('body').wait_for(timeout=20000)
    body = frame.locator('body').inner_text()
    assert 'Hello from Proxy Target' not in body, (
        'a files-scoped link proxied to a LAN host; the dispatcher kept its '
        'proxy branch'
    )
    # Assert what it *is*, not only what it is not: with no proxy branch the
    # dispatcher falls to the local UI, whose mux has no route for a host:port
    # path. A blank or failed page would satisfy the check above on its own.
    assert '404' in body, f'expected the local UI to answer, got: {body[:200]!r}'
    page.close()


def test_unscoped_link_can_reach_a_lan_host(scoped, browser_context, target_app):
    """The control for the test above: the same path on the identity's own
    link does proxy. Without this, the assertion above would pass just as
    happily if the proxy were broken for everyone."""
    _, urls = scoped
    page = browser_context.new_page()
    page.goto(f"{urls['owner']}/{target_app}/", wait_until='networkidle')
    frame = page.frame_locator('#device-frame')
    heading = frame.locator('#heading')
    heading.wait_for(timeout=20000)
    assert heading.text_content() == 'Hello from Proxy Target'
    page.close()


def test_revoking_a_link_closes_the_browser_session(pty_listener, test_server,
                                                    browser_context, tmp_path_factory):
    """Deleting an entry closes the sessions using it, rather than only
    barring the next connection -- and the browser says why.

    Driven through the console's `rm`, which is how a link is revoked now
    that there is no reload signal. That also makes this the end-to-end
    test of a console command reaching a live browser session.

    The saying-why half is easy to get wrong invisibly. The device iframe
    is position:fixed, full viewport and opaque, so a message printed into
    #connection-ui underneath it cannot be seen; and the channel closing
    behind the message routes into the reconnect loop, which would leave
    "Reconnecting..." on screen for a session that ended on purpose. So
    assert what the user actually ends up looking at.
    """
    shared = str(tmp_path_factory.mktemp('revoke-share'))
    with open(os.path.join(shared, 'notes.txt'), 'w') as f:
        f.write('secret plans\n')

    l = pty_listener('serve', '-server', test_server, '-files', shared,
                     links=[{'label': 'contractor', 'scope': ['files']}])

    page = browser_context.new_page()
    page.goto(l.link_url('contractor'), wait_until='networkidle')
    page.frame_locator('#device-frame').locator('body').wait_for(timeout=20000)

    l.open_console()
    l.command('rm contractor', 'removed "contractor"')

    ui = page.locator('#connection-ui')
    expect(ui).to_contain_text('this link was revoked', timeout=30000)
    expect(page.locator('#bb-reload-btn')).to_be_visible()


# A forward-only link authorizes fine and has nothing to render: TCP
# forwarding is driven by `connect -L`. It used to answer a bare 404,
# which reads as a broken link rather than as "this one is for the CLI".
def test_forward_only_link_explains_itself(listener, test_server,
                                           browser_context, tmp_path_factory):
    home = str(tmp_path_factory.mktemp('fwd-home'))
    l = listener('serve', '-server', test_server, home=home,
                 links=[{'label': 'fwdonly', 'scope': ['forward']}])
    urls = l.await_links(['fwdonly'])

    page = browser_context.new_page()
    page.goto(urls['fwdonly'], wait_until='networkidle')
    frame = page.frame_locator('#device-frame')
    frame.locator('body').wait_for(timeout=20000)
    text = frame.locator('body').inner_text()

    assert '404' not in text, f'still a bare 404:\n{text[:200]}'
    assert 'forward' in text, f'does not say what the link grants:\n{text[:200]}'
    assert '-L' in text, f'does not show the CLI it is for:\n{text[:200]}'


# A link granting two capabilities has to offer a way between them. The
# strip used to live inside the shell launcher, and the launcher is only
# mounted when shell is granted -- so a files+proxy holder landed on
# files with no way to reach the proxy they had been given.
def test_multi_cap_link_without_shell_gets_the_cap_bar(listener, test_server,
                                                       browser_context,
                                                       tmp_path_factory):
    home = str(tmp_path_factory.mktemp('capbar-home'))
    shared = str(tmp_path_factory.mktemp('capbar-share'))
    with open(os.path.join(shared, 'notes.txt'), 'w') as f:
        f.write('hi\n')
    l = listener('serve', '-server', test_server, '-files', shared, home=home, links=[
        {'label': 'filesproxy', 'scope': ['files', 'proxy']},
        {'label': 'filesonly', 'scope': ['files']},
    ])
    urls = l.await_links(['filesproxy', 'filesonly'])

    page = browser_context.new_page()
    page.goto(urls['filesproxy'], wait_until='networkidle')
    frame = page.frame_locator('#device-frame')
    frame.locator('#bb-cap-bar').wait_for(timeout=20000)

    items = frame.locator('#bb-cap-bar nav a')
    labels = [items.nth(i).inner_text() for i in range(items.count())]
    assert labels == ['Files', 'Proxy'], labels
    # Shell is not granted, so it must not be offered.
    assert 'Shell' not in labels

    # The strip is a fixed overlay; the page has to make room or it sits
    # on top of the content.
    assert frame.locator('body').get_attribute('class') == 'with-cap-bar'
    bar = frame.locator('#bb-cap-bar').bounding_box()
    assert bar['height'] == 22, f'strip is {bar["height"]}px; every offset assumes 22'
    # Nothing shifts down for the caret: it is a corner control, and the
    # page's own margin clears it. What must hold is that the first row
    # is not underneath it -- horizontally or vertically.
    first = frame.locator('.container > *').first.bounding_box()
    clash = (bar['x'] + bar['width'] > first['x']) and (bar['y'] + bar['height'] > first['y'])
    assert not clash, f'caret ends at ({bar["x"]+bar["width"]},{bar["y"]+bar["height"]}), first row at ({first["x"]},{first["y"]})'
    assert first['y'] < 22, f'content pushed down to {first["y"]} for a corner control'


    # A light page gets the caret alone -- no band across it, and nothing
    # painted behind it. A full-width black strip here looked like a
    # rendering fault.
    page_width = frame.locator('body').bounding_box()['width']
    assert bar['width'] < page_width / 4, \
        f'caret is {bar["width"]} of {page_width}: that is a band, not a caret'
    bg = frame.locator('#bb-cap-bar').evaluate("e => getComputedStyle(e).backgroundColor")
    assert bg in ('rgba(0, 0, 0, 0)', 'transparent'), bg


# One capability has nowhere to go, so no strip and no wasted 22px.
def test_single_cap_link_has_no_cap_bar(listener, test_server, browser_context,
                                        tmp_path_factory):
    home = str(tmp_path_factory.mktemp('nobar-home'))
    shared = str(tmp_path_factory.mktemp('nobar-share'))
    with open(os.path.join(shared, 'notes.txt'), 'w') as f:
        f.write('hi\n')
    l = listener('serve', '-server', test_server, '-files', shared, home=home,
                 links=[{'label': 'filesonly', 'scope': ['files']}])
    urls = l.await_links(['filesonly'])

    page = browser_context.new_page()
    page.goto(urls['filesonly'], wait_until='networkidle')
    frame = page.frame_locator('#device-frame')
    frame.locator("text=notes.txt").wait_for(timeout=20000)
    assert frame.locator('#bb-cap-bar').count() == 0
    assert frame.locator('body').get_attribute('class') in (None, '')

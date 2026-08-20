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

    l = listener('serve', '-server', test_server, '-files', shared, home=home)
    urls = l.write_links([{'label': 'contractor', 'scope': ['files']}])
    assert 'contractor' in urls, f'link never appeared in the listing:\n{l.log()}'
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


def test_revoking_a_link_closes_the_browser_session(scoped, browser_context):
    """Deleting an entry closes the sessions using it, rather than only
    barring the next connection -- and the browser says why.

    The saying-why half is easy to get wrong invisibly. The device iframe
    is position:fixed, full viewport and opaque, so a message printed into
    #connection-ui underneath it cannot be seen; and the channel closing
    behind the message routes into the reconnect loop, which would leave
    "Reconnecting..." on screen for a session that ended on purpose. So
    assert what the user actually ends up looking at.
    """
    l, urls = scoped
    page = browser_context.new_page()
    page.goto(urls['contractor'], wait_until='networkidle')
    page.frame_locator('#device-frame').locator('body').wait_for(timeout=20000)

    l.write_links([])  # revoke every link but the implicit one
    l.wait_for(r'Closing .*link "contractor" was deleted', timeout=90)

    ui = page.locator('#connection-ui')
    expect(ui).to_contain_text('this link was revoked', timeout=30000)
    expect(page.locator('#bb-reload-btn')).to_be_visible()
    # The iframe has to be out of the way, or the message above is being
    # asserted on an element nobody can see.
    expect(page.locator('#device-frame')).to_be_hidden()
    page.close()

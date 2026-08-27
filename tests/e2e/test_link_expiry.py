"""A link that lapses.

Expiry closing a session that is already open is the part unit tests
cannot reach: it depends on the listener's one-minute poll noticing the
lapse and reaching into a live session, not on Terms.Check refusing the
next connection.
"""

import datetime
import os
import time

import pytest

# linkPoll in cmd/bitbang/serve_links.go is one minute, so a link that
# lapses shortly after it is used takes up to that long to be noticed.
POLL_SECONDS = 60


def _in(seconds):
    """An absolute UTC expiry, the way links.json spells it."""
    when = datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(seconds=seconds)
    return when.strftime('%Y-%m-%dT%H:%M:%SZ')


@pytest.fixture
def expiring(listener, test_server, tmp_path_factory):
    """Start a listener with a provisioned table. A factory rather than a
    fixed listener: the table has to exist before the process does, so
    each test supplies its own."""
    def start(entries):
        home = str(tmp_path_factory.mktemp('expiry-home'))
        shared = str(tmp_path_factory.mktemp('expiry-share'))
        with open(os.path.join(shared, 'hello.txt'), 'w') as f:
            f.write('still here\n')
        return listener('serve', 'shell', 'proxy', 'forward', 'files', shared, '-server', test_server,
                        home=home, links=entries)
    return start


# An already-lapsed link is retired on load: its code is cleared, so
# there is no URL to present in the first place. Stronger than refusing
# one, and it is why await_links cannot be used -- the entry will never
# have a URL to wait for.
def test_expired_link_is_retired_and_has_no_url(expiring):
    l = expiring([{'label': 'lapsed', 'grant': 'files', 'expires': _in(-3600)}])

    # Two lines per entry now: the label and its state on one, the URL
    # (or its absence) on the next.
    deadline = time.time() + 25
    head = tail = None
    while time.time() < deadline:
        lines = l.log().splitlines()
        for i, candidate in enumerate(lines):
            if 'lapsed' in candidate:
                head = candidate
                tail = lines[i + 1] if i + 1 < len(lines) else ''
        if head and 'expired' in head:
            break
        time.sleep(0.2)

    assert head, f'the expired link was never listed:\n{l.log()}'
    assert 'expired' in head, f'not marked expired: {head!r}'
    # Listed, but with nothing anyone could connect with.
    assert 'no code' in (tail or ''), f'an expired link still carries a code: {tail!r}'
    assert 'lapsed' not in l.urls_by_label(), 'an expired link produced a usable URL'


# The one that matters: connect while valid, keep the page open, and let
# the link lapse underneath it.
@pytest.mark.slow
@pytest.mark.timeout(240)
def test_expiry_closes_a_session_already_open(expiring, browser_context):
    l = expiring([{'label': 'shortlived', 'grant': 'files', 'expires': _in(45)}])
    urls = l.await_links(['shortlived'])

    page = browser_context.new_page()
    page.goto(urls['shortlived'], wait_until='networkidle')
    page.frame_locator('#device-frame').locator("text=hello.txt").wait_for(timeout=30000)

    # Live now. Wait out the expiry plus a full poll interval.
    time.sleep(45 + POLL_SECONDS + 15)

    log = l.log()
    assert any(word in log.lower() for word in ('expired', 'closing', 'closed')), \
        f'no sign the session was closed:\n{log}'

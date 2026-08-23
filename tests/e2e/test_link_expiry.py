"""A link that lapses while somebody is using it.

Expiry closing a *live* session is the headline of the access-links
work, and the part unit tests cannot reach: it depends on the listener's
one-minute poll noticing the lapse and reaching into an open session,
not on Terms.Check returning false the next time someone connects.
"""

import datetime
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
    home = str(tmp_path_factory.mktemp('expiry-home'))
    shared = str(tmp_path_factory.mktemp('expiry-share'))
    with open(shared + '/hello.txt', 'w') as f:
        f.write('still here\n')
    l = listener('serve', '-server', test_server, '-files', shared, home=home)
    return l


# An already-lapsed link is retired on load: its code is cleared, so
# there is no URL to present in the first place. Stronger than refusing
# one, and it is why write_links cannot be used here -- that helper waits
# for a URL the entry will never have.
def test_expired_link_is_retired_and_has_no_url(expiring):
    import json
    import os
    import signal

    os.makedirs(os.path.dirname(expiring.links_path), exist_ok=True)
    with open(expiring.links_path, 'w') as f:
        json.dump([{'label': 'lapsed', 'scope': ['files'], 'expires': _in(-3600)}], f)
    expiring.proc.send_signal(signal.SIGHUP)

    deadline = time.time() + 20
    line = None
    while time.time() < deadline:
        for candidate in expiring.log().splitlines():
            if 'lapsed' in candidate:
                line = candidate
        if line and 'expired' in line:
            break
        time.sleep(0.2)

    assert line, f'the expired link was never listed:\n{expiring.log()}'
    assert 'expired' in line, f'not marked expired: {line!r}'
    # Listed, but with nothing anyone could connect with.
    assert 'no code' in line, f'an expired link still carries a code: {line!r}'
    assert 'lapsed' not in expiring.urls_by_label(), \
        'an expired link produced a usable URL'


# The one that matters: connect while valid, keep the page open, and let
# the link lapse underneath it.
@pytest.mark.slow
@pytest.mark.timeout(240)
def test_expiry_closes_a_session_already_open(expiring, browser_context):
    urls = expiring.write_links([
        {'label': 'shortlived', 'scope': ['files'], 'expires': _in(30)},
    ])
    assert 'shortlived' in urls, f'link never listed:\n{expiring.log()}'

    page = browser_context.new_page()
    page.goto(urls['shortlived'], wait_until='networkidle')
    frame = page.frame_locator('#device-frame')
    frame.locator("text=hello.txt").wait_for(timeout=30000)

    # Live now. Wait out the expiry plus a full poll interval.
    time.sleep(30 + POLL_SECONDS + 15)

    log = expiring.log()
    assert 'shortlived' in log, 'the listener never mentioned the link again'
    # The listener must have closed it rather than leaving it open until
    # the browser happens to reconnect.
    assert any(word in log.lower() for word in ('expired', 'closing', 'closed')), \
        f'no sign the session was closed:\n{log}'

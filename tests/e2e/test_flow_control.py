"""SWSP v4 flow control in the browser.

The browser used to negotiate v3, which has no flow control: the listener
sent as fast as the network allowed, and whatever the page's reader did not
take piled up in the tab. In v4 every stream has credit in each direction,
returned as the bytes are consumed, so a slow reader slows the listener
instead -- and a listener reading from a target stops reading it, which TCP
carries back to the target.

That last step is what these observe, at the far end: the Flask target counts
what each /big stream has handed to its socket (target_app.py). Measured there
rather than in the page because the page cannot see the queue that matters --
it is in the service worker -- and because "the target stopped" is the claim.

A session the listener negotiates below v4 (a listener from before 0.5.0) has
none of this, and the tests that need it skip rather than fail, so the rest of
the module still runs against one. See bitbang-docs/browser-large-downloads.md,
section 10.
"""

import json
import os
import re
import time
import urllib.request

import pytest

TARGET = 'http://localhost:18080'

# How far ahead of a stopped reader the target may get. Not small: Chromium
# reads a page's fetch() body some 40-50 MB ahead of its reader before it
# stops pulling from the worker (measured 2026-10-18: the target sat at 58 MiB
# at 5, 15 and 30 s with the reader stopped after one read, and under 50 MB
# of that reached bootstrap.js), and the TCP and listener buffers add a few
# more. What flow control promises is that this is a bound -- the same for
# any size of body -- where without it the whole body goes.
READ_AHEAD_BOUND = 128 * 1024 * 1024


def _stats(stream_id):
    with urllib.request.urlopen(f'{TARGET}/stats?id={stream_id}') as r:
        return json.loads(r.read())


def _open(context, proxy_url):
    """The proxied page, and the SWSP version its session negotiated. The
    page's console is kept on page.bb_console: bootstrap.js logs every 50 MB
    a response brings in, which says how far a stream got past the device."""
    page = context.new_page()
    page.bb_console = []
    page.on('console', lambda m: page.bb_console.append(m.text))
    page.goto(proxy_url, wait_until='networkidle')
    frame = page.frame_locator('#device-frame')
    frame.locator('#heading').wait_for(timeout=30000)
    ready = [v for t in page.bb_console for v in re.findall(r'Device ready \(SWSP v(\d+)\)', t)]
    version = int(ready[-1]) if ready else 0
    return page, frame, version


def _arrived(page):
    """The last 'Download: N MB' bootstrap.js logged, or a note that it never
    logged one (fewer than 50 MB reached it)."""
    lines = [t for t in page.bb_console if t.startswith('Download:')]
    return lines[-1] if lines else 'under 50 MB reached bootstrap.js'


def _need_v4(version):
    if version < 4:
        pytest.skip(f'session negotiated SWSP v{version}; flow control needs v4')


def test_session_negotiates_v4(proxy_url, browser_context):
    """Against the listener under test, which speaks v4. Asserted, because
    every other test here proves nothing if the session quietly ran v3."""
    page, _, version = _open(browser_context, proxy_url)
    page.close()
    if os.environ.get('BITBANG_EXPECT_SWSP'):
        assert version == int(os.environ['BITBANG_EXPECT_SWSP'])
    else:
        assert version == 4, f'session negotiated v{version}'


def test_a_reader_that_stops_stops_the_target(proxy_url, browser_context):
    page, frame, version = _open(browser_context, proxy_url)
    _need_v4(version)

    # Read a 1 GB body slowly for 4 s, then not at all. The reader stays open,
    # and the stream with it.
    read = frame.locator('body').evaluate('''async () => {
        const res = await fetch('/big?id=slow&mb=1000');
        const reader = res.body.getReader();
        let got = 0;
        const until = Date.now() + 4000;
        while (Date.now() < until) {
            const { value, done } = await reader.read();
            if (done) break;
            got += value.byteLength;
            await new Promise(r => setTimeout(r, 100));
        }
        window.__slowReader = reader;  // keep it, unread
        return got;
    }''')
    time.sleep(4)
    first = _stats('slow')['yielded']
    time.sleep(10)
    second = _stats('slow')['yielded']
    arrived = _arrived(page)
    page.close()

    assert read > 0
    assert second <= READ_AHEAD_BOUND, (
        f'the target handed out {second / 2**20:.0f} MiB while the page read '
        f'{read / 2**20:.1f} MiB ({arrived}) -- nothing held it back')
    assert second == first, (
        f'the target kept sending to a reader that had stopped: '
        f'{first / 2**20:.0f} MiB, then {second / 2**20:.0f} MiB 10 s later')


def test_cancelling_the_reader_stops_the_target(proxy_url, browser_context):
    """A download cancelled in the browser used to keep arriving until the
    whole file had been sent. v4 resets the stream, the listener cancels its
    request to the target, and the target sees the connection go."""
    page, frame, version = _open(browser_context, proxy_url)
    _need_v4(version)

    frame.locator('body').evaluate('''async () => {
        const res = await fetch('/big?id=cancel&mb=500');
        const reader = res.body.getReader();
        for (let i = 0; i < 20; i++) await reader.read();
        await reader.cancel();
    }''')
    deadline = time.time() + 15
    while time.time() < deadline and not _stats('cancel').get('closed'):
        time.sleep(0.25)
    stats = _stats('cancel')
    page.close()

    assert stats.get('closed'), f'the target never saw the stream end: {stats}'
    assert stats['yielded'] < 100 * 1024 * 1024, stats


def _upload_digest(frame, size):
    """POST `size` random bytes from the page and return what the target
    received, beside what was sent."""
    # Locator.evaluate hands the element over first, and the argument second.
    return frame.locator('body').evaluate('''async (_, size) => {
        const body = new Uint8Array(size);
        for (let off = 0; off < size; off += 65536) {
            crypto.getRandomValues(body.subarray(off, Math.min(off + 65536, size)));
        }
        const want = Array.from(new Uint8Array(await crypto.subtle.digest('SHA-256', body)))
            .map(b => b.toString(16).padStart(2, '0')).join('');
        const res = await fetch('/api/digest', { method: 'POST', body });
        return { want, got: await res.json() };
    }''', size)


@pytest.mark.parametrize('size', [3 * 1024 * 1024, 16 * 1024 * 1024])
def test_an_upload_past_the_window_arrives_whole(proxy_url, browser_context, size):
    """Past the 1 MiB window, so it only completes if the browser waits for
    the listener's credit -- sending past it gets the stream reset."""
    page, frame, _ = _open(browser_context, proxy_url)
    r = _upload_digest(frame, size)
    page.close()
    assert r['got'] == {'length': size, 'sha256': r['want']}


def test_an_upload_past_the_window_arrives_whole_in_firefox(proxy_url, firefox_context):
    """Firefox has no Request.body, so the worker holds the whole body and
    hands it over in slices under the same credit."""
    page, frame, _ = _open(firefox_context, proxy_url)
    size = 8 * 1024 * 1024
    r = _upload_digest(frame, size)
    page.close()
    assert r['got'] == {'length': size, 'sha256': r['want']}


def test_a_websocket_message_past_the_window_round_trips(proxy_url, browser_context):
    """3 MiB each way. Out, it is sent in fragments that wait for credit;
    back, the fragments are credited as they join reassembly -- credited only
    on delivery, a message larger than the window could never finish."""
    page, frame, _ = _open(browser_context, proxy_url)
    result = frame.locator('body').evaluate('''() => new Promise((resolve, reject) => {
        const msg = 'x'.repeat(3 * 1024 * 1024);
        const ws = new WebSocket('ws://' + location.host + '/ws/echo');
        ws.onopen = () => ws.send(msg);
        ws.onmessage = (e) => { ws.close(); resolve(e.data === 'echo: ' + msg); };
        ws.onerror = () => reject('WebSocket error');
        setTimeout(() => reject('WebSocket timeout'), 30000);
    })''')
    page.close()
    assert result is True

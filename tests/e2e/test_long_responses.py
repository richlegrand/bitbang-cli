"""A response that outlives the service worker's idle timeout.

Every proxied response is streamed by the service worker, but its bytes reach
the worker from bootstrap.js over a MessagePort, and port traffic is not an
event: as far as the browser can tell, a worker relaying a download is idle.
bootstrap.js now sends the worker a keepalive message while a request is open.

The response here goes quiet for 45 seconds, so nothing at all reaches the
worker for longer than an idle timeout. That is the condition the keepalive's
timer exists for; while data is flowing the keepalive rides on arriving
frames instead, which test_browser_downloads.py covers.

Measured 2026-10-18 against a worker without the keepalive: Chromium kept it
alive through 45 s and through 330 s, so the 5-minute cap reported for Chrome
in 2017 no longer applies to a streaming worker. Firefox stopped it inside 45
s, and the response did not fail -- it ended, short, as if complete. That is
the case these tests exist for.

Runs against the deployed sw.js and bootstrap.js, so the Firefox test fails
against a server without the keepalive.
"""

import os

import pytest


GAP_SECONDS = 45
# Chrome is reported to stop a worker after 5 minutes even while it is
# streaming, keepalive or not. Long, so only on request.
LONG_GAP_SECONDS = int(os.environ.get('BITBANG_LONG_GAP', '0'))


def _survives_gap(context, proxy_url, gap):
    page = context.new_page()
    page.goto(proxy_url, wait_until='networkidle')
    frame = page.frame_locator('#device-frame')
    frame.locator('#heading').wait_for(timeout=30000)

    body = frame.locator('body').evaluate(
        f"async () => (await fetch('/slow?gap={gap}')).text()")
    page.close()
    assert body == 'first\nlast\n', (
        'the response did not survive the gap -- was the service worker '
        'stopped while it was relaying it?')


@pytest.mark.timeout(GAP_SECONDS + 75)
def test_response_survives_a_long_quiet_gap(proxy_url, browser_context):
    _survives_gap(browser_context, proxy_url, GAP_SECONDS)


@pytest.mark.timeout(GAP_SECONDS + 75)
def test_response_survives_a_long_quiet_gap_in_firefox(proxy_url, firefox_context):
    _survives_gap(firefox_context, proxy_url, GAP_SECONDS)


@pytest.mark.skipif(not LONG_GAP_SECONDS, reason='set BITBANG_LONG_GAP=<seconds> to run')
@pytest.mark.timeout(LONG_GAP_SECONDS + 75)
def test_response_survives_past_five_minutes(proxy_url, browser_context):
    _survives_gap(browser_context, proxy_url, LONG_GAP_SECONDS)

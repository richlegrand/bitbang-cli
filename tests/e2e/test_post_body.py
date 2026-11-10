"""Test POST body forwarding through the proxy."""

import json

from .frames import device_frame


def test_post_json_body(proxy_url, browser_context):
    """JSON POST body arrives at target through proxy."""
    page = browser_context.new_page()
    page.goto(proxy_url, wait_until='networkidle')

    frame = device_frame(page)
    frame.locator('#heading').wait_for(timeout=15000)

    result = frame.locator('body').evaluate('''() => {
        return fetch("/api/echo", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ test: "proxy" })
        }).then(r => r.json());
    }''')

    body = json.loads(result['echo'])
    assert body == {'test': 'proxy'}
    page.close()


def test_post_form_body(proxy_url, browser_context):
    """URL-encoded form body arrives at target through proxy."""
    page = browser_context.new_page()
    page.goto(proxy_url, wait_until='networkidle')

    frame = device_frame(page)
    frame.locator('#heading').wait_for(timeout=15000)

    result = frame.locator('body').evaluate('''() => {
        return fetch("/api/echo", {
            method: "POST",
            headers: { "Content-Type": "application/x-www-form-urlencoded" },
            body: "operation=read&key=value"
        }).then(r => r.json());
    }''')

    assert result['echo'] == 'operation=read&key=value'
    page.close()


def test_host_header_rewritten(proxy_url, browser_context):
    """Target sees its own Host header, not bitba.ng."""
    page = browser_context.new_page()
    page.goto(proxy_url, wait_until='networkidle')

    frame = device_frame(page)
    frame.locator('#heading').wait_for(timeout=15000)

    result = frame.locator('body').evaluate('''() => {
        return fetch("/api/headers").then(r => r.json());
    }''')

    # Host should be localhost:18080, not test.bitba.ng
    assert 'localhost' in result['host'] or '127.0.0.1' in result['host']
    page.close()

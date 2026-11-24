"""Test redirect handling through the proxy."""

from .frames import device_frame

def test_same_host_redirect(proxy_url, browser_context):
    """Same-host 302 redirect is passed to browser and resolved correctly."""
    page = browser_context.new_page()
    page.goto(proxy_url, wait_until='networkidle')

    frame = device_frame(page)
    frame.locator('#heading').wait_for(timeout=15000)

    # /redirect-test returns 302 -> /
    # The proxy should pass the redirect to the browser, which follows it
    result = frame.locator('body').evaluate('''() => {
        return fetch("/redirect-test", { redirect: "follow" })
            .then(r => r.text());
    }''')

    assert 'Hello from Proxy Target' in result
    page.close()

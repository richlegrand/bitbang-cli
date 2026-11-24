"""Test basic page load through the Go proxy."""

from .frames import device_frame

def test_proxy_page_loads(proxy_url, browser_context):
    """Local Flask app served through proxy via WebRTC."""
    page = browser_context.new_page()
    page.goto(proxy_url, wait_until='networkidle')

    frame = device_frame(page)
    heading = frame.locator('#heading')
    heading.wait_for(timeout=15000)

    assert heading.text_content() == 'Hello from Proxy Target'
    page.close()


def test_proxy_static_css(proxy_url, browser_context):
    """CSS loads through the proxy."""
    page = browser_context.new_page()
    page.goto(proxy_url, wait_until='networkidle')

    frame = device_frame(page)
    frame.locator('#heading').wait_for(timeout=15000)

    bg = frame.locator('body').evaluate('el => getComputedStyle(el).backgroundColor')
    assert '255, 255, 255' in bg
    page.close()


def test_proxy_static_js(proxy_url, browser_context):
    """JavaScript loads and executes through the proxy."""
    page = browser_context.new_page()
    page.goto(proxy_url, wait_until='networkidle')

    frame = device_frame(page)
    heading = frame.locator('#heading')
    heading.wait_for(timeout=15000)

    assert heading.get_attribute('data-loaded') == 'true'
    page.close()

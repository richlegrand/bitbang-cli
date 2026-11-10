"""Test WebSocket echo through the proxy (ws-shim.js)."""

from .frames import device_frame

def test_websocket_echo(proxy_url, browser_context):
    """WebSocket messages round-trip through the proxy via ws-shim."""
    page = browser_context.new_page()
    page.goto(proxy_url, wait_until='networkidle')

    frame = device_frame(page)
    frame.locator('#heading').wait_for(timeout=15000)

    # Open a WebSocket, send a message, verify the echo
    result = frame.locator('body').evaluate('''() => {
        return new Promise((resolve, reject) => {
            const ws = new WebSocket("ws://" + location.host + "/ws/echo");
            ws.onopen = () => {
                ws.send("hello proxy");
            };
            ws.onmessage = (e) => {
                ws.close();
                resolve(e.data);
            };
            ws.onerror = (e) => {
                reject("WebSocket error");
            };
            setTimeout(() => reject("WebSocket timeout"), 10000);
        });
    }''')

    assert result == 'echo: hello proxy'
    page.close()


def test_websocket_multiple_messages(proxy_url, browser_context):
    """Multiple WebSocket messages round-trip correctly."""
    page = browser_context.new_page()
    page.goto(proxy_url, wait_until='networkidle')

    frame = device_frame(page)
    frame.locator('#heading').wait_for(timeout=15000)

    result = frame.locator('body').evaluate('''() => {
        return new Promise((resolve, reject) => {
            const ws = new WebSocket("ws://" + location.host + "/ws/echo");
            const messages = [];
            let sent = 0;
            ws.onopen = () => {
                ws.send("msg1");
                ws.send("msg2");
                ws.send("msg3");
                sent = 3;
            };
            ws.onmessage = (e) => {
                messages.push(e.data);
                if (messages.length >= sent) {
                    ws.close();
                    resolve(messages);
                }
            };
            ws.onerror = () => reject("WebSocket error");
            setTimeout(() => reject("WebSocket timeout"), 10000);
        });
    }''')

    assert result == ['echo: msg1', 'echo: msg2', 'echo: msg3']
    page.close()

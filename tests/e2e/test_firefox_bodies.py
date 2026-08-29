"""Request bodies in a browser without `Request.body`.

Firefox does not implement `Request.body` in any version (Samsung Internet
gained it in 20, Chrome in 105), so a service worker there cannot stream the
request body and has to buffer it instead. That branch was missing: every POST
went out with no body at all and no error, which surfaced as a login failure
in whatever app was being proxied. The rest of the suite runs on Chromium and
saw none of it.

These run against the deployed `sw.js`, so they fail until a server carrying
the fix is deployed -- which is the point of having them.
"""

import json
import os


def test_firefox_really_lacks_request_streams(proxy_url, firefox_context):
    """The premise of this module, asserted rather than assumed.

    If Firefox ever ships `Request.body`, this fails and the tests below stop
    covering the buffered path -- at which point they are testing Chromium's
    path twice and someone should say so.
    """
    page = firefox_context.new_page()
    page.goto(proxy_url, wait_until='networkidle')
    frame = page.frame_locator('#device-frame')
    frame.locator('#heading').wait_for(timeout=30000)

    streams = frame.locator('body').evaluate(
        "() => !!(new Request('/x', {method: 'POST', body: 'ab'})).body")
    assert streams is False, (
        'Firefox now supports Request.body; this module no longer exercises '
        'the buffered path and should be re-pointed at a browser that does not'
    )
    page.close()


def test_json_post_body_arrives(proxy_url, firefox_context):
    """A JSON POST is the shape that broke: the app got an empty body and
    answered 400, which reads as bad credentials rather than a lost body."""
    page = firefox_context.new_page()
    page.goto(proxy_url, wait_until='networkidle')
    frame = page.frame_locator('#device-frame')
    frame.locator('#heading').wait_for(timeout=30000)

    result = frame.locator('body').evaluate('''() => {
        return fetch("/api/echo", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ test: "firefox" })
        }).then(r => r.json());
    }''')
    assert json.loads(result['echo']) == {'test': 'firefox'}
    page.close()


def test_binary_post_body_arrives_byte_exact(proxy_url, firefox_context):
    """Non-ASCII too, since buffering re-encodes and a length in characters
    rather than bytes would truncate exactly here."""
    page = firefox_context.new_page()
    page.goto(proxy_url, wait_until='networkidle')
    frame = page.frame_locator('#device-frame')
    frame.locator('#heading').wait_for(timeout=30000)

    payload = {'pw': 'sécret-🔑-' + 'x' * 500}
    result = frame.locator('body').evaluate('''(el, payload) => {
        return fetch("/api/echo", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify(payload)
        }).then(r => r.json());
    }''', payload)
    assert json.loads(result['echo']) == payload
    page.close()


def test_file_upload_is_not_silently_empty(listener, test_server, firefox_context,
                                           tmp_path_factory):
    """Upload failed the same way but worse: a 200 back and a zero-byte file,
    so it looked like it had worked."""
    home = str(tmp_path_factory.mktemp('ff-upload-home'))
    shared = os.path.join(home, 'shared')
    os.makedirs(shared)
    url = listener('serve', 'files', shared, '-files-upload',
                   '-server', test_server, '-ephemeral', home=home).url

    page = firefox_context.new_page()
    page.goto(url, wait_until='networkidle')
    frame = page.frame_locator('#device-frame')
    frame.locator('body').wait_for(timeout=30000)

    content = 'upload-through-firefox-' + 'y' * 300
    status = frame.locator('body').evaluate('''async (el, text) => {
        const fd = new FormData();
        fd.append('file', new Blob([text], {type: 'text/plain'}), 'probe.txt');
        const r = await fetch('api/upload?path=/', {method: 'POST', body: fd});
        return r.status;
    }''', content)
    assert status == 200, f'upload returned {status}'

    landed = os.path.join(shared, 'probe.txt')
    assert os.path.exists(landed), 'upload reported success but wrote no file'
    with open(landed) as f:
        assert f.read() == content, 'uploaded file does not match what was sent'
    page.close()

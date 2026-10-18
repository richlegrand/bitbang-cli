"""Clicking a file in the file browser hands it to the browser's download
manager.

The page used to read the whole response into memory and save it as a Blob at
the end, so a file had to fit in the tab: a 32 GB download stalled near 2 GB
and took Chrome down with it. Now the click is a navigation the browser saves
as it arrives, which puts three things on the line that a fetch() never
touched:

- the service worker has to route a request that names no frame, so the page
  builds a /__device__/<sid> URL for it;
- the saved name comes from Content-Disposition, not from the page;
- the worker injects its shims into any navigation that answers text/html, so
  a saved .html file must not be one.

Both engines, because a download is exactly the kind of thing they differ on.
"""

import hashlib
import os

import pytest


BIG_SIZE = 32 * 1024 * 1024
FILES = {
    'hello.txt': b'contents of hello\n',
    'page.html': b'<!DOCTYPE html><p>saved, not rendered</p>\n',
    'caf\xe9.txt': b'accented\n',
}
BIG_SHA256 = []  # filled in by the fixture that writes big.bin


@pytest.fixture(scope='module')
def downloads_url(listener, test_server, tmp_path_factory):
    home = str(tmp_path_factory.mktemp('downloads-home'))
    shared = os.path.join(home, 'shared')
    os.makedirs(shared)
    for name, body in FILES.items():
        with open(os.path.join(shared, name), 'wb') as f:
            f.write(body)
    # Random rather than zeros, so a dropped or repeated chunk shows up in
    # the hash instead of hiding in a run of identical bytes.
    big = os.urandom(BIG_SIZE)
    with open(os.path.join(shared, 'big.bin'), 'wb') as f:
        f.write(big)
    BIG_SHA256.append(_sha256(big))
    return listener('serve', 'files', shared, '-server', test_server,
                    '-ephemeral', home=home).url


def _open_browser(context, url):
    page = context.new_page()
    page.goto(url, wait_until='networkidle')
    frame = page.frame_locator('#device-frame')
    frame.locator('.file-item', has_text='hello.txt').wait_for(timeout=30000)
    return page, frame


def _click_download(page, frame, name, timeout=30000):
    with page.expect_download(timeout=timeout) as info:
        frame.locator('.file-item', has_text=name).click()
    download = info.value
    with open(download.path(), 'rb') as f:
        return download.suggested_filename, f.read()


def _sha256(data):
    return hashlib.sha256(data).hexdigest()


@pytest.mark.parametrize('name', sorted(FILES))
def test_click_saves_the_file(downloads_url, browser_context, name):
    page, frame = _open_browser(browser_context, downloads_url)
    saved_as, body = _click_download(page, frame, name)
    assert saved_as == name
    assert body == FILES[name], (
        'the saved file differs from the one shared'
        + (' -- shims injected into an HTML download?' if name.endswith('.html') else ''))
    page.close()


def test_click_saves_a_large_file_intact(downloads_url, browser_context):
    page, frame = _open_browser(browser_context, downloads_url)
    saved_as, body = _click_download(page, frame, 'big.bin', timeout=120000)
    assert saved_as == 'big.bin'
    assert len(body) == BIG_SIZE
    assert _sha256(body) == BIG_SHA256[0]
    page.close()


def test_page_stays_put_after_a_download(downloads_url, browser_context):
    """A download that the browser treated as a page instead would replace
    the file browser with the file."""
    page, frame = _open_browser(browser_context, downloads_url)
    _click_download(page, frame, 'hello.txt')
    assert frame.locator('.file-item', has_text='page.html').count() == 1
    page.close()


def test_a_refused_download_is_reported(downloads_url, browser_context):
    """The file can vanish between the listing and the click. The listener's
    404 lands in the hidden frame, where nobody would see it; the page has
    to say so."""
    page, frame = _open_browser(browser_context, downloads_url)
    messages = []
    page.on('dialog', lambda d: (messages.append(d.message), d.dismiss()))
    frame.locator('body').evaluate("() => downloadFile('gone.txt')")
    page.wait_for_timeout(5000)
    assert messages and messages[0].startswith('Failed to download file'), messages
    page.close()


@pytest.fixture(scope='module')
def send_url(listener, test_server, tmp_path_factory):
    """`serve files <file>`: one file, and the page with one button."""
    home = str(tmp_path_factory.mktemp('send-home'))
    path = os.path.join(home, 'page.html')
    with open(path, 'wb') as f:
        f.write(FILES['page.html'])
    return listener('serve', 'files', path, '-server', test_server,
                    '-ephemeral', home=home).url


def test_send_page_saves_the_file(send_url, browser_context):
    page = browser_context.new_page()
    page.goto(send_url, wait_until='networkidle')
    frame = page.frame_locator('#device-frame')
    button = frame.locator('#download-btn')
    button.wait_for(timeout=30000)
    with page.expect_download(timeout=30000) as info:
        button.click()
    with open(info.value.path(), 'rb') as f:
        assert f.read() == FILES['page.html']
    assert info.value.suggested_filename == 'page.html'
    page.close()


@pytest.mark.parametrize('name', ['hello.txt', 'page.html'])
def test_click_saves_the_file_in_firefox(downloads_url, firefox_context, name):
    page, frame = _open_browser(firefox_context, downloads_url)
    saved_as, body = _click_download(page, frame, name)
    assert saved_as == name
    assert body == FILES[name]
    page.close()

"""`bitbang share` seen from a real connector.

internal/share covers the worker, the role slots and tmux's attach
semantics, all in-process. What none of it covers is the seam: a real
`bitbang share` publishing a session, and a real `bitbang connect`
reaching it over signaling and WebRTC. Both sides can pass their own
tests while the join between them is broken.

Needs tmux; skipped without it.
"""

import os
import re
import shutil
import subprocess
import time

import pytest

pytestmark = pytest.mark.skipif(shutil.which('tmux') is None, reason='tmux not installed')

MARKER = 'MARKER-BEFORE-ANYONE-ATTACHED'


@pytest.fixture
def shared(tmp_path_factory, test_server):
    """A tmux session with something already on screen, published by a
    real `bitbang share`. Yields (control_url, view_url, tmux)."""
    pexpect = pytest.importorskip('pexpect')
    home = str(tmp_path_factory.mktemp('share-home'))
    label = 'bbe2e' + str(os.getpid())
    env = dict(os.environ, HOME=home)

    def tmux(*args, **kw):
        return subprocess.run(['tmux', '-L', label, *args], env=env,
                              capture_output=True, text=True, **kw)

    tmux('kill-server')
    tmux('new-session', '-d', '-s', 'demo', '-x', '80', '-y', '24')
    # On screen before any viewer exists -- an attach has to show it.
    tmux('send-keys', '-t', 'demo', f'echo {MARKER}', 'Enter')
    time.sleep(1)
    socket = tmux('display-message', '-p', '#{socket_path}').stdout.strip()

    proc = subprocess.Popen(
        [os.environ['BITBANG_BIN'], 'share', '-server', test_server,
         '-target', 'demo', '-socket', socket],
        env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)

    urls, deadline = {}, time.time() + 60
    while time.time() < deadline and len(urls) < 2:
        line = proc.stdout.readline()
        if not line:
            break
        m = re.search(r'(Control|View) URL:\s+(\S+)', line)
        if m:
            urls[m.group(1).lower()] = m.group(2)
    assert 'control' in urls and 'view' in urls, f'share never printed both URLs: {urls}'

    yield urls['control'], urls['view'], tmux

    proc.terminate()
    tmux('kill-server')


def _pane(tmux):
    return tmux('capture-pane', '-p', '-t', 'demo').stdout


def _connect(url, timeout=60):
    pexpect = pytest.importorskip('pexpect')
    return pexpect.spawn(os.environ['BITBANG_BIN'], ['connect', url],
                         encoding='utf-8', timeout=timeout, dimensions=(24, 80))


# The property tmux_integration_test.go asserts in-process, now over a
# real connection: attaching shows what was already on screen.
def test_viewer_sees_output_from_before_it_attached(shared):
    _, view_url, _ = shared
    c = _connect(view_url)
    try:
        c.expect(MARKER, timeout=60)
    finally:
        c.terminate(force=True)


# The positive control for the test below. Without it, "the viewer
# cannot type" would also pass if typing were broken for everyone.
def test_the_control_url_can_type(shared):
    control_url, _, tmux = shared
    c = _connect(control_url)
    try:
        c.expect(MARKER, timeout=60)
        c.sendline('echo TYPED-BY-CONTROL')
        deadline = time.time() + 30
        while time.time() < deadline:
            if 'TYPED-BY-CONTROL' in _pane(tmux):
                return
            time.sleep(0.5)
        pytest.fail(f'control keystrokes never reached tmux:\n{_pane(tmux)}')
    finally:
        c.terminate(force=True)


# The credential split is the whole point of two URLs, and it is
# enforced across the data channel rather than in the UI.
def test_the_view_url_cannot_type(shared):
    _, view_url, tmux = shared
    c = _connect(view_url)
    try:
        c.expect(MARKER, timeout=60)
        c.sendline('echo TYPED-BY-VIEWER')
        time.sleep(8)
        assert 'TYPED-BY-VIEWER' not in _pane(tmux), \
            f'a view-only URL typed into the session:\n{_pane(tmux)}'
    finally:
        c.terminate(force=True)

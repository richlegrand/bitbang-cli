"""A narrowed link, seen from a real connector.

The Go tests assert that the handlers are *built* narrowed: the TCP
handler's allowlist holds one target, the file share is rooted at the
subdirectory, the shell handler carries the pinned argv. That is
construction, not enforcement, and the two can come apart -- a handler
built with the right allowlist still has to consult it on the path a real
request takes.

So these connect for real, over signaling and WebRTC, with the CLI. Each
one narrows along a different axis and then asks for something the
listener itself can reach but the link should not:

  files    a sibling directory outside the link's root
  forward  a second port the listener forwards to
  shell    a command other than the one the link pinned

The owner link is the control in each case. Without it a test could pass
because the listener never served the thing at all, which is a different
bug wearing the same result.
"""

import json
import os
import socket
import subprocess
import threading
import time
from http.server import HTTPServer, BaseHTTPRequestHandler

import pytest

# Comfortably inside the suite's per-test timeout (60s in CI). A working
# connection is up in a few seconds; these ceilings only decide how long a
# broken one takes to report.
CONNECT_TIMEOUT = 40


def run_cli(bitbang_bin, home, *args, timeout=CONNECT_TIMEOUT):
    """Run the CLI against a listener and return (rc, combined output).

    A fresh HOME per call: `connect` saves the device it reached, and a
    saved name resolving differently later would make these tests depend
    on each other's order.
    """
    return _run(subprocess.run(
        [bitbang_bin, *args], env=dict(os.environ, HOME=home),
        capture_output=True, text=True, timeout=timeout))


def _run(p):
    return p.returncode, (p.stdout or '') + (p.stderr or '')


@pytest.fixture
def connector_home(tmp_path_factory):
    return str(tmp_path_factory.mktemp('connector'))


# -- files: narrowed to a subdirectory --

@pytest.fixture(scope='module')
def files_narrowed(listener, test_server, tmp_path_factory):
    """A listener sharing a tree, and a link rooted one level down."""
    home = str(tmp_path_factory.mktemp('narrow-files-home'))
    shared = os.path.join(home, 'share')
    os.makedirs(os.path.join(shared, 'public'))
    os.makedirs(os.path.join(shared, 'private'))
    with open(os.path.join(shared, 'public', 'ok.txt'), 'w') as f:
        f.write('public file')
    with open(os.path.join(shared, 'private', 'secret.txt'), 'w') as f:
        f.write('SECRET')

    l = listener('serve', 'files', shared, '-server', test_server, home=home,
                 links=[{'label': 'narrow',
                         'grant': 'files ' + os.path.join(shared, 'public')}])
    l.await_links(['narrow'])
    return l, l.urls_by_label()


def test_narrowed_files_link_reads_inside_its_root(files_narrowed, bitbang_bin,
                                                   connector_home):
    _, urls = files_narrowed
    rc, out = run_cli(bitbang_bin, connector_home, 'cp', urls['narrow'] + ':/ok.txt', '-')
    assert rc == 0 and 'public file' in out, out


def test_narrowed_files_link_cannot_read_a_sibling(files_narrowed, bitbang_bin,
                                                   connector_home):
    """The sibling is not merely hidden from the listing: the link's share
    is rooted at the subdirectory, so the path does not resolve at all."""
    _, urls = files_narrowed
    rc, out = run_cli(bitbang_bin, connector_home, 'cp',
                      urls['narrow'] + ':/private/secret.txt', '-')
    assert rc != 0, 'a link narrowed to public/ read a file outside it'
    assert 'SECRET' not in out, out


def test_owner_link_still_reads_the_sibling(files_narrowed, bitbang_bin,
                                            connector_home):
    """The control: the listener does serve that file, so the refusal
    above is the link narrowing and not the file being unreachable."""
    _, urls = files_narrowed
    rc, out = run_cli(bitbang_bin, connector_home, 'cp',
                      urls['owner'] + ':/private/secret.txt', '-')
    assert rc == 0 and 'SECRET' in out, out


# -- forward: narrowed to one of two targets --

class _Hello(BaseHTTPRequestHandler):
    def do_GET(self):
        body = self.server.body.encode()
        self.send_response(200)
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *a):
        pass


def _serve_http(body):
    """A throwaway HTTP server on a free port, returning (port, stop)."""
    srv = HTTPServer(('127.0.0.1', 0), _Hello)
    srv.body = body
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv.server_port, srv.shutdown


def _free_port():
    with socket.socket() as s:
        s.bind(('127.0.0.1', 0))
        return s.getsockname()[1]


@pytest.fixture(scope='module')
def forward_narrowed(listener, test_server, tmp_path_factory):
    """A listener forwarding to two ports, and a link holding only one."""
    allowed_port, stop_a = _serve_http('ALLOWED-TARGET')
    denied_port, stop_b = _serve_http('DENIED-TARGET')
    home = str(tmp_path_factory.mktemp('narrow-fwd-home'))

    allowed = '127.0.0.1:%d' % allowed_port
    denied = '127.0.0.1:%d' % denied_port
    l = listener('serve', 'forward', '%s,%s' % (allowed, denied),
                 '-server', test_server, home=home,
                 links=[{'label': 'narrow', 'grant': 'forward ' + allowed}])
    l.await_links(['narrow'])
    yield l, l.urls_by_label(), allowed, denied
    stop_a()
    stop_b()


def _forward_and_fetch(bitbang_bin, home, url, target):
    """Hold a forward, fetch through it, and return (body, connector output)."""
    local = _free_port()
    p = subprocess.Popen(
        [bitbang_bin, 'connect', url, '-L', '%d:%s' % (local, target)],
        env=dict(os.environ, HOME=home),
        stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    body = ''
    try:
        deadline = time.time() + 20
        while time.time() < deadline:
            try:
                with socket.create_connection(('127.0.0.1', local), timeout=5) as s:
                    s.sendall(b'GET / HTTP/1.0\r\nHost: x\r\n\r\n')
                    # To EOF, not one recv: the body arrives in a separate
                    # segment from the headers, and reading once returns a
                    # 200 with nothing in it.
                    chunks = []
                    while True:
                        chunk = s.recv(4096)
                        if not chunk:
                            break
                        chunks.append(chunk)
                    body = b''.join(chunks).decode('utf-8', 'replace')
                if body:
                    break
            except OSError:
                pass
            if p.poll() is not None:
                break
            time.sleep(0.5)
    finally:
        p.terminate()
        try:
            out = p.communicate(timeout=10)[0] or ''
        except subprocess.TimeoutExpired:
            p.kill()
            out = p.communicate()[0] or ''
    return body, out


def test_narrowed_forward_link_reaches_its_target(forward_narrowed, bitbang_bin,
                                                  connector_home):
    _, urls, allowed, _ = forward_narrowed
    body, out = _forward_and_fetch(bitbang_bin, connector_home, urls['narrow'], allowed)
    assert 'ALLOWED-TARGET' in body, 'forward to the granted target failed:\n' + out


def test_narrowed_forward_link_refuses_the_other_target(forward_narrowed, bitbang_bin,
                                                        connector_home):
    """The listener forwards to this port. The link does not, and the
    refusal has to name what the link does reach rather than blaming the
    listener for a restriction it did not impose."""
    _, urls, allowed, denied = forward_narrowed
    body, out = _forward_and_fetch(bitbang_bin, connector_home, urls['narrow'], denied)
    assert 'DENIED-TARGET' not in body, 'a link narrowed away from this port still reached it'
    assert 'not one of the allowed forwards for your link' in out, out
    assert allowed in out, 'the refusal does not say what the link can reach:\n' + out


def test_owner_link_still_reaches_both(forward_narrowed, bitbang_bin, connector_home):
    """The control, again: the listener really does forward to the port
    the narrowed link was refused."""
    _, urls, _, denied = forward_narrowed
    body, out = _forward_and_fetch(bitbang_bin, connector_home, urls['owner'], denied)
    assert 'DENIED-TARGET' in body, 'the listener does not forward here at all:\n' + out


# -- shell: pinned by the link --

@pytest.fixture(scope='module')
def shell_narrowed(listener, test_server, tmp_path_factory):
    """A listener whose shell is open, and a link pinning one command."""
    home = str(tmp_path_factory.mktemp('narrow-shell-home'))
    l = listener('serve', 'shell', '-server', test_server, home=home,
                 links=[{'label': 'narrow', 'grant': 'shell "/bin/echo pinned-output"'}])
    l.await_links(['narrow'])
    return l, l.urls_by_label()


def test_pinned_link_runs_its_command(shell_narrowed, bitbang_bin, connector_home):
    _, urls = shell_narrowed
    rc, out = run_cli(bitbang_bin, connector_home, 'connect', urls['narrow'])
    assert 'pinned-output' in out, out


def test_pinned_link_refuses_another_command(shell_narrowed, bitbang_bin,
                                             connector_home):
    """A command the link pinned is not a default. Refusing rather than
    silently running the pin: a connector that asked for something else
    must not read the pin's output believing it got what it asked for."""
    _, urls = shell_narrowed
    rc, out = run_cli(bitbang_bin, connector_home, 'connect', urls['narrow'],
                      '--', 'echo', 'connector-chose-this')
    assert 'connector-chose-this' not in out, 'the link pin did not hold'
    assert 'does not accept one' in out, out


def test_open_listener_still_takes_a_command(shell_narrowed, bitbang_bin,
                                             connector_home):
    """The control: the listener named no command, so the owner link is
    still an ordinary shell."""
    _, urls = shell_narrowed
    rc, out = run_cli(bitbang_bin, connector_home, 'connect', urls['owner'],
                      '--', 'echo', 'connector-chose-this')
    assert rc == 0 and 'connector-chose-this' in out, out

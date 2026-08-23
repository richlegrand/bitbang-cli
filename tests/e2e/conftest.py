"""Pytest fixtures for BitBangProxy E2E tests.

Starts:
1. A local Flask target app on localhost:18080
2. One or more bitbang listeners connecting to test.bitba.ng
3. Provides their URLs to tests

`listener` is the general fixture: it starts any listener mode and hands back
a handle that knows its URL, its log, its HOME (so a test can write a
links.json), and how to reload it. `proxy_url` is the fixed-target proxy case
kept as its own fixture because most tests want just the URL.
"""

import json
import os
import pytest
import queue
import re
import subprocess
import sys
import threading
import time

TEST_SERVER = os.environ.get('BITBANG_TEST_SERVER', 'test.bitba.ng')
TARGET_PORT = 18080
PROXY_STARTUP_TIMEOUT = 30


def bitbang_binary():
    """Path to the binary under test, or skip."""
    repo_dir = os.path.dirname(os.path.dirname(os.path.dirname(__file__)))
    default_name = 'bitbangproxy.exe' if os.name == 'nt' else 'bitbangproxy'
    proxy_bin = os.environ.get('BITBANG_BIN', os.path.join(repo_dir, default_name))
    if not os.path.isfile(proxy_bin):
        pytest.fail(
            f'BitBangProxy binary not found at {proxy_bin}. '
            f'Run: go build -o {default_name} ./cmd/bitbang/'
        )
    return proxy_bin


class Listener:
    """A running bitbang listener, and the handful of things a test needs
    from it: its URL, its output, and (for link tests) its HOME, so the
    link table can be written where the listener will look for it."""

    def __init__(self, proc, home, lines, captured):
        self.proc = proc
        self.home = home
        self._lines = lines
        self._captured = captured
        self.url = None

    def log(self):
        """Everything the listener has printed so far."""
        self._drain()
        return ''.join(self._captured)

    def _drain(self):
        while True:
            try:
                line = self._lines.get_nowait()
            except queue.Empty:
                return
            if line is None:
                return
            self._captured.append(line)
            print(f'[listener] {line.rstrip()}')

    def wait_for(self, pattern, timeout=15):
        """Block until a line matches, returning the match. Beats sleeping:
        reload and revocation both announce themselves in the log."""
        deadline = time.time() + timeout
        while time.time() < deadline:
            self._drain()
            m = re.search(pattern, ''.join(self._captured))
            if m:
                return m
            time.sleep(0.1)
        pytest.fail(f'listener never printed {pattern!r}. Output:\n{self.log()}')

    @property
    def links_path(self):
        return os.path.join(self.home, '.bitbang', 'bitbang', 'links.json')

    def await_links(self, labels):
        """Block until every label appears in the listing, then return
        {label: url}.

        Waiting on the labels rather than on "some new output" matters:
        the listener prints its pairing code asynchronously a moment
        after Ready, so any-new-line is satisfied before the table has
        been printed.
        """
        wanted = set(labels)
        if not wanted:
            return self.urls_by_label()
        deadline = time.time() + 20
        while time.time() < deadline:
            urls = self.urls_by_label()
            if wanted <= set(urls):
                return urls
            time.sleep(0.2)
        pytest.fail(
            f'links {sorted(wanted)} never appeared in the listing. '
            f'Output:\n{self.log()}'
        )

    def urls_by_label(self):
        """Parse the listing into {label: url}. The listing is the listener's
        own view of the table, so reading it back is also a check that the
        table loaded.

        Two lines per entry: `N) label ...` on one, the URL alone on the
        next. An entry whose code has been retired has "(no code until
        renewed)" there instead and is left out, since there is no URL to
        return."""
        self._drain()
        found, pending = {}, None
        for line in self._captured:
            url = re.match(r'\s+(https://\S+)\s*$', line)
            if url and pending:
                found[pending] = url.group(1)
                pending = None
                continue
            head = re.match(r'\s{2}\d+\)\s+(\S+)\s+\S', line)
            pending = head.group(1) if head else None
        return found


def _start_listener(args, home, timeout=PROXY_STARTUP_TIMEOUT):
    """Spawn a listener and wait for it to print a URL and report Ready.

    Reads stdout on a background thread and polls with a real deadline: a
    blocking readline() would let a silent or stuck listener (e.g. unable to
    register with the signaling server in CI) hang the whole job.
    """
    env = dict(os.environ, HOME=home)
    proc = subprocess.Popen(
        [bitbang_binary()] + args,
        stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, env=env,
    )
    lines = queue.Queue()

    def _pump():
        for line in proc.stdout:
            lines.put(line)
        lines.put(None)  # EOF sentinel

    threading.Thread(target=_pump, daemon=True).start()

    listener = Listener(proc, home, lines, [])
    ready = False
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            line = lines.get(timeout=max(0.1, deadline - time.time()))
        except queue.Empty:
            break
        if line is None:  # exited before becoming ready
            break
        listener._captured.append(line)
        print(f'[listener] {line.rstrip()}')
        # The CLI prints the device URL on a "URL: https://..." line and a
        # separate "Ready" status line.
        m = re.search(r'URL:\s*(https://\S+)', line)
        if m:
            listener.url = m.group(1)
        if re.search(r'\bReady\b', line):
            ready = True
        if listener.url and ready:
            break

    if not (listener.url and ready):
        proc.kill()
        pytest.fail(
            f'Listener {args} did not become ready within {timeout}s '
            f'(url={listener.url!r}, ready={ready}, server={TEST_SERVER}). '
            f'Output:\n{listener.log()}'
        )
    print(f'[listener] {args[:2]} URL: {listener.url}')
    return listener


def _stop(listener):
    listener.proc.terminate()
    try:
        listener.proc.wait(timeout=5)
    except subprocess.TimeoutExpired:
        listener.proc.kill()


def _provision_links(home, entries):
    """Write a link table into a home before its listener starts.

    Provisioned rather than pushed: a running listener has no reload
    signal, by design. Changing links while one is up is the console's
    job, which is what the revocation test drives.
    """
    path = os.path.join(home, '.bitbang', 'bitbang', 'links.json')
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, 'w') as f:
        json.dump(entries, f)


def pytest_configure(config):
    config.addinivalue_line(
        'markers', 'slow: takes tens of seconds by design, e.g. waiting out a timeout')


class PtyListener:
    """A listener running under a pty, so it has a console to prompt on.

    The plain `listener` fixture uses pipes, which is right for tests that
    only need a URL -- but the console opens /dev/tty and refuses to exist
    without one, so anything exercising it needs a real terminal.
    """

    def __init__(self, child, home, pairing_code, device_url):
        self.child = child
        self.home = home
        self.pairing_code = pairing_code
        self.device_url = device_url

    def open_console(self):
        """Press Enter and wait for the console to come up."""
        self.child.sendline('')
        self.child.expect('console --', timeout=20)

    def command(self, line, expect, timeout=20):
        """Run one console command and wait for something in its output.

        Waits for the terminal to echo the line back before matching, so
        a pattern cannot be satisfied by output that was already in the
        buffer when the command was sent.
        """
        self.child.sendline(line)
        if line:
            self.child.expect_exact(line, timeout=timeout)
        self.child.expect(expect, timeout=timeout)
        return self.child.match

    def links(self):
        path = os.path.join(self.home, '.bitbang', 'bitbang', 'links.json')
        if not os.path.exists(path):
            return []
        with open(path) as f:
            return json.load(f)

    def link_url(self, label):
        """The shareable URL for a provisioned link.

        Built from the code in links.json plus the device URL the banner
        printed, rather than scraped from the listing -- the listing is
        column-aligned and its URL column moves with the longest label.
        """
        for e in self.links():
            if e.get('label') == label and e.get('code'):
                base = self.device_url.split('#')[0]
                return f"{base}#{e['code']}"
        raise AssertionError(f'no link called {label!r} in {self.links()}')


@pytest.fixture
def pty_listener(tmp_path_factory):
    """Factory for listeners with a terminal attached.

    Function-scoped: console tests mutate the link table, and a shared
    listener would make them order-dependent.
    """
    pexpect = pytest.importorskip('pexpect')
    started = []

    def start(*args, links=None):
        home = str(tmp_path_factory.mktemp('pty-home'))
        os.makedirs(os.path.join(home, 'share'), exist_ok=True)
        if links is not None:
            _provision_links(home, links)
        child = pexpect.spawn(
            bitbang_binary(), list(args),
            env=dict(os.environ, HOME=home, TERM='dumb'),
            encoding='utf-8', timeout=60, dimensions=(50, 110),
        )
        # The URL is printed before the pairing code, so it has to be
        # matched first or it is already consumed. The listener also
        # suppresses its "Ready" marker on a terminal, which is why the
        # banner is the registration signal here rather than that.
        child.expect(r'https://\S+/[A-Za-z0-9_-]{22}#[A-Za-z0-9_-]+',
                     timeout=PROXY_STARTUP_TIMEOUT + 15)
        device_url = child.after.strip()
        child.expect('Pairing code:', timeout=30)
        child.expect(r'(\d{6})', timeout=10)
        code = child.match.group(1)
        child.expect(pexpect.TIMEOUT, timeout=3)  # let the banner settle
        l = PtyListener(child, home, code, device_url)
        started.append(l)
        return l

    yield start
    for l in started:
        l.child.terminate(force=True)


@pytest.fixture(scope='session')
def bitbang_bin():
    """Path to the binary under test. A fixture rather than an import so
    tests do not have to reach into conftest as a module."""
    return bitbang_binary()


@pytest.fixture(scope='session')
def test_server():
    """The signaling server under test. A fixture rather than an import so
    tests do not have to reach into conftest as a module."""
    return TEST_SERVER


@pytest.fixture(scope='session')
def listener(tmp_path_factory):
    """Factory: start a listener in any mode, cleaned up at session end.

    Each listener gets its own HOME so identities and link tables do not
    collide, and so a test that writes links.json cannot disturb the
    developer's real one.
    """
    started = []

    def start(*args, home=None, links=None):
        if home is None:
            home = str(tmp_path_factory.mktemp('home'))
        if links is not None:
            _provision_links(home, links)
        l = _start_listener(list(args), home)
        started.append(l)
        return l

    yield start
    for l in started:
        _stop(l)


@pytest.fixture(scope='session')
def target_app():
    """Start the local Flask target app."""
    target_script = os.path.join(os.path.dirname(__file__), 'target_app.py')
    proc = subprocess.Popen(
        [sys.executable, target_script],
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
    )

    # Wait for Flask to start
    time.sleep(1)
    if proc.poll() is not None:
        output = proc.stdout.read()
        pytest.fail(f'Target app failed to start:\n{output}')

    yield f'localhost:{TARGET_PORT}'

    proc.terminate()
    try:
        proc.wait(timeout=5)
    except subprocess.TimeoutExpired:
        proc.kill()


@pytest.fixture(scope='session')
def proxy_url(target_app, listener):
    """Start a fixed-target proxy listener and return its URL. This is the
    mode the OctoPrint plugin runs, and what most tests here exercise."""
    return listener('serve', 'proxy', '-server', TEST_SERVER,
                    '-target', target_app, '-ephemeral').url


@pytest.fixture(scope='session')
def browser_context(playwright, proxy_url):
    """Create a persistent browser context for the test session."""
    browser = playwright.chromium.launch(headless=True)
    context = browser.new_context()
    yield context
    context.close()
    browser.close()

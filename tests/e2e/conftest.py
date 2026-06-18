"""Pytest fixtures for BitBangProxy E2E tests.

Starts:
1. A local Flask target app on localhost:18080
2. BitBangProxy connecting to test.bitba.ng, targeting localhost:18080
3. Provides the proxy URL to tests
"""

import pytest
import subprocess
import time
import sys
import os
import re
import signal
import threading
import queue

TEST_SERVER = os.environ.get('BITBANG_TEST_SERVER', 'test.bitba.ng')
TARGET_PORT = 18080
PROXY_STARTUP_TIMEOUT = 30


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
def proxy_url(target_app):
    """Start BitBangProxy targeting the local Flask app and return its URL."""
    repo_dir = os.path.dirname(os.path.dirname(os.path.dirname(__file__)))
    proxy_bin = os.path.join(repo_dir, 'bitbangproxy')

    if not os.path.isfile(proxy_bin):
        pytest.fail(f'BitBangProxy binary not found at {proxy_bin}. Run: go build -o bitbangproxy ./cmd/bitbang/')

    proc = subprocess.Popen(
        [proxy_bin, 'serve', 'proxy', '-server', TEST_SERVER, '-target', target_app, '-ephemeral'],
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
    )

    # Wait for the "Ready: https://..." line. Read stdout on a background
    # thread and poll with a real deadline -- a blocking readline() here would
    # let a silent/stuck proxy (e.g. unable to register with the signaling
    # server in CI) hang the whole job past PROXY_STARTUP_TIMEOUT.
    lines = queue.Queue()

    def _pump():
        for line in proc.stdout:
            lines.put(line)
        lines.put(None)  # EOF sentinel

    threading.Thread(target=_pump, daemon=True).start()

    url = None
    ready = False
    captured = []
    deadline = time.time() + PROXY_STARTUP_TIMEOUT
    while time.time() < deadline:
        try:
            line = lines.get(timeout=max(0.1, deadline - time.time()))
        except queue.Empty:
            break
        if line is None:  # proxy exited before becoming ready
            break
        captured.append(line)
        print(f'[proxy] {line.rstrip()}')
        # Current CLI prints the share URL on a "URL: https://..." line and a
        # separate "Ready" status line. Capture the URL, then proceed once the
        # proxy reports Ready.
        m = re.search(r'URL:\s*(https://\S+)', line)
        if m:
            url = m.group(1)
        if re.search(r'\bReady\b', line):
            ready = True
        if url and ready:
            break

    if not (url and ready):
        proc.kill()
        pytest.fail(
            f'Proxy did not become ready within {PROXY_STARTUP_TIMEOUT}s '
            f'(url={url!r}, ready={ready}, server={TEST_SERVER}). '
            f'Output:\n{"".join(captured)}'
        )

    print(f'[proxy] URL: {url}')
    yield url

    proc.terminate()
    try:
        proc.wait(timeout=5)
    except subprocess.TimeoutExpired:
        proc.kill()


@pytest.fixture(scope='session')
def browser_context(playwright, proxy_url):
    """Create a persistent browser context for the test session."""
    browser = playwright.chromium.launch(headless=True)
    context = browser.new_context()
    yield context
    context.close()
    browser.close()

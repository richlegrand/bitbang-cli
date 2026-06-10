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

TEST_SERVER = os.environ.get('BITBANG_TEST_SERVER', 'test.bitba.ng')
TARGET_PORT = 18080
PROXY_STARTUP_TIMEOUT = 15


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

    # Wait for the "Ready: https://..." line
    url = None
    deadline = time.time() + PROXY_STARTUP_TIMEOUT
    while time.time() < deadline:
        line = proc.stdout.readline()
        if not line:
            if proc.poll() is not None:
                break
            continue
        print(f'[proxy] {line.rstrip()}')
        match = re.search(r'Ready: (https://\S+)', line)
        if match:
            url = match.group(1)
            break

    if url is None:
        proc.kill()
        output = proc.stdout.read()
        pytest.fail(f'Proxy failed to start. Output:\n{output}')

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

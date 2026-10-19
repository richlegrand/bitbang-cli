"""Local Flask app used as a proxy target for E2E tests.
Runs on localhost:18080. BitBangProxy proxies requests to this."""

from flask import Flask, request, make_response, Response
from flask_sock import Sock
import hashlib
import json
import time

app = Flask(__name__)
sock = Sock(app)

@app.route('/')
def index():
    return '''<!DOCTYPE html>
<html>
<head><title>Proxy Test</title>
<link rel="stylesheet" href="/style.css">
<script src="/app.js"></script>
</head>
<body>
<h1 id="heading">Hello from Proxy Target</h1>
</body>
</html>'''

@app.route('/style.css')
def style():
    return Response('body { background: white; }', mimetype='text/css')

@app.route('/app.js')
def script():
    return Response('document.addEventListener("DOMContentLoaded", function() { '
                    'document.getElementById("heading").dataset.loaded = "true"; });',
                    mimetype='application/javascript')

@app.route('/api/echo', methods=['POST'])
def echo():
    data = request.get_data(as_text=True)
    return json.dumps({
        'echo': data,
        'content_type': request.content_type,
        'host': request.headers.get('Host', ''),
    })

@app.route('/api/headers')
def headers():
    return json.dumps({
        'host': request.headers.get('Host', ''),
        'referer': request.headers.get('Referer', ''),
    })

@app.route('/login', methods=['POST'])
def login():
    resp = make_response(json.dumps({'status': 'ok'}))
    resp.set_cookie('session', 'proxy-session-456', path='/')
    return resp

@app.route('/protected')
def protected():
    session = request.cookies.get('session')
    if session:
        return json.dumps({'status': 'ok', 'session': session})
    return json.dumps({'status': 'unauthorized'}), 401

@app.route('/sse')
def sse():
    def generate():
        for i in range(3):
            yield f'data: proxy message {i}\n\n'
            time.sleep(0.1)
    return Response(generate(), mimetype='text/event-stream')

@app.route('/slow')
def slow():
    """A response that goes quiet mid-body for ?gap= seconds. Nothing reaches
    the service worker during the gap, which is the condition under which a
    browser stops an idle worker -- see test_long_responses.py."""
    gap = float(request.args.get('gap', '45'))
    def generate():
        yield 'first\n'
        time.sleep(gap)
        yield 'last\n'
    return Response(generate(), mimetype='text/plain')

# What each /big stream has handed to the server so far, by id, for
# test_flow_control.py. The server writes each chunk to its socket before the
# generator resumes, so 'yielded' is what has left this process: with flow
# control, a reader that stops makes the listener stop reading, TCP pushes
# back, and this stops growing.
_streams = {}


@app.route('/big')
def big():
    state = {'yielded': 0, 'finished': False, 'closed': False}
    _streams[request.args['id']] = state
    total = int(request.args.get('mb', '500')) * 1024 * 1024
    chunk = b'\0' * 65536

    def generate():
        try:
            while state['yielded'] < total:
                yield chunk
                state['yielded'] += len(chunk)
            state['finished'] = True
        finally:
            # Closed early: the far end went away and the server noticed.
            if not state['finished']:
                state['closed'] = True
    return Response(generate(), mimetype='application/octet-stream')


@app.route('/stats')
def stats():
    return json.dumps(_streams.get(request.args['id'], {}))


@app.route('/api/digest', methods=['POST'])
def digest():
    """Length and SHA-256 of the request body, read as it arrives."""
    h = hashlib.sha256()
    n = 0
    while True:
        block = request.stream.read(65536)
        if not block:
            break
        h.update(block)
        n += len(block)
    return json.dumps({'length': n, 'sha256': h.hexdigest()})


@app.route('/redirect-test')
def redirect_test():
    return '', 302, {'Location': '/'}

@sock.route('/ws/echo')
def ws_echo(ws):
    """WebSocket echo endpoint."""
    while True:
        data = ws.receive()
        if data is None:
            break
        ws.send(f'echo: {data}')

if __name__ == '__main__':
    app.run(host='127.0.0.1', port=18080)

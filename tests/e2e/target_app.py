"""Local Flask app used as a proxy target for E2E tests.
Runs on localhost:18080. BitBangProxy proxies requests to this."""

from flask import Flask, request, make_response, Response
from flask_sock import Sock
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

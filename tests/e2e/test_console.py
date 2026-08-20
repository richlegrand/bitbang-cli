"""The listener's interactive console.

Everything here needs a real terminal: the console opens /dev/tty and
does not exist without one. The two bugs it has already had were both in
that plumbing -- a second reader on the terminal, and a handler running
on the reader's own goroutine -- and neither is reachable from a Go test.
"""

import os
import time

import pytest


def test_enter_opens_the_console_and_exit_resumes(pty_listener, test_server):
    l = pty_listener('serve', '-server', test_server)
    l.open_console()
    l.command('help', 'leave the console')
    l.command('exit', 'resuming output')


def test_commands_that_only_read(pty_listener, test_server):
    l = pty_listener('serve', '-server', test_server)
    l.open_console()
    # url is the bootstrap command: it is how you get the URL back once
    # the banner has scrolled away, or later off a daemon's log.
    l.command('url', 'https://' + test_server + '/')
    l.command('status', 'nobody connected')
    l.command('list', 'Only this device.s own code')


def test_unknown_command_says_so(pty_listener, test_server):
    l = pty_listener('serve', '-server', test_server)
    l.open_console()
    l.command('frobnicate', 'unknown command')


# The console deadlocked on its first command once, because the handler
# ran on the goroutine that had to deliver the next line. Any command
# completing proves the loop is being fed; running several proves it
# keeps being fed.
def test_the_loop_keeps_accepting_commands(pty_listener, test_server):
    l = pty_listener('serve', '-server', test_server)
    l.open_console()
    for _ in range(3):
        l.command('status', 'nobody connected')
    l.command('help', 'leave the console')


def test_add_list_edit_rm(pty_listener, test_server):
    l = pty_listener('serve', '-server', test_server)
    l.open_console()

    l.command('add', 'Grant which')
    l.command('1', 'Expires')          # files, the first entry: least powerful first
    l.command('3', 'Label')            # 24 hours
    l.command('contractor', 'contractor -- ')
    entry = [e for e in l.links() if e['label'] == 'contractor']
    assert entry, 'add did not write the entry'
    assert entry[0]['scope'] == ['files']
    assert entry[0]['code'], 'add did not mint a code'
    assert entry[0]['expires'].endswith('Z'), 'expiry should be absolute UTC'

    l.command('list', 'contractor')

    # edit re-asks the same questions seeded with the current values, so
    # pressing Enter through changes nothing but the label.
    l.command('edit contractor', 'Grant which')
    l.command('', 'Expires')
    l.command('', 'Label')
    l.command('renamed', 'renamed -- ')
    labels = [e['label'] for e in l.links()]
    assert 'renamed' in labels and 'contractor' not in labels
    kept = [e for e in l.links() if e['label'] == 'renamed'][0]
    assert kept['scope'] == ['files'], 'Enter-through changed the scope'
    assert kept['code'] == entry[0]['code'], 'a rename should not reissue the code'

    l.command('rm renamed', 'removed "renamed"')
    assert not l.links(), 'rm did not remove the entry'


def test_rm_of_an_unknown_label_is_reported(pty_listener, test_server):
    l = pty_listener('serve', '-server', test_server)
    l.open_console()
    l.command('rm nothing-here', 'no link called')


# `me` is the identity's own code, not a table entry: removing it would
# revoke the operator's own access and it would return on reload anyway.
def test_me_cannot_be_removed(pty_listener, test_server):
    l = pty_listener('serve', '-server', test_server)
    l.open_console()
    l.command('rm me', "own code")


# The console used to close after thirty seconds of silence, which is
# short if you are reading a link table or composing a label.
@pytest.mark.slow
@pytest.mark.timeout(180)  # the suite default is shorter than the sleep
def test_the_console_does_not_time_out(pty_listener, test_server):
    l = pty_listener('serve', '-server', test_server)
    l.open_console()
    time.sleep(40)
    l.command('status', 'nobody connected')


# The console and a pairing prompt both want lines from the same
# terminal. When two goroutines received from one channel, whichever won
# took the line -- and a pairing SAS typed by the operator was run as a
# console command, so the pairing timed out as user_declined.
#
# Drives both ends: the connector shows a SAS, the operator types it into
# the listener, and the grant questions follow.
def test_pairing_prompt_is_not_stolen_by_the_console(pty_listener, test_server, bitbang_bin):
    pexpect = pytest.importorskip('pexpect')

    l = pty_listener('serve', '-server', test_server)

    # Its own HOME: the connector would otherwise share the listener's
    # identity directory and its process lock.
    chome = os.path.join(l.home, 'connector')
    os.makedirs(chome, exist_ok=True)
    con = pexpect.spawn(
        bitbang_bin, ['connect', l.pairing_code, '-server', test_server, '-name', 'dev1'],
        env=dict(os.environ, HOME=chome, TERM='dumb'),
        encoding='utf-8', timeout=90, dimensions=(50, 110),
    )
    try:
        con.expect(r'Your pairing code: (\d{6})', timeout=60)
        sas = con.match.group(1)

        l.child.expect(r'Enter code \(attempt 1/3\)', timeout=30)
        l.command(sas, 'Grant everything, no expiry')

        # Decline the default and take the narrow path, so the grant
        # questions are exercised too.
        l.command('n', 'Grant which')
        l.command('1', 'Expires')
        l.command('3', 'Label')
        l.command('phone', 'phone -- ')

        con.expect('Paired', timeout=60)
        con.expect(r'URL: \S+#(\S+)', timeout=10)
        handed = con.match.group(1).strip()

        entry = [e for e in l.links() if e['label'] == 'phone']
        assert entry, 'pairing did not write a link'
        assert entry[0]['scope'] == ['files']
        # The whole point: what the connector is handed is the minted
        # link, not the identity's own code, so it can be revoked and
        # expired on its own.
        assert handed == entry[0]['code'], 'connector did not get the minted code'
        me = l.command('url', r'https://\S+#(\S+)').group(1).strip()
        assert handed != me, 'pairing handed out the device.s own code'
    finally:
        con.terminate(force=True)

"""The listener's interactive console.

Everything here needs a real terminal: the console opens /dev/tty and
does not exist without one. The two bugs it has already had were both in
that plumbing -- a second reader on the terminal, and a handler running
on the reader's own goroutine -- and neither is reachable from a Go test.
"""

import os
import re
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


# `owner` is the identity's own code, not a table entry: removing it
# would revoke the operator's own access and it would return on reload
# anyway.
def test_owner_cannot_be_removed(pty_listener, test_server):
    l = pty_listener('serve', '-server', test_server)
    l.open_console()
    l.command('rm owner', "own code")


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
        own = l.command('url', r'https://\S+#(\S+)').group(1).strip()
        assert handed != own, 'pairing handed out the device.s own code'
    finally:
        con.terminate(force=True)


# The listener most people run has no links at all, and that case still
# has to show the URL -- otherwise the change made for "reprint the URL
# that scrolled away" reprints everything except the URL.
def test_enter_shows_the_url_with_no_links(pty_listener, test_server):
    l = pty_listener('serve', '-server', test_server)
    l.child.sendline('')
    l.child.expect(re.escape(l.device_url), timeout=20)
    l.child.expect('console --', timeout=20)


# Enter reprints the table before prompting. Scrolled-away URLs are the
# usual reason to come back to a listener, so the console opens with the
# thing you came for rather than a hint to type `help`.
def test_enter_reprints_the_table(pty_listener, test_server):
    l = pty_listener('serve', '-server', test_server,
                     links=[{'label': 'contractor', 'scope': ['files']}])
    l.child.sendline('')
    l.child.expect('contractor', timeout=20)   # the table, before the prompt
    l.child.expect('console --', timeout=20)


# The listing numbers every entry so a link can be named by position
# rather than by typing a label out. Labels stay canonical; numbers are
# a convenience resolved at the edge.
def test_links_can_be_addressed_by_number(pty_listener, test_server):
    l = pty_listener('serve', '-server', test_server, links=[
        {'label': 'contractor', 'scope': ['files']},
        {'label': 'ana', 'scope': ['files']},
    ])
    l.open_console()
    l.command('list', r'1\) contractor')
    l.command('qr 1', 'https://')
    l.command('rm 2', 'removed "ana"')
    assert [e['label'] for e in l.links()] == ['contractor']


# 0 is the owner row, refused exactly as the label is -- removing it
# would revoke the operator's own access.
def test_number_zero_is_the_owner_and_is_refused(pty_listener, test_server):
    l = pty_listener('serve', '-server', test_server,
                     links=[{'label': 'contractor', 'scope': ['files']}])
    l.open_console()
    l.command('rm 0', 'own code')
    l.command('rm 7', 'no link called')


# An entry someone literally named "2" has to stay reachable by name,
# so a label match wins over the index it collides with.
def test_a_label_that_looks_like_a_number_wins(pty_listener, test_server):
    l = pty_listener('serve', '-server', test_server, links=[
        {'label': 'contractor', 'scope': ['files']},
        {'label': 'ana', 'scope': ['files']},
        {'label': '2', 'scope': ['files']},
    ])
    l.open_console()
    l.command('rm 2', 'removed "2"')
    # 'ana' sat at index 2 and must be untouched.
    assert sorted(e['label'] for e in l.links()) == ['ana', 'contractor']

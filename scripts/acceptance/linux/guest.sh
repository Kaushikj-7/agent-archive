#!/bin/bash
# The acceptance checks, run as root inside the disposable machine by host.sh
# (docker exec), never anywhere else: it creates files in /home, rewrites
# /etc/machine-id and bind-mounts over it. Each check prints PASS or FAIL; the
# exit status is the number of failures (capped at 125).
set -u

# Refuse to run outside the machine host.sh made: host.sh tells it the run's
# name prefix, only the image built from the Dockerfile beside this script has
# the marker file, and host.sh stages the binary in /acceptance. A Mac or a
# developer's own Linux login, even as root, has none of the three.
case ${AA_ACCEPT_GUEST:-} in
  aa-accept-*) ;;
  *) echo "guest.sh runs only inside the disposable machine host.sh makes; run scripts/acceptance/linux/host.sh" >&2; exit 2 ;;
esac
{ [ "$(uname -s)" = Linux ] && [ "$(id -u)" = 0 ] && [ -f /etc/agent-archive-acceptance-machine ] && [ -x /acceptance/agent-archive ]; } ||
  { echo "not the disposable acceptance machine" >&2; exit 2; }
: "${ENDPOINT:?}" "${ACCESS_KEY:?}" "${SECRET_KEY:?}" "${BUCKET:?}"
pass=0
fail=0
ok() { echo "PASS: $*"; pass=$((pass + 1)); }
bad() { echo "FAIL: $*"; fail=$((fail + 1)); }
check() { # DESCRIPTION COMMAND...
  local what=$1
  shift
  if "$@" >/dev/null 2>&1; then ok "$what"; else bad "$what"; fi
}
check_out() { # DESCRIPTION PATTERN COMMAND...: the command's output matches
  local what=$1 pattern=$2 out
  shift 2
  out=$("$@" 2>&1)
  # shellcheck disable=SC2001  # a prefix on every line is a sed job
  if grep -Eq -- "$pattern" <<<"$out"; then ok "$what"; else bad "$what (wanted /$pattern/ in:"; echo "$out" | sed 's/^/      /'; echo "    )"; fi
}
check_not_out() { # DESCRIPTION PATTERN COMMAND...: the output does not match
  local what=$1 pattern=$2 out
  shift 2
  out=$("$@" 2>&1)
  # shellcheck disable=SC2001  # a prefix on every line is a sed job
  if grep -Eq -- "$pattern" <<<"$out"; then bad "$what (found /$pattern/ in:"; echo "$out" | sed 's/^/      /'; echo "    )"; else ok "$what"; fi
}
# shellcheck disable=SC2329  # called through check_out and check_not_out
views() { # COMMAND... (ending in status): its default view, then --issues, which carries every warning
  "$@"
  "$@" --issues
}
wait_for() { # DESCRIPTION SECONDS COMMAND...
  local what=$1 limit=$2 i
  shift 2
  for ((i = 0; i < limit; i += 3)); do
    if "$@" >/dev/null 2>&1; then ok "$what (after ~${i}s)"; return 0; fi
    # Progress, so a wait never looks like a hang.
    if [ $((i % 15)) -eq 12 ]; then echo "   ... waiting (${i}s of ${limit}s): $what"; fi
    sleep 3
  done
  bad "$what (not within ${limit}s)"
  return 1
}

# ada, as an ssh login of a lingering user sees the machine: ~/.local/bin on
# PATH (Ubuntu's ~/.profile adds it when the directory exists), and the user
# manager's runtime directory and bus (pam_systemd sets both at login).
ADA_ENV=(XDG_RUNTIME_DIR=/run/user/1100 DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1100/bus
  PATH=/home/ada/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin)
as_ada() { sudo -u ada -H env "${ADA_ENV[@]}" "$@"; }
aa() { as_ada agent-archive "$@"; }
# A shell of ada's that has XDG directories of its own, as setup's is told.
XDG_CONFIG=/home/ada/xdg/config
XDG_CACHE=/home/ada/xdg/cache
aa_xdg() { as_ada env XDG_CONFIG_HOME=$XDG_CONFIG XDG_CACHE_HOME=$XDG_CACHE agent-archive "$@"; }
DATA=/home/ada/.local/share/agent-archive
UNITS=/home/ada/.config/systemd/user
REF=agent-archive-collector
SESSION=7d1c2f3a-4b5e-4c6d-8e7f-0a1b2c3d4e5f
PROJECT=/home/ada/projects/widget

echo "== 0. the machine"
systemctl --version | head -1
echo "systemd is $(systemctl is-system-running); ada: $(loginctl show-user ada -p Linger -p State | tr '\n' ' ')"
wait_for "ada's user manager is up" 60 test -S /run/user/1100/bus
check "systemd is at least 240 (the adapter's floor)" test "$(systemctl --version | awk 'NR==1{print $2}')" -ge 240
check_out "ada's user manager answers" 'running|degraded' as_ada systemctl --user is-system-running
if [ -s /etc/machine-id ]; then ok "the machine has a machine ID (/etc/machine-id)"; else bad "no /etc/machine-id"; fi

echo "== 1. a project, an AWS profile for the local MinIO, and the binary"
install -d -o ada -g ada -m 0700 /home/ada/.aws
cat >/home/ada/.aws/config <<EOF
[profile minio]
region = us-east-1
endpoint_url = $ENDPOINT
s3 =
    addressing_style = path
EOF
cat >/home/ada/.aws/credentials <<EOF
[minio]
aws_access_key_id = $ACCESS_KEY
aws_secret_access_key = $SECRET_KEY
EOF
chown ada:ada /home/ada/.aws/config /home/ada/.aws/credentials
chmod 600 /home/ada/.aws/config /home/ada/.aws/credentials
as_ada bash -c '
  git config --global user.email ada@example.test && git config --global user.name Ada
  mkdir -p ~/projects/widget && cd ~/projects/widget && git init -q . && echo hello > README.md && git add . && git commit -qm init
'
# A Cursor database with one synthetic chat in the project, where Cursor keeps
# it under ada's XDG config directory (VS Code's layout, unverified against a
# real Cursor on Linux), with its workspace folder. A writer that stays
# connected leaves the -wal and -shm files that mean "Cursor is running", which
# is when agent-archive reads a chat from a copy of the database (a snapshot)
# and not in place.
CURSOR_DB=$XDG_CONFIG/Cursor/User/globalStorage/state.vscdb
CHAT=c0ffee00-0000-4000-8000-000000000001
as_ada mkdir -p "$(dirname "$CURSOR_DB")" $XDG_CONFIG/Cursor/User/workspaceStorage/ws1 /home/ada/.cursor
# sudo's umask may allow group writes; the synthetic cache home must be private.
install -d -o ada -g ada -m 0700 "$XDG_CACHE"
echo "   guest umask: $(umask); cache home: $(stat -c '%a %U:%G' "$XDG_CACHE")"
# Ten days old: setup imports only the last 7 days, so the chat is left for
# backfill (section 4) to import.
CREATED_MS=$(($(date +%s) * 1000 - 10 * 86400000))
cat >/tmp/cursor-chat.sql <<EOF
PRAGMA journal_mode=WAL;
CREATE TABLE ItemTable (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB);
CREATE TABLE cursorDiskKV (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB);
INSERT INTO cursorDiskKV VALUES ('composerData:$CHAT', '{"_v":18,"composerId":"$CHAT","createdAt":$CREATED_MS,"lastUpdatedAt":$((CREATED_MS + 2000)),"status":"completed","fullConversationHeadersOnly":[{"bubbleId":"$CHAT-m0","type":1},{"bubbleId":"$CHAT-m1","type":2}],"workspaceIdentifier":{"id":"ws1","uri":{"scheme":"file","fsPath":"$PROJECT","path":"$PROJECT"}}}');
INSERT INTO cursorDiskKV VALUES ('bubbleId:$CHAT:$CHAT-m0', '{"_v":3,"bubbleId":"$CHAT-m0","type":1,"text":"synthetic cursor question","createdAt":$CREATED_MS}');
INSERT INTO cursorDiskKV VALUES ('bubbleId:$CHAT:$CHAT-m1', '{"_v":3,"bubbleId":"$CHAT-m1","type":2,"text":"synthetic cursor answer","createdAt":$((CREATED_MS + 1000))}');
EOF
chmod 644 /tmp/cursor-chat.sql
as_ada bash -c "( cat /tmp/cursor-chat.sql; sleep 3000 ) | nohup sqlite3 '$CURSOR_DB' >/dev/null 2>&1 &"
wait_for "Cursor's database is written and open by a writer (-wal and -shm exist)" 30 \
  bash -c "test -e '$CURSOR_DB-wal' -a -e '$CURSOR_DB-shm' && [ \"\$(sudo -u ada sqlite3 -readonly '$CURSOR_DB' 'select count(*) from cursorDiskKV')\" = 3 ]"
echo "{\"folder\":\"file://$PROJECT\"}" | as_ada tee $XDG_CONFIG/Cursor/User/workspaceStorage/ws1/workspace.json >/dev/null
as_ada mkdir -p /home/ada/.local/bin
install -o ada -g ada -m 0755 /acceptance/agent-archive /home/ada/.local/bin/agent-archive
check_out "the binary runs" '^dev|^v?[0-9]' aa --version

echo "== 2. setup --yes (S3 to MinIO), from a shell with XDG_CONFIG_HOME and XDG_CACHE_HOME set"
check "no cache directory exists before the first copy" test ! -e $XDG_CACHE/agent-archive
aa_xdg setup --yes --provider s3 --bucket "$BUCKET" --aws-profile minio --region us-east-1 \
  --project $PROJECT --apps claude,cursor 2>&1 | tail -25
check "setup wrote the service unit" test -f $UNITS/$REF.service
check "setup wrote the timer unit" test -f $UNITS/$REF.timer
check "the unit files are private (0600)" test "$(stat -c %a $UNITS/$REF.service $UNITS/$REF.timer | sort -u)" = 600
check "enabling made the link in timers.target.wants" test -L $UNITS/timers.target.wants/$REF.timer
check_out "the unit records XDG_CONFIG_HOME" "Environment=XDG_CONFIG_HOME=$XDG_CONFIG" cat $UNITS/$REF.service
check_out "the unit records XDG_CACHE_HOME" "Environment=XDG_CACHE_HOME=$XDG_CACHE" cat $UNITS/$REF.service
check_out "the unit's PATH is the shell's entries then systemd's default" 'Environment=PATH=/home/ada/\.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin$' cat $UNITS/$REF.service
check_out "the unit's logs append to the data directory" "StandardOutput=append:$DATA/collector.log" cat $UNITS/$REF.service
check_out "the timer is enabled and waiting" 'active \(waiting\)' as_ada systemctl --user status $REF.timer --no-pager
check_out "config.json records the systemd backend" '"background_backend": ?"systemd"' cat $DATA/config.json
check_out "config.json records this machine's host ID" '"host_id": ?"[0-9a-f]{64}"' cat $DATA/config.json
check_not_out "the host ID is not the machine ID itself" "$(tr -d '\n' </etc/machine-id)" cat $DATA/config.json
check_out "the manager's service environment has the recorded PATH" '/home/ada/\.local/bin' as_ada systemctl --user show $REF.service -p Environment
check_out "status says the job is loaded" '"background": "loaded"' aa status --json
check_not_out "status has no warning about XDG or another machine from the setup shell" 'XDG_|different machine' views aa_xdg status
check_not_out "status sees no network filesystem (the home is on the machine's own disk)" 'network filesystem' views aa status
check_not_out "config.json records no network-home opt-in" 'allow_network_home' cat $DATA/config.json

echo "== 3. a synthetic Claude Code session through the installed hook command"
CLAUDE_HOOK=$(grep -o "'/home/ada/.local/bin/agent-archive' _hook --harness claude" /home/ada/.claude/settings.json | head -1)
check "the hook command setup installed is in Claude Code's settings" test -n "$CLAUDE_HOOK"
as_ada bash -s "$SESSION" "$PROJECT" <<'EOF'
set -eu
ID=$1
PROJECT=$2
DIR=$HOME/.claude/projects/-home-ada-projects-widget
mkdir -p "$DIR"
T=$DIR/$ID.jsonl
sed -e "s#native-claude#$ID#g" -e "s#/work/widget#$PROJECT#g" /acceptance/fixture-claude.jsonl >"$T"
HOOK="$HOME/.local/bin/agent-archive _hook --harness claude"
send() {
  printf '{"hook_event_name":"%s","session_id":"%s","cwd":"%s","transcript_path":"%s"%s}' "$1" "$ID" "$PROJECT" "$T" "${2:-}" | $HOOK
  echo "hook $1 exit=$?"
}
send SessionStart ',"source":"startup"'
send UserPromptSubmit ',"prompt":"file the bugs and run the tests"'
send Stop ',"stop_hook_active":false'
send SessionEnd ',"reason":"other"'
EOF
HOOKED=$(date +%s)
check_out "status counts the session the hooks registered" 'Claude Code.* hooks installed +1 session' aa_xdg status --verbose
echo "   (not running sync: the timer's collector must publish it)"
wait_for "the timer-started collector published the session (list shows it)" 240 bash -c 'sudo -u ada -H env '"${ADA_ENV[*]}"' agent-archive list | grep -q "file the bugs"'
# One project's sessions name it in a PROJECT column or, with several, in the
# heading ("2 sessions · widget").
check_out "list shows the session" '^[^ ]+ +claude +(widget +)?[0-9a-f]{8} +file the bugs and run the tests$' aa list
check_out "list names the session's project" '^[^ ]+ +claude +widget +[0-9a-f]{8} |^[0-9]+ sessions? · widget$' aa list
ID8=$(aa list 2>/dev/null | awk '/file the bugs/ { for (i = 1; i <= NF; i++) if ($i ~ /^[0-9a-f]{8}$/) { print $i; exit } }')
check_out "show reads the session back" 'Claude Code 2\.1\.275' aa show "$ID8"
check_out "show's transcript has the synthetic prompt" 'file the bugs and run the tests' aa show "$ID8" --harness claude --transcript
check_out "the journal shows the service ran after the hooks, started by the timer" 'Finished agent-archive-collector' as_ada journalctl --user -u $REF.service --no-pager --since "@$HOOKED"

echo "== 4. Cursor: a backfill from the shell reads the chat from a copy under XDG_CACHE_HOME"
out=$(aa_xdg backfill --harness cursor --project $PROJECT --yes 2>&1)
echo "$out" | tail -8
check_out "backfill imported the Cursor chat (1 session)" '^Registered 1 session as import ' echo "$out"
check "the cache directory has a CACHEDIR.TAG" test -f $XDG_CACHE/agent-archive/CACHEDIR.TAG
check_out "the tag starts with the specification's signature" '^Signature: 8a477f597d28d172789f06886806bc55' cat $XDG_CACHE/agent-archive/CACHEDIR.TAG
check "the snapshot root is 0700 and ada's" test "$(stat -c '%a %U' $XDG_CACHE/agent-archive/cursor-snapshots)" = "700 ada"
check "the cache directory is ada's and not writable by others" test "$(stat -c '%U %a' $XDG_CACHE/agent-archive)" = "ada 700"
check "no copy of Cursor's chats is left behind" test -z "$(ls -A $XDG_CACHE/agent-archive/cursor-snapshots)"
check "nothing was made under /tmp" test -z "$(ls -d /tmp/agent-archive-cursor-* 2>/dev/null)"
check "nothing was made in the default cache directory" test ! -e /home/ada/.cache/agent-archive
# backfill uploads what it imports, so this is normally there at once; the wait
# covers an upload left for the collector.
wait_for "the imported Cursor chat is in the bucket (list shows it)" 240 bash -c 'sudo -u ada -H env '"${ADA_ENV[*]}"' agent-archive list --harness cursor | grep -q "synthetic cursor question"'
check_out "status shows the imported Cursor chat, uploaded, collector on" 'Cursor +hooks installed +no sessions yet .* 1 imported' aa_xdg status --verbose
pkill -u ada sqlite3 || true

echo "== 5. the shell's XDG directories drifting from the collector's"
check_out "status warns when the shell's XDG_CACHE_HOME differs" "This shell's XDG_CACHE_HOME is ~/elsewhere and the background collector's is ~/xdg/cache" \
  as_ada env XDG_CONFIG_HOME=$XDG_CONFIG XDG_CACHE_HOME=/home/ada/elsewhere agent-archive status --issues
check_not_out "status says nothing about XDG_CONFIG_HOME when only the cache differs" "XDG_CONFIG_HOME" \
  views as_ada env XDG_CONFIG_HOME=$XDG_CONFIG XDG_CACHE_HOME=/home/ada/elsewhere agent-archive status
check_out "status warns when the shell has neither" "This shell's XDG_CONFIG_HOME is not set .* and the background collector's is ~/xdg/config" aa status --issues
check_not_out "status is quiet when the shell matches" 'XDG_' views aa_xdg status

echo "== 6. a copy of the data directory on another machine (its machine ID differs)"
OLD_ID=$(cat /etc/machine-id)
HOST_ID_RECORDED=$(grep -Eo '"host_id": ?"[0-9a-f]{64}"' $DATA/config.json)
check "config.json's host ID was read for the checks below" test -n "$HOST_ID_RECORDED"
check_not_out "status is quiet on the machine that was set up" 'different machine' views aa_xdg status
echo "0123456789abcdef0123456789abcdef" >/etc/machine-id
check_out "status says the data directory was set up on a different machine" 'set up on a different machine' aa_xdg status --issues
check_out "status names the fix" 'uninstall --delete-local-data' aa_xdg status --issues
check_out "status --json carries the warning too" 'different machine' aa status --json
check_out "setup run on the copy warns before it changes anything" 'set up on a different machine' \
  aa_xdg setup --yes --provider s3 --bucket "$BUCKET" --aws-profile minio --region us-east-1 --project $PROJECT --apps claude,cursor
check "setup run on the copy kept the recorded host ID (the warning cannot be silenced by running setup again)" \
  grep -q -- "$HOST_ID_RECORDED" $DATA/config.json
check_out "the copy's warning is still there after that setup" 'set up on a different machine' aa_xdg status --issues
# A container (or a read-only root) that bind-mounts a machine ID over
# /etc/machine-id has the mounting system's ID, not its own: the adapter reads
# no ID there, so there is nothing to compare and no warning.
echo "$OLD_ID" >/etc/machine-id
echo "fedcba9876543210fedcba9876543210" >/run/aa-bound-machine-id
mount --bind /run/aa-bound-machine-id /etc/machine-id
check_not_out "a bind-mounted machine ID (another ID) is not read, so status stays quiet" 'different machine' views aa_xdg status
umount /etc/machine-id
rm -f /run/aa-bound-machine-id
if grep -q ' /etc/machine-id ' /proc/self/mountinfo; then bad "the bind mount over /etc/machine-id is gone"; else ok "the bind mount over /etc/machine-id is gone"; fi
check_not_out "restoring the machine ID silences it" 'different machine' views aa_xdg status

echo "== 7. setup --refresh after the binary moved"
as_ada bash -c 'mkdir -p ~/bin && cp ~/.local/bin/agent-archive ~/bin/agent-archive'
as_ada env XDG_CONFIG_HOME=$XDG_CONFIG XDG_CACHE_HOME=$XDG_CACHE /home/ada/bin/agent-archive setup --refresh 2>&1 | tail -6
check_out "the unit runs the binary refresh was run from" 'ExecStart=/home/ada/bin/agent-archive _collect' cat $UNITS/$REF.service
check_out "refresh kept the recorded XDG directories" "Environment=XDG_CACHE_HOME=$XDG_CACHE" cat $UNITS/$REF.service
check_out "the job is still loaded (or mid-run) after refresh" '"background": "(loaded|running)"' aa status --json
check_out "the hooks run the new binary" "'/home/ada/bin/agent-archive' _hook" cat /home/ada/.claude/settings.json
runs() { as_ada journalctl --user -u $REF.service --no-pager -o short-unix 2>/dev/null | grep -c 'Finished'; }
BEFORE=$(runs)
wait_for "the timer keeps running the collector after refresh" 150 bash -c "[ \"\$(sudo -u ada -H env ${ADA_ENV[*]} journalctl --user -u $REF.service --no-pager -o short-unix | grep -c Finished)\" -gt $BEFORE ]"

echo "== 8. uninstall"
out=$(aa uninstall --yes 2>&1)
for i in 1 2 3 4 5; do grep -q 'holds the collector lock' <<<"$out" || break; sleep 5; out=$(aa uninstall --yes 2>&1); done
echo "$out" | tail -4
check_out "uninstall completes" 'Uninstall complete' echo "$out"
check "the unit files are gone" test ! -e $UNITS/$REF.service -a ! -e $UNITS/$REF.timer
check "the enable link is gone" test ! -L $UNITS/timers.target.wants/$REF.timer
check_not_out "the manager no longer lists the job" "$REF" as_ada systemctl --user list-units --all --no-legend "$REF.*"
check_not_out "the hooks are out of Claude Code's settings" 'agent-archive' cat /home/ada/.claude/settings.json
check_out "status says the background job is missing" '"background": "missing"' aa status --json

echo "== 9. setup whose start fails after systemctl enable made the link (the rollback must leave nothing)"
# A systemctl in ada's ~/.local/bin, ahead of /usr/bin on her PATH, that is the
# real one except that `enable --now X.timer` enables without starting and then
# fails: the manager holds a link and no running timer, which is what a start
# that fails halfway leaves.
as_ada tee /home/ada/.local/bin/systemctl >/dev/null <<'EOF'
#!/bin/sh
case " $* " in
  *" enable --now "*)
    for a; do shift; [ "$a" = --now ] || set -- "$@" "$a"; done
    /usr/bin/systemctl "$@" || exit $?
    echo "injected by the acceptance run: systemctl failed after enabling" >&2
    exit 1 ;;
esac
exec /usr/bin/systemctl "$@"
EOF
as_ada chmod 755 /home/ada/.local/bin/systemctl
out=$(aa_xdg setup --yes --provider s3 --bucket "$BUCKET" --aws-profile minio --region us-east-1 --project $PROJECT --apps claude 2>&1)
status=$?
if [ $status -ne 0 ] && grep -q 'injected by the acceptance run' <<<"$out"; then ok "setup fails when the start fails (exit $status), saying why"; else bad "setup with a failing start (exit $status): $out"; fi
echo "$out" | tail -8 | sed 's/^/   /'
check "the rollback removed the unit files" test ! -e $UNITS/$REF.service -a ! -e $UNITS/$REF.timer
check "the rollback left no enable link (not even a dangling one)" test ! -e $UNITS/timers.target.wants/$REF.timer -a ! -L $UNITS/timers.target.wants/$REF.timer
check_not_out "no timer is running after the rollback" '^active' as_ada systemctl --user is-active $REF.timer
check_not_out "the timer is not enabled after the rollback" '^enabled' as_ada systemctl --user is-enabled $REF.timer
as_ada rm -f /home/ada/.local/bin/systemctl
check "the next setup, with the real systemctl, succeeds" \
  aa_xdg setup --yes --provider s3 --bucket "$BUCKET" --aws-profile minio --region us-east-1 --project $PROJECT --apps claude
check "set up again: timer enabled" test -L $UNITS/timers.target.wants/$REF.timer

echo "== 10. uninstall --skip-scheduler from a session that does reach the manager"
out=$(aa uninstall --yes --skip-scheduler 2>&1)
for i in 1 2 3 4 5; do grep -q 'holds the collector lock' <<<"$out" || break; sleep 5; out=$(aa uninstall --yes --skip-scheduler 2>&1); done
if grep -q 'Uninstall complete' <<<"$out" && ! grep -q 'Not verified stopped' <<<"$out"; then ok "with a reachable manager the flag still stops the job (nothing 'not verified')"; else bad "--skip-scheduler with a manager: $out"; fi
check "the unit files are gone" test ! -e $UNITS/$REF.service -a ! -e $UNITS/$REF.timer
check "the enable link is gone" test ! -e $UNITS/timers.target.wants/$REF.timer -a ! -L $UNITS/timers.target.wants/$REF.timer
check_not_out "the timer is not running" '^active' as_ada systemctl --user is-active $REF.timer

echo "== 11. setup again, then uninstall --skip-scheduler from a session with no user bus"
aa_xdg setup --yes --provider s3 --bucket "$BUCKET" --aws-profile minio --region us-east-1 --project $PROJECT --apps claude 2>&1 | tail -3
check "set up again: timer enabled" test -L $UNITS/timers.target.wants/$REF.timer
wait_for "no collector pass is running (one holds the lock uninstall waits for)" 120 bash -c "! pgrep -u ada -f \"[a]gent-archive _collect\""
# sudo without XDG_RUNTIME_DIR or the bus address, as an ssh session that has no
# pam_systemd (or su from root) is: systemctl --user cannot reach the manager.
NOBUS=(sudo -u ada -H env -u XDG_RUNTIME_DIR -u DBUS_SESSION_BUS_ADDRESS PATH=/home/ada/.local/bin:/usr/bin:/bin)
# The timer's own collector pass may start just before uninstall looks (it holds
# a lock uninstall refuses to run under), so a refusal for that is retried.
nobus_uninstall() {
  local o i
  o=$("${NOBUS[@]}" agent-archive uninstall --yes "$@" 2>&1)
  for i in 1 2 3 4 5 6; do
    grep -q 'holds the collector lock' <<<"$o" || break
    sleep 5
    o=$("${NOBUS[@]}" agent-archive uninstall --yes "$@" 2>&1)
  done
  printf '%s\n' "$o"
}
out=$(nobus_uninstall)
if grep -q 'skip-scheduler' <<<"$out" && grep -q 'systemctl --user stop' <<<"$out"; then ok "uninstall refuses with no user bus, and names the flag and the manual stop command"; else bad "uninstall without a bus: $out"; fi
check "the refusal changed nothing (unit still there)" test -f $UNITS/$REF.service
out=$(nobus_uninstall --skip-scheduler)
if grep -q 'Not verified stopped' <<<"$out"; then ok "--skip-scheduler says the job was not verified stopped"; else bad "--skip-scheduler: $out"; fi
check "--skip-scheduler removed the unit files" test ! -e $UNITS/$REF.service -a ! -e $UNITS/$REF.timer
check "--skip-scheduler left no dangling enable link" test ! -L $UNITS/timers.target.wants/$REF.timer
check_out "the manager still runs the timer, as the summary warned" '^active$' as_ada systemctl --user is-active $REF.timer
as_ada systemctl --user stop $REF.timer $REF.service
check_not_out "the printed manual stop command stops it" '^active' as_ada systemctl --user is-active $REF.timer

echo "== 12. the real-manager tests, run as a second lingering user"
wait_for "bob's user manager is up" 60 test -S /run/user/1101/bus
install -o bob -g bob -m 0755 /acceptance/cli.test /home/bob/cli.test
install -d -o bob -g bob /home/bob/systemd
install -o bob -g bob -m 0755 /acceptance/systemd.test /home/bob/systemd/systemd.test
cp -r /acceptance/systemd-testdata /home/bob/systemd/testdata && chown -R bob:bob /home/bob/systemd
BOB_ENV=(XDG_RUNTIME_DIR=/run/user/1101 DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1101/bus AGENT_ARCHIVE_REAL_SYSTEMD=1)
out=$(cd /home/bob && timeout 660 sudo -u bob -H env "${BOB_ENV[@]}" ./cli.test -test.run 'TestRealSystemd' -test.v -test.timeout 10m 2>&1)
status=$?
grep -E '^(--- |PASS|FAIL|ok)' <<<"$out" | sed 's/^/   /'
# passed OUTPUT TEST...: go test -v's OUTPUT says each TEST passed at the top
# level, so a pattern that matched no test, or a gate that skipped one, is not
# taken for a pass (go test exits 0 for both).
passed() {
  local out=$1 t
  shift
  for t; do grep -q "^--- PASS: $t " <<<"$out" || return 1; done
}
if [ $status -eq 0 ] && passed "$out" TestRealSystemdSetupRunsTheTimerAndUninstallStopsIt TestRealSystemdUninstallSkippingTheSchedulerPrintsACommandThatStopsTheJob; then
  ok "the real-manager tests of the commands pass"
else
  bad "the real-manager tests of the commands (exit $status, or one did not run)"
  echo "$out" | tail -20
fi
out=$(cd /home/bob/systemd && timeout 660 sudo -u bob -H env "${BOB_ENV[@]}" ./systemd.test -test.run 'Real' -test.v -test.timeout 10m 2>&1)
status=$?
grep -E '^(--- |PASS|FAIL|ok)' <<<"$out" | sed 's/^/   /'
if [ $status -eq 0 ] && passed "$out" TestRealUserManagerConformance TestRealSystemctlVersionParsesWhateverTheTerminalSays; then
  ok "the adapter's conformance run over the real manager passes"
else
  bad "the adapter's conformance run over the real manager (exit $status, or it did not run)"
  echo "$out" | tail -30
fi

echo "== 13. the Cursor snapshot and platform tests on a real Linux account (the account's home, not \$HOME)"
out=$(cd /home/bob && sudo -u bob -H env HOME=/home/bob /acceptance/cursorstore.test -test.run 'Snapshot|CacheDirectory' -test.v -test.timeout 5m 2>&1)
status=$?
if [ $status -eq 0 ] && passed "$out" TestSnapshotRootIsTheSystemsOwn TestLinuxSnapshotRootIsMadeUnderTheCacheDirectoryWithATag; then
  ok "cursorstore's snapshot tests pass on Linux"
else
  bad "cursorstore's snapshot tests on Linux (exit $status, or the Linux ones did not run)"
  echo "$out" | tail -20
fi
# The one that needs root to give a directory to another account (it skips as
# bob), run as the machine's root.
out=$(cd /tmp && /acceptance/cursorstore.test -test.run '^TestCacheDirectoryAndRootOwnedByAnotherAccountAreRefused$' -test.v -test.timeout 5m 2>&1)
status=$?
if [ $status -eq 0 ] && passed "$out" TestCacheDirectoryAndRootOwnedByAnotherAccountAreRefused; then
  ok "a cache directory or snapshot root another account owns is refused (as root)"
else
  bad "the snapshot root owned by another account (as root; exit $status, or it did not run)"
  echo "$out" | tail -20
fi

echo
echo "acceptance: $pass passed, $fail failed"
if [ "$fail" -gt 125 ]; then exit 125; fi
exit "$fail"

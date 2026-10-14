# Cloud plugin probe

A marker-only Claude Code plugin used on 2026-10-14 to check how a plugin
added to a claude.ai account behaves in Claude Code cloud sessions. Every hook
writes a small JSON file to `/tmp/aa-plugin-probe/` in the cloud VM and uploads
nothing. Results are in
[cloud capture](../../proposals/cloud-capture.md#verified-behavior-account-plugins).

To rebuild the uploadable zip with the bundled Linux binary (claude.ai rejects
a top-level `bin/`, so it goes in `libexec/`):

```sh
stage=$(mktemp -d)
cp -R dev/probes/cloud-plugin-probe/.claude-plugin dev/probes/cloud-plugin-probe/hooks "$stage/"
mkdir "$stage/libexec"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o "$stage/libexec/agent-archive" ./cmd/agent-archive
mkdir -p dev/probes/out
(cd "$stage" && zip -qr -9 "$OLDPWD/dev/probes/out/aa-probe.zip" .claude-plugin hooks libexec)
```

Upload it on claude.ai under Settings → Plugins → Add → Upload plugin, start a
cloud session at claude.ai/code, and list `/tmp/aa-plugin-probe/`. Delete the
plugin afterwards.

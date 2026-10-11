# Clean older archive copies

Use this guide when you need to remove earlier filtered copies or sessions
from a bucket. The shell recipe requires AWS CLI and jq and coordinates
explicitly stopped writers. Review [what a filter upgrade changes](privacy.md#after-a-filter-upgrade)
first. For removing the entire archive, use the [uninstall guide](../getting-started/uninstall.md#delete-the-archive-in-the-bucket).

## Shell cleanup recipe

`show` prints a session's filter version (`filter_version` in `show --json`). To remove the older copies now,
delete from the bucket the source objects no session's metadata points at,
including its preserved revisions. Referenced older-filter revisions must be
replaced through verified privacy publication before unreferenced cleanup can
remove their objects.
Pause every machine that uploads to the bucket first, so no publication is in
flight: a new source is uploaded before the metadata that points at it.
This needs the [AWS CLI](https://aws.amazon.com/cli/) and `jq`, and
credentials that can list, read, and delete under the prefix. Keep **every**
uploading machine paused until the plan has been applied. Every uploading
installation must report a successful pause; this local recipe cannot establish
that remote writers have stopped. Run the following blocks
in the **same bash or zsh shell**; a plan expires after five minutes and can
only be applied once. Keep the printed plan directory for recovery with
`purge_resume` if an attempt fails or expires. External
writers can still race a shell recipe, so these commands cannot provide an
atomic deletion against concurrent writes.

First, list what would be deleted:

<!-- purge-recipe:list (scripts/test_purge_recipe.py runs the three blocks below) -->
```sh
purge_stop_uploads() {
  purge_dir=                  # invalidate every previous plan before attempting stop
  purge_pause_ok=
  case "${1:-pause}" in
    pause)
      if agent-archive pause; then purge_pause_ok=yes; fi ;;
    uninstall)
      # Explicit opt-in only. Never use --skip-scheduler for this check.
      if agent-archive uninstall --yes; then purge_pause_ok=yes; fi ;;
    *) echo "Choose pause or uninstall; no cleanup plan is available." >&2; return 1 ;;
  esac
  [ "$purge_pause_ok" = yes ] || {
    echo "Stopping uploads failed; no cleanup plan is available. Resolve the lock or scheduler error and retry." >&2
    return 1
  }
}
purge_stop_uploads pause       # must succeed on every uploading installation

bucket=my-archive-bucket       # your bucket
prefix=agent-archive/          # your prefix with its trailing slash, or empty
# export AWS_PROFILE=...       # a profile that can list, read, and delete
# For R2: export AWS_ENDPOINT_URL=https://<ACCOUNT_ID>.r2.cloudflarestorage.com

# Identity contains no credentials. Use the same explicit endpoint/profile/region
# in a fresh shell. Do not put credentials in endpoint URLs.
purge_identity() {
  printf '%s\n' "${AWS_ENDPOINT_URL:-}" "${AWS_ENDPOINT_URL_S3:-}" \
    "${AWS_PROFILE:-}" "${AWS_DEFAULT_PROFILE:-}" "${AWS_REGION:-}" "${AWS_DEFAULT_REGION:-}" \
    "${AWS_IGNORE_CONFIGURED_ENDPOINT_URLS:-}" "${AWS_USE_FIPS_ENDPOINT:-}" "${AWS_USE_DUALSTACK_ENDPOINT:-}"
  config_path=${AWS_CONFIG_FILE:-$HOME/.aws/config}
  printf '%s\n' "$config_path"
  if [ -e "$config_path" ]; then
    shasum -a 256 "$config_path" || return 1
  else
    printf '%s\n' absent
  fi
}
purge_seal() {
  (cd "$purge_dir" && shasum -a 256 manifest.json > manifest.sha256) || return 1
  chmod 400 "$purge_dir/manifest.json" "$purge_dir/manifest.sha256" || return 1
}
purge_seal_attempt() {
  (cd "$purge_dir" && shasum -a 256 bucket prefix mode created keys targets metas tombstones > attempt.sha256) || return 1
  n=0
  while IFS= read -r meta; do
    n=$((n + 1))
    (cd "$purge_dir" && shasum -a 256 "meta.$n" >> attempt.sha256) || return 1
  done < "$purge_dir/metas"
}
# Commit the authoritative progress payload and its checksum with one rename.
# Text logs remain useful diagnostics; the capsule preserves crash uncertainty.
purge_progress_write() {
  [ ! -d "$purge_dir/progress.json" ] && [ ! -L "$purge_dir/progress.json" ] || return 1
  jq -n --arg inflight "$1" --rawfile removed "$purge_dir/removed" \
    --rawfile tombstones "$purge_dir/tombstones" \
    '{removed:($removed|split("\n")[:-1]),tombstones:($tombstones|split("\n")[:-1]),inflight:$inflight}' \
    > "$purge_dir/progress.payload" || return 1
  digest=$(shasum -a 256 "$purge_dir/progress.payload") || return 1
  digest=${digest%% *}
  jq -n --arg digest "$digest" --rawfile payload "$purge_dir/progress.payload" \
    '{digest:$digest,payload:$payload}' > "$purge_dir/progress.next" || return 1
  chmod 400 "$purge_dir/progress.next" || return 1
  mv "$purge_dir/progress.next" "$purge_dir/progress.json"
}
purge_progress_check() {
  jq -ejr '.payload | select(type == "string")' "$1/progress.json" > "$purge_dir/progress.check" || return 1
  digest=$(shasum -a 256 "$purge_dir/progress.check") || return 1
  digest=${digest%% *}
  [ "$digest" = "$(jq -er '.digest' "$1/progress.json")" ] || return 1
  jq -e --slurpfile manifest "$purge_dir/manifest.json" '
    . as $state | $manifest[0].targets as $targets |
    (.removed|type)=="array" and (.tombstones|type)=="array" and
    (.inflight|type)=="string" and
    all(.removed[],.tombstones[]; . as $key | $targets|index($key)) and
    (.inflight == "" or (.inflight as $key | $targets|index($key)))
    ' "$purge_dir/progress.check" >/dev/null || return 1
  # A crash can append a successful inflight deletion before the capsule rename.
  jq -r '.removed[]' "$purge_dir/progress.check" > "$purge_dir/progress.expected" || return 1
  if ! cmp -s "$purge_dir/progress.expected" "$1/removed"; then
    jq -er '.inflight | select(length > 0)' "$purge_dir/progress.check" >> "$purge_dir/progress.expected" || return 1
    cmp -s "$purge_dir/progress.expected" "$1/removed" || return 1
  fi
  jq -r '.tombstones[]' "$purge_dir/progress.check" > "$purge_dir/progress.expected" || return 1
  cmp -s "$purge_dir/progress.expected" "$1/tombstones"
}
purge_manifest() {
  jq -n --arg bucket "$bucket" --arg prefix "$prefix" --arg mode "$mode" \
    --arg selector "$selector" --arg identity "$(purge_identity)" \
    --argjson created "$(cat "$purge_dir/created")" \
    --rawfile keys "$purge_dir/keys" --rawfile targets "$purge_dir/targets" \
    --rawfile metas "$purge_dir/metas" --rawfile selected "$purge_dir/selected" --slurpfile snapshots "$purge_dir/snapshots" \
    '{format:1,bucket:$bucket,prefix:$prefix,mode:$mode,selector:$selector,
      identity:$identity,created:$created,keys:($keys|split("\n")[:-1]),
      targets:($targets|split("\n")[:-1]),metas:($metas|split("\n")[:-1]),
      selected_sessions:($selected|split("\n")[:-1]),snapshots:$snapshots}' > "$purge_dir/manifest.json" || return 1
  purge_seal
}
purge_check_manifest() {
  (cd "$1" && shasum -a 256 -c manifest.sha256 >/dev/null) &&
    jq -e 'type == "object" and .format == 1 and
      (.created|type)=="number" and (.keys|type)=="array" and
      (.targets|type)=="array" and (.metas|type)=="array" and
      (.snapshots|type)=="array" and (.metas|length)==(.snapshots|length) and
      (.keys as $k | all(.targets[]; . as $t | $k|index($t)))' \
      "$1/manifest.json" >/dev/null
}
purge_dir=                         # never inherit a previous plan
purge_prepare() {
  mode=$1; selector=${2:-}
  [ "${purge_pause_ok:-}" = yes ] || { echo "Pause must succeed before preparing a plan." >&2; return 1; }
  purge_dir=                       # a failed new attempt cannot expose an old plan
  case "$prefix" in ''|*/) ;; *) echo "Prefix must be empty or end in /." >&2; return 1 ;; esac
  case "$mode" in
    unreferenced|all) ;;
    old) case "$selector" in ''|*[!0-9]*) echo "Invalid filter version." >&2; return 1 ;; esac ;;
    machine) [ -n "$selector" ] || { echo "Missing machine ID." >&2; return 1; } ;;
    *) echo "Invalid purge mode." >&2; return 1 ;;
  esac
  case "${AWS_ENDPOINT_URL:-}${AWS_ENDPOINT_URL_S3:-}" in
    *'@'*|*'?'*|*'#'*) echo "Endpoint must not contain credentials, query, or fragment." >&2; return 1 ;;
  esac
  purge_identity >/dev/null || return 1
  umask 077
  purge_dir=$(mktemp -d "${TMPDIR:-/tmp}/agent-archive-purge.XXXXXXXX") || return 1
  printf '%s\n' "$bucket" > "$purge_dir/bucket" || return 1
  printf '%s\n' "$prefix" > "$purge_dir/prefix" || return 1
  printf '%s\n' "$mode" > "$purge_dir/mode" || return 1
  # Fetch a complete listing before inspecting or deleting any object.
  scope="${prefix}sessions/"; [ "$mode" = all ] && scope=$prefix
  if ! aws s3api list-objects-v2 --bucket "$bucket" --prefix "$scope" \
      --query 'Contents[].Key' --output json </dev/null > "$purge_dir/list.json"; then
    echo "Listing failed; nothing deleted." >&2; return 1
  fi
  if ! jq -r --arg p "$scope" '
      if . == null then empty
      elif type == "array" and all(.[]; type == "string" and startswith($p) and
          (explode | all(.[]; . >= 32))) then .[]
      else error("incomplete listing") end
    ' "$purge_dir/list.json" > "$purge_dir/unsorted"; then
    echo "Listing invalid; nothing deleted." >&2; return 1
  fi
  LC_ALL=C sort -u "$purge_dir/unsorted" > "$purge_dir/keys" || return 1
  : > "$purge_dir/metas" || return 1
  : > "$purge_dir/metadata.keys" || return 1
  while IFS= read -r key; do
    case "$key" in "${prefix}sessions/"*/metadata.json)
      printf '%s\n' "$key" >> "$purge_dir/metadata.keys" || return 1 ;;
    esac
  done < "$purge_dir/keys"
  # Full-prefix mode deliberately removes every scoped key, even damaged metadata.
  # Only selective modes read metadata to establish source ownership.
  [ "$mode" = all ] || cp "$purge_dir/metadata.keys" "$purge_dir/metas" || return 1
  : > "$purge_dir/current" || return 1
  : > "$purge_dir/selected" || return 1
  : > "$purge_dir/snapshots" || return 1
  n=0
  while IFS= read -r meta; do
    n=$((n + 1))
    if ! aws s3 cp "s3://$bucket/$meta" - </dev/null > "$purge_dir/meta.$n"; then
      echo "Cannot read $meta; nothing deleted." >&2; return 1
    fi
    jq -Rs . "$purge_dir/meta.$n" >> "$purge_dir/snapshots" || return 1
    # Validate the complete active/preserved set before classifying any source.
    if ! jq -ser --arg dir "${meta%metadata.json}" --arg p "$prefix" '
        def uuid: type == "string" and test("^[0-9a-fA-F]{8}(-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}$");
        def captured_time:
          if type != "string" then false else
            [try (capture("^(?<year>[0-9]{4})-(?<month>0[1-9]|1[0-2])-(?<day>0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](\\.[0-9]+)?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])$") |
              (.year | tonumber) as $y | (.month | tonumber) as $m |
              (.day | tonumber) as $d |
              ($d <= ([31, (if $y % 4 == 0 and ($y % 100 != 0 or $y % 400 == 0) then 29 else 28 end),
                31,30,31,30,31,31,30,31,30,31][$m - 1]))) catch false] | .[0] // false
          end;
        def reference($base):
          type == "object" and (.key | type) == "string" and
          (.key | startswith($base)) and
          (.key | ltrimstr($base) | test("^source\\.[0-9a-f]{64}\\.jsonl\\.gz$"));
        if length == 1 and (.[0] | type) == "object" then .[0]
        else error("metadata must contain one object") end |
        ($dir | ltrimstr($p)) as $base |
        .filter_version as $v | .machine_id as $m |
        if .schema_version == 1 and .history == null then [.source_bundle]
        elif .schema_version == 2 and ($base | startswith("sessions/codex/")) and
          (.history | type) == "object" and (.history.current_revision | uuid) and
          (.history.preserved == null or (.history.preserved | type) == "array") and
          ((.history.preserved // []) | length <= 64) and
          all((.history.preserved // [])[];
            (.revision_id | uuid) and (.captured_at | captured_time) and
            (.captured_at | startswith("0001-") | not) and
            ((if .source_schema_version == null then 0 else .source_schema_version end) as $sv | $sv == 0 or $sv == 2 or $sv == 3)) and
          ([.history.current_revision, (.history.preserved // [])[].revision_id] |
            length == (unique | length)) then
          [.source_bundle, (.history.preserved // [])[].source] |
          if all(.[]; (.sha256 | type) == "string" and
              (.sha256 | test("^[0-9a-f]{64}$")) and
              (.sha256 as $sha | .key | endswith("source." + $sha + ".jsonl.gz")) and
              (.compressed_bytes | type) == "number" and .compressed_bytes > 0 and
              .compressed_bytes == (.compressed_bytes | floor))
          then . else error("invalid history source reference") end
        else error("unsupported or invalid metadata history") end |
        . as $refs |
        if all($refs[]; reference($base)) and
           ([$refs[].key] | length == (unique | length)) and
           ($v | tostring | test("^[0-9]+$")) and
           ($m | type) == "string" and ($m | length > 0) then
          $refs[] | [.key, ($v | tostring), $m] | @tsv
        else error("invalid metadata") end
      ' "$purge_dir/meta.$n" > "$purge_dir/fields.$n"; then
      echo "Invalid $meta; nothing deleted." >&2; return 1
    fi
    while IFS="$(printf '\t')" read -r source version owner; do
      if ! grep -Fxq -- "$prefix$source" "$purge_dir/keys"; then
        echo "Referenced source missing for $meta; nothing deleted." >&2; return 1
      fi
      printf '%s%s\n' "$prefix" "$source" >> "$purge_dir/current" || return 1
    done < "$purge_dir/fields.$n"
    IFS="$(printf '\t')" read -r source version owner < "$purge_dir/fields.$n" || return 1
    if { [ "$mode" = old ] && [ "$version" -lt "$selector" ]; } ||
       { [ "$mode" = machine ] && [ "$owner" = "$selector" ]; }; then
      printf '%s\n' "$meta" >> "$purge_dir/selected" || return 1
    fi
  done < "$purge_dir/metas"
  LC_ALL=C sort -u "$purge_dir/current" -o "$purge_dir/current" || return 1
  : > "$purge_dir/targets" || return 1
  if [ "$mode" = all ]; then
    cat "$purge_dir/metadata.keys" > "$purge_dir/targets" || return 1
    while IFS= read -r key; do
      if ! grep -Fxq -- "$key" "$purge_dir/metadata.keys"; then
        printf '%s\n' "$key" >> "$purge_dir/targets" || return 1
      fi
    done < "$purge_dir/keys"
  elif [ "$mode" = unreferenced ]; then
    jq -r --arg p "$prefix" '
      if . == null then empty else .[] |
        select((ltrimstr($p) |
          test("^sessions/[^/]+/[^/]+/source\\.[0-9a-f]{64}\\.jsonl\\.gz$"))) end
      ' "$purge_dir/list.json" > "$purge_dir/sources" || return 1
    LC_ALL=C sort -u "$purge_dir/sources" -o "$purge_dir/sources" || return 1
    LC_ALL=C comm -23 "$purge_dir/sources" "$purge_dir/current" > "$purge_dir/targets" || return 1
  else
    while IFS= read -r meta; do
      printf '%s\n' "$meta" >> "$purge_dir/targets" || return 1
      while IFS= read -r key; do
        case "$key" in "${meta%metadata.json}"*)
          [ "$key" = "$meta" ] || printf '%s\n' "$key" >> "$purge_dir/targets" || return 1 ;;
        esac
      done < "$purge_dir/keys"
    done < "$purge_dir/selected"
  fi
  date +%s > "$purge_dir/created" || return 1
  : > "$purge_dir/tombstones" || return 1
  purge_manifest || return 1
  purge_seal_attempt || return 1
  : > "$purge_dir/VALID" || return 1 # written only after every check succeeds
  echo "Plan $purge_dir: $(wc -l < "$purge_dir/targets" | tr -d ' ') exact keys."
  cat "$purge_dir/targets"
}
purge_apply() {
  [ "${purge_pause_ok:-}" = yes ] || { echo "Pause must succeed before deleting." >&2; return 1; }
  if [ -z "${purge_dir:-}" ] || [ ! -f "$purge_dir/VALID" ]; then
    echo "No valid plan; nothing deleted." >&2; return 1
  fi
  rm "$purge_dir/VALID" || return 1 # a plan can be attempted only once
  (cd "$purge_dir" && shasum -a 256 -c attempt.sha256 >/dev/null) || return 1
  purge_check_manifest "$purge_dir" || { echo "Invalid manifest; nothing deleted." >&2; return 1; }
  [ "$(jq -r .identity "$purge_dir/manifest.json")" = "$(purge_identity)" ] || {
    echo "Destination identity changed; nothing deleted." >&2; return 1;
  }
  if [ "$(cat "$purge_dir/bucket")" != "$bucket" ] ||
     [ "$(cat "$purge_dir/prefix")" != "$prefix" ] ||
     [ "$(cat "$purge_dir/mode")" != "$mode" ]; then
    echo "Bucket, prefix, or mode changed; nothing deleted." >&2; return 1
  fi
  now=$(date +%s); created=$(cat "$purge_dir/created")
  if [ "$now" -lt "$created" ] || [ $((now - created)) -gt 300 ]; then
    echo "Plan expired; nothing deleted." >&2; return 1
  fi
  scope="${prefix}sessions/"; [ "$mode" = all ] && scope=$prefix
  if ! aws s3api list-objects-v2 --bucket "$bucket" --prefix "$scope" \
      --query 'Contents[].Key' --output json </dev/null > "$purge_dir/recheck.json" ||
     ! jq -r --arg p "$scope" '
       if . == null then empty
       elif type == "array" and all(.[]; type == "string" and startswith($p) and
           (explode | all(.[]; . >= 32))) then .[]
       else error("incomplete listing") end
     ' "$purge_dir/recheck.json" > "$purge_dir/recheck.unsorted" ||
     ! LC_ALL=C sort -u "$purge_dir/recheck.unsorted" > "$purge_dir/recheck.keys" ||
     ! cmp -s "$purge_dir/keys" "$purge_dir/recheck.keys"; then
    echo "Listing changed or failed; nothing deleted. Make a new plan." >&2; return 1
  fi
  n=0
  while IFS= read -r meta; do
    n=$((n + 1))
    if ! aws s3 cp "s3://$bucket/$meta" - </dev/null > "$purge_dir/recheck.meta" ||
       ! cmp -s "$purge_dir/meta.$n" "$purge_dir/recheck.meta"; then
      echo "Metadata changed or failed at $meta; nothing deleted. Make a new plan." >&2
      return 1
    fi
  done < "$purge_dir/metas"
  : > "$purge_dir/removed" || return 1
  : > "$purge_dir/errors" || return 1
  : > "$purge_dir/pending" || return 1
  cp "$purge_dir/targets" "$purge_dir/pending" || return 1
  purge_progress_write "" || return 1
  while IFS= read -r key; do
    # Write ahead: if interrupted after remote deletion, listing resolves uncertainty.
    printf '%s\n' "$key" > "$purge_dir/inflight" || return 1
    purge_progress_write "$key" || return 1
    if ! aws s3 rm "s3://$bucket/$key" </dev/null 2> "$purge_dir/error.next"; then
      cat "$purge_dir/error.next" >> "$purge_dir/errors" || return 1
      cat "$purge_dir/error.next" >&2
      purge_progress_write "" || return 1
      rm "$purge_dir/inflight" || return 1
      echo "Delete failed at $key. Already removed:" >&2
      cat "$purge_dir/removed" >&2
      echo "Not confirmed removed (including failed key):" >&2
      cat "$purge_dir/pending" >&2
      return 1
    fi
    if ! printf '%s\n' "$key" >> "$purge_dir/removed" ||
       ! sed '1d' "$purge_dir/pending" > "$purge_dir/next" ||
       ! mv "$purge_dir/next" "$purge_dir/pending"; then
      echo "Progress log failed after removing $key; stopped. Inspect the bucket." >&2
      return 1
    fi
    purge_progress_write "" || return 1
    rm "$purge_dir/inflight" || return 1
  done < "$purge_dir/targets"
  echo "Deleted $(wc -l < "$purge_dir/removed" | tr -d ' ') keys."
}
purge_resume() {
  original=$1
  [ "${purge_pause_ok:-}" = yes ] || { echo "Pause must succeed before recovery." >&2; return 1; }
  purge_dir=
  purge_check_manifest "$original" || { echo "Unreadable or corrupt manifest; nothing deleted." >&2; return 1; }
  [ "$(jq -r .bucket "$original/manifest.json")" = "$bucket" ] &&
    [ "$(jq -r .prefix "$original/manifest.json")" = "$prefix" ] &&
    [ "$(jq -r .identity "$original/manifest.json")" = "$(purge_identity)" ] || {
      echo "Destination changed; nothing deleted." >&2; return 1;
    }
  case "${AWS_ENDPOINT_URL:-}${AWS_ENDPOINT_URL_S3:-}" in
    *'@'*|*'?'*|*'#'*) echo "Endpoint must not contain credentials, query, or fragment." >&2; return 1 ;;
  esac
  purge_identity >/dev/null || return 1
  umask 077
  purge_dir=$(mktemp -d "${TMPDIR:-/tmp}/agent-archive-purge.XXXXXXXX") || return 1
  cp "$original/manifest.json" "$purge_dir/manifest.json" &&
    cp "$original/manifest.sha256" "$purge_dir/manifest.sha256" || return 1
  mode=$(jq -r .mode "$purge_dir/manifest.json")
  selector=$(jq -r .selector "$purge_dir/manifest.json")
  printf '%s\n' "$bucket" > "$purge_dir/bucket" || return 1
  printf '%s\n' "$prefix" > "$purge_dir/prefix" || return 1
  printf '%s\n' "$mode" > "$purge_dir/mode" || return 1
  scope="${prefix}sessions/"; [ "$mode" = all ] && scope=$prefix
  aws s3api list-objects-v2 --bucket "$bucket" --prefix "$scope" \
    --query 'Contents[].Key' --output json </dev/null > "$purge_dir/list.json" || return 1
  jq -r --arg p "$scope" '
    if . == null then empty
    elif type == "array" and all(.[]; type == "string" and startswith($p) and
      (explode|all(.[]; . >= 32))) then .[] else error("incomplete listing") end
    ' "$purge_dir/list.json" > "$purge_dir/unsorted" || return 1
  LC_ALL=C sort -u "$purge_dir/unsorted" > "$purge_dir/keys" || return 1
  jq -r '.keys[]' "$purge_dir/manifest.json" > "$purge_dir/original.keys" || return 1
  LC_ALL=C comm -13 "$purge_dir/original.keys" "$purge_dir/keys" > "$purge_dir/new.keys" || return 1
  [ ! -s "$purge_dir/new.keys" ] || { echo "New objects conflict with recovery; nothing deleted." >&2; return 1; }
  : > "$purge_dir/metas" || return 1
  : > "$purge_dir/targets" || return 1
  : > "$purge_dir/tombstones" || return 1
  if [ -e "$original/progress.json" ]; then
    purge_progress_check "$original" || { echo "Corrupt progress; nothing deleted." >&2; return 1; }
    jq -r '.tombstones[],.removed[],(.inflight|select(length>0))' \
      "$purge_dir/progress.check" > "$purge_dir/tombstones" || return 1
  elif [ -e "$original/removed" ] || [ -e "$original/pending" ] || [ -e "$original/inflight" ]; then
    echo "Missing progress capsule; nothing deleted." >&2; return 1
  fi
  jq -r '.metas[]' "$purge_dir/manifest.json" > "$purge_dir/original.metas" || return 1
  n=0; i=0
  while IFS= read -r meta; do
    i=$((i + 1))
    if grep -Fxq -- "$meta" "$purge_dir/keys"; then
      if grep -Fxq -- "$meta" "$purge_dir/tombstones"; then
        echo "Metadata reappeared or has an interrupted delete: $meta; nothing deleted." >&2; return 1
      fi
      n=$((n + 1))
      aws s3 cp "s3://$bucket/$meta" - </dev/null > "$purge_dir/meta.$n" || return 1
      jq -jr --argjson i "$((i - 1))" '.snapshots[$i]' "$purge_dir/manifest.json" > "$purge_dir/expected" || return 1
      cmp -s "$purge_dir/expected" "$purge_dir/meta.$n" || {
        echo "Metadata changed at $meta; nothing deleted." >&2; return 1;
      }
      printf '%s\n' "$meta" >> "$purge_dir/metas" || return 1
    elif ! jq -e --arg m "$meta" '.targets|index($m)' "$purge_dir/manifest.json" >/dev/null; then
      echo "Unselected metadata disappeared; nothing deleted." >&2; return 1
    else
      # An observed absence is lineage history even without a local delete record.
      printf '%s\n' "$meta" >> "$purge_dir/tombstones" || return 1
    fi
  done < "$purge_dir/original.metas"
  # Full-prefix plans do not snapshot metadata bodies, but retain deletion history.
  if [ "$mode" = all ]; then
    while IFS= read -r key; do
      case "$key" in "${prefix}sessions/"*/metadata.json)
        if grep -Fxq -- "$key" "$purge_dir/tombstones" &&
           grep -Fxq -- "$key" "$purge_dir/keys"; then
          echo "Metadata reappeared or has an interrupted delete: $key; nothing deleted." >&2; return 1
        elif ! grep -Fxq -- "$key" "$purge_dir/keys"; then
          printf '%s\n' "$key" >> "$purge_dir/tombstones" || return 1
        fi ;;
      esac
    done < "$purge_dir/original.keys"
  fi
  # Preserve original metadata-first order; never discover new ownership.
  jq -r '.targets[]' "$purge_dir/manifest.json" > "$purge_dir/original.targets" || return 1
  while IFS= read -r key; do
    if grep -Fxq -- "$key" "$purge_dir/keys"; then
      printf '%s\n' "$key" >> "$purge_dir/targets" || return 1
    fi
  done < "$purge_dir/original.targets"
  # Preserve inherited deletion history even if this recovery plan is never applied.
  : > "$purge_dir/removed" || return 1
  purge_progress_write "" || return 1
  date +%s > "$purge_dir/created" || return 1
  purge_seal_attempt || return 1
  : > "$purge_dir/VALID" || return 1
  echo "Recovery plan $purge_dir: review these remaining original keys, then purge_apply within five minutes:"
  cat "$purge_dir/targets"
}
purge_prepare unreferenced
```

Review the printed exact keys. The manifest needs the macOS `shasum` utility as well as AWS CLI and `jq`.
A failed listing or any unreadable or malformed
metadata leaves no valid plan and must be fixed before trying again. Then,
within five minutes and in the same shell, delete:

<!-- purge-recipe:delete -->
```sh
purge_apply
```

To remove sessions whose current copy predates a filter version (here 10)
as well, delete each one whole, metadata first:

Run the preparation block above first in the same shell. The following makes
a new plan. Review every printed key, then run the `purge_apply` block above
within five minutes in that shell.

<!-- purge-recipe:old-sessions -->
```sh
purge_prepare old 10
```

After a failed, interrupted, or expired attempt, keep every writer stopped.
In a fresh bash or zsh shell, load the helper definitions above (omit the final
`purge_prepare unreferenced` line), restore the same bucket, prefix, endpoint,
profile and region (and unchanged AWS configuration). For still-installed
installations, run `purge_stop_uploads pause`; after uninstall, explicitly run
`purge_stop_uploads uninstall`. The latter reruns ordinary uninstall without
prompting; never use `--skip-scheduler` to establish that uploads stopped.
Only after that stop command succeeds, run `purge_resume /absolute/path/to/retained-plan`.
Review its printed remaining original keys and run `purge_apply` again within
five minutes. Use the newest printed recovery directory for each subsequent retry, keeping
its earlier directories until cleanup succeeds. Recovery uses the immutable
manifest and a complete bucket listing, so a remote delete interrupted before local logging does not lose source keys.
Changed or reappearing metadata, new objects, and corrupt or missing attempt progress abort recovery. An uncertain
metadata deletion that still exists also aborts; inspect that conflict first.
The recipes cannot guarantee atomicity against external writers. The private
manifest retains metadata snapshots and object names, never transcript content
or credentials. After successful cleanup, remove the retained plan directories;
deleting them earlier removes resumability.

When you are done, run `agent-archive resume` on every machine you paused.

A session deleted this way is gone from `list`; if its transcript is still
on the capturing machine and changes again, that machine publishes it anew. On an S3
bucket with versioning turned on, a delete only hides the object: remove
the noncurrent versions too, or add a lifecycle rule that expires them.


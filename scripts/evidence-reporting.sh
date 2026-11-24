#!/bin/bash
# Evidence that a failing state adapter keeps the component's last good state with
# `reporting` set (never fail), that `beacon check --adapter` refuses a bad document, and that
# /v3/state is served as schema 3. Builds beacon into a temp dir, runs it against temp
# folders only, and cleans up: nothing outside the temp dir is touched. Exit 0 = all shown.
set -euo pipefail
cd "$(dirname "$0")/.."

work=$(mktemp -d)
serve_pid=
cleanup() {
  if [ -n "$serve_pid" ]; then kill "$serve_pid" 2>/dev/null || true; wait "$serve_pid" 2>/dev/null || true; fi
  rm -rf "$work"
}
trap cleanup EXIT

go build -o "$work/beacon" ./cmd/beacon
export BEACON_DATA="$work/data" BEACON_ETC="$work/etc" BEACON_HOST_FEED=off BEACON_SOURCE=evidence
mkdir -p "$BEACON_ETC/components.d"
beacon() { "$work/beacon" "$@"; }

# A definition whose adapter works while $work/mode says "good" and breaks after.
cat > "$work/adapter" <<EOF
#!/bin/sh
if [ "\$(cat "$work/mode")" = good ]; then
  echo '{"status":"warn","summary":"3 open","gauges":[{"key":"open","value":3,"unit":"count"}],"items":[{"id":"a","title":"Fix the thing","waiting":true}]}'
else
  echo 'database is locked' >&2; exit 1
fi
EOF
chmod +x "$work/adapter"
cat > "$BEACON_ETC/components.d/queue.json" <<EOF
{"id": "queue", "class": "list", "title": "Review queue", "state": "$work/adapter"}
EOF
show() { python3 -I -c '
import json, sys
c = json.load(open(sys.argv[1]))
print("  class=%s status=%s summary=%r items=%d reporting=%s" % (c["class"], c.get("status"), c.get("summary"), len(c.get("items", [])), json.dumps(c.get("reporting"))))
' "$BEACON_DATA/components/queue.json"; }

echo "1. the adapter works"
echo good > "$work/mode"
beacon collect
show
python3 -I -c 'import json,sys; c=json.load(open(sys.argv[1])); assert "reporting" not in c and c["status"]=="warn"' "$BEACON_DATA/components/queue.json"

echo "2. the adapter breaks: the last good state stays, reporting is set, the status is not fail"
echo bad > "$work/mode"
beacon collect
show
python3 -I -c '
import json,sys
c=json.load(open(sys.argv[1])); r=c["reporting"]
assert c["status"]=="warn" and c["summary"]=="3 open" and len(c["items"])==1, c
assert r["last_good_at"] and r["since"] and "database is locked" in r["error"], r
' "$BEACON_DATA/components/queue.json"

echo "3. beacon check --adapter refuses a bad document"
for doc in '{"status":"ok","class":"service"}' '{"status":"ok","reporting":null}' 'not json'; do
  if echo "$doc" | beacon check --adapter - ; then echo "accepted: $doc" >&2; exit 1; else echo "  refused (exit $?): $doc"; fi
done
echo '{"status":"ok","summary":"fine"}' | beacon check --adapter - && echo "  a good document passes"
echo "  and the definitions, run for real: $(beacon check --definitions "$BEACON_ETC/components.d" 2>&1 || true)"

echo "4. /v3/state is served as schema 3"
token=$(beacon token add evidence)
port=$(python3 -I -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')
BEACON_LISTEN="127.0.0.1:$port" "$work/beacon" serve 2>"$work/serve.log" &
serve_pid=$!
for _ in $(seq 50); do curl -fs "http://127.0.0.1:$port/healthz" >/dev/null 2>&1 && break; sleep 0.1; done
curl -fs -H "Authorization: Bearer $token" "http://127.0.0.1:$port/v3/state" > "$work/state.json"
python3 -I -c '
import json,sys
s=json.load(open(sys.argv[1]))
q=[c for c in s["components"] if c["id"]=="queue"][0]
assert s["schema"]==3 and q["class"]=="list" and q["reporting"]["last_good_at"], s
print("  schema=%d components=%s reporting.error=%r" % (s["schema"], [c["id"] for c in s["components"]], q["reporting"]["error"]))
' "$work/state.json"
beacon check --envelope "$work/state.json" && echo "  the answer is a valid envelope"
code=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $token" "http://127.0.0.1:$port/v""2/state")
[ "$code" = 404 ] && echo "  the old schema's path answers 404"

echo "5. the adapter works again: reporting clears"
echo good > "$work/mode"
beacon collect
show
python3 -I -c 'import json,sys; assert "reporting" not in json.load(open(sys.argv[1]))' "$BEACON_DATA/components/queue.json"
echo "evidence: ok (cleaned up)"

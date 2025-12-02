#!/usr/bin/env bash
# Wrapper for govulncheck with grace period for newly published vulnerabilities
set -euo pipefail

GRACE_HOURS="${VULN_GRACE_PERIOD_HOURS:-48}"
OUTPUT=$(mktemp)
trap "rm -f $OUTPUT" EXIT

go tool govulncheck -json ./pkg/... > "$OUTPUT" 2>&1 || true

# Parse and check publish dates
python3 - "$OUTPUT" "$GRACE_HOURS" << 'EOF'
import json, sys, datetime
output_file, grace_hours = sys.argv[1], int(sys.argv[2])
cutoff = datetime.datetime.now(datetime.timezone.utc) - datetime.timedelta(hours=grace_hours)

with open(output_file) as f:
    content = f.read()

# Parse JSON stream
objects, depth, start = [], 0, 0
for i, c in enumerate(content):
    if c == '{':
        if depth == 0: start = i
        depth += 1
    elif c == '}':
        depth -= 1
        if depth == 0:
            try: objects.append(json.loads(content[start:i+1]))
            except: pass

findings = [o for o in objects if o.get("finding")]
if not findings:
    print("No vulnerabilities found.")
    sys.exit(0)

osv_map = {e["osv"]["id"]: e["osv"] for e in objects if e.get("osv")}
seen, blocking = set(), []

for f in findings:
    vuln_id = f["finding"]["osv"]
    if vuln_id in seen: continue
    seen.add(vuln_id)
    published = osv_map.get(vuln_id, {}).get("published", "")
    if published:
        pub_dt = datetime.datetime.fromisoformat(published.replace("Z", "+00:00"))
        if pub_dt > cutoff:
            print(f"⚠️  {vuln_id}: within {grace_hours}h grace period (published {published})")
            continue
    blocking.append(vuln_id)

if blocking:
    print(f"❌ Blocking: {blocking}")
    sys.exit(1)
print("✅ All vulnerabilities within grace period")
EOF

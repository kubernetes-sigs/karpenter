#!/usr/bin/env bash
# Prints the kind-e2e-presubmit GITHUB_OUTPUT lines for the diff BASE..HEAD:
#   run=true|false  false when every changed file is documentation
#   repeat=<json>   [{"name","suite","focus_file"}] for each suite with changed e2e specs
#
# A changed spec is an It or Entry whose body holds a changed line in
# test/suites/{regression,disruption}/*_test.go. They rerun because a racy new spec can pass once.
# Changes to setup nodes, containers, table bodies and helpers aren't repeated.
#
# Usage: hack/kind-e2e-presubmit-changes.sh BASE HEAD
set -euo pipefail

base=$1
head=$2
# Skip a suite's repeat leg above this many changed specs, so a large test refactor doesn't add hours.
max_specs=${MAX_REPEAT_SPECS:-15}

changed=$(git diff --name-only "${base}" "${head}")

run=false
while IFS= read -r f; do
  [ -z "${f}" ] && continue
  case "${f}" in
    *.md | designs/* | OWNERS | */OWNERS | OWNERS_ALIASES | LICENSE | NOTICE | SECURITY_CONTACTS | SECURITY-INSIGHTS.yml) ;;
    *) run=true ;;
  esac
done <<< "${changed}"
echo "run=${run}"

# "path line" per added or changed line. -M keeps a moved file from counting as all new specs.
# A pure deletion prints "path -line" for the line below it.
changed_lines=$(git diff -U0 -M "${base}" "${head}" -- test/suites |
  awk '/^\+\+\+ / { f = substr($2, 3) } /^@@/ { split($3, a, ","); s = substr(a[1], 2); n = (a[2] == "" ? 1 : a[2]); if (n == 0) print f, -(s + 1); for (i = s; i < s + n; i++) print f, i }')

entries=()
for suite in regression disruption; do
  filters=()
  count=0
  # The strict name pattern also keeps the value safe to pass through make to the shell.
  for f in $(awk '{ print $1 }' <<< "${changed_lines}" | sort -u | grep -E "^test/suites/${suite}/[a-z0-9_]+_test\.go$" | grep -v '/suite_test\.go$' || true); do
    specs=$(git show "${head}:${f}" | awk -v changed="$(awk -v f="${f}" '$1 == f { print $2 }' <<< "${changed_lines}" | tr '\n' ' ')" '
      BEGIN { split(changed, c, " "); for (i in c) if (c[i] < 0) del[-c[i]] = 1; else add[c[i]] = 1 }
      /^(var _ = )?[[:space:]]*(BeforeEach|AfterEach|JustBeforeEach|JustAfterEach|BeforeAll|AfterAll|Describe|Context|When|DescribeTable)\(/ || /^func/ { spec = 0 }
      /^[[:space:]]*(It|Entry)\(/ { spec = NR; match($0, /^[[:space:]]*/); closer = "^" substr($0, 1, RLENGTH) "[})]" }
      # A deletion just above a spec belongs to what precedes the spec.
      spec && ((NR in add) || ((NR in del) && NR != spec)) { out[spec] = 1 }
      # The line that closes the spec at its indent is the last one in it.
      spec && NR != spec && $0 ~ closer { spec = 0 }
      END { for (s in out) print s }' | sort -n | paste -sd, -)
    [ -z "${specs}" ] && continue
    count=$((count + $(tr ',' '\n' <<< "${specs}" | wc -l)))
    filters+=("suites/${suite}/$(basename "${f}"):${specs}")
  done
  [ "${#filters[@]}" -eq 0 ] && continue
  if [ "${count}" -gt "${max_specs}" ]; then
    echo "skipping the ${suite} repeat leg: ${count} changed specs is over ${max_specs}" >&2
    continue
  fi
  name="$(tr '[:lower:]' '[:upper:]' <<< "${suite:0:1}")${suite:1}"
  entries+=("{\"name\":\"repeat-${suite}\",\"suite\":\"${name}\",\"focus_file\":\"${filters[*]}\"}")
done
echo "repeat=[$(IFS=,; echo "${entries[*]-}")]"

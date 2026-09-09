#!/usr/bin/env bash
# scan.sh runs every repository quality check and prints one PASS/FAIL line
# per check, with the offending lines indented under a failure. The exit
# status is non-zero when any check fails, so it gates commits and CI.
#
# The trace word lists below are deliberately short and readable: anyone can
# audit what the scanner looks for by reading this file top to bottom.

set -u
cd "$(dirname "$0")/.."

failures=0

# check COMMAND... passes when the command exits zero.
check() {
    local name="$1"
    shift
    local out
    if out=$("$@" 2>&1); then
        echo "PASS  $name"
    else
        echo "FAIL  $name"
        printf '%s\n' "$out" | sed 's/^/      /'
        failures=$((failures + 1))
    fi
}

# check_silent COMMAND... passes when the command prints nothing
# (used for scanners whose findings ARE their output).
check_silent() {
    local name="$1"
    shift
    local out
    out=$("$@" 2>&1)
    if [ -z "$out" ]; then
        echo "PASS  $name"
    else
        echo "FAIL  $name"
        printf '%s\n' "$out" | sed 's/^/      /'
        failures=$((failures + 1))
    fi
}

# Source files the text scans cover. The scanner itself is excluded: it
# legitimately contains the words it looks for.
sources() {
    find . -type f \( -name '*.go' -o -name '*.sql' -o -name '*.toml' \
        -o -name '*.md' -o -name '*.mod' -o -name '*.sh' \) \
        -not -path './.git/*' -not -path './scripts/scan.sh'
}

# 1. Compilation and tests.
check      "go build"         go build ./...
check      "go vet"           go vet ./...
check      "go test"          go test ./...
check_silent "gofmt"          gofmt -l .

# 2. ASCII only: no non-ASCII byte may appear in any source file.
check_silent "ascii-only"     bash -c "$(declare -f sources); sources | xargs grep -nP '[^\x00-\x7F]'"

# 3. Contextual traces: wording tied to one team or workflow must not leak
#    into the repository.
check_silent "contextual-traces" bash -c "$(declare -f sources); sources | xargs grep -niE '\b(operation team|ops team|cloud team|system administrat|sop\b|tpm\b|trusted platform|auditab)'"

# 4. AI traces: boilerplate phrasing and telltale punctuation.
check_silent "ai-traces"      bash -c "$(declare -f sources); sources | xargs grep -niE '\b(delve|leverage|robust|seamless|comprehensive|furthermore|moreover|additionally|in summary|it.s worth noting)\b'"

# 5. Leftover work markers.
check_silent "todo-markers"   bash -c "$(declare -f sources); sources | xargs grep -nE '\b(TODO|FIXME|XXX|HACK)\b'"

# 6. Optional external scanners, used when installed.
for tool in staticcheck govulncheck gitleaks; do
    if command -v "$tool" >/dev/null 2>&1; then
        case "$tool" in
            gitleaks) check "$tool" "$tool" detect --source . --no-git ;;
            *)        check "$tool" "$tool" ./... ;;
        esac
    else
        echo "SKIP  $tool (not installed)"
    fi
done

if [ "$failures" -gt 0 ]; then
    echo "RESULT: $failures check(s) failed"
    exit 1
fi
echo "RESULT: all checks passed"

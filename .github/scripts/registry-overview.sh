#!/usr/bin/env bash
#
# Render the container-registry overview page (Docker Hub's "full
# description") to stdout.
#
# No container instructions are maintained here — they are read straight from
# the documentation site, so the two can never drift. This script strips the
# Starlight frontmatter and markup, lifts the settings table out of the server
# configuration page, makes relative links absolute, and wraps the result in
# the framing a registry page needs and the site does not, which lives in
# .github/registry/overview-{header,settings,footer}.md.
#
# The settings table is reshaped on the way through: the site leads with the
# CLI flag, but someone reading this page runs a container and has no notion of
# argv, so the flag column is dropped and the environment variable leads.
#
set -euo pipefail

root=$(git rev-parse --show-toplevel)
src="$root/site/src/content/docs/docker.md"
cfg="$root/site/src/content/docs/server-configuration.md"
header="$root/.github/registry/overview-header.md"
settings="$root/.github/registry/overview-settings.md"
footer="$root/.github/registry/overview-footer.md"
site_url="https://yocache.dev"

for f in "$src" "$cfg" "$header" "$settings" "$footer"; do
	[ -r "$f" ] || { echo "registry-overview: cannot read $f" >&2; exit 1; }
done

body=$(awk '
	NR == 1 && $0 == "---" { infm = 1; next }
	infm { if ($0 == "---") infm = 0; next }
	{ print }
' "$src")

# Taking only the "|" lines also drops the <div class="no-wrap-col1"> wrapper
# that styles the table on the site and would be dead markup here.
table=$(awk '/^\|/ { f = 1; print; next } f && !/^\|/ { exit }' "$cfg")

# Reshaping below assumes this exact column order. If the site page changes
# shape, stop rather than silently publishing a mangled table.
expected='| Flag | Env var | Default | What it does |'
if [ "$(printf '%s\n' "$table" | head -1)" != "$expected" ]; then
	echo "registry-overview: unexpected settings table header in $cfg" >&2
	echo "  expected: $expected" >&2
	echo "  found:    $(printf '%s\n' "$table" | head -1)" >&2
	exit 1
fi

table=$(printf '%s\n' "$table" | sed -E 's/^\|[^|]*\|/|/')

out=$(printf '%s\n%s\n\n%s\n\n%s\n\n%s\n' \
	"$(cat "$header")" "$body" "$(cat "$settings")" "$table" "$(cat "$footer")")

# Relative links resolve against the site, not a registry page — absolutise
# them, then refuse to emit anything that still would not resolve off-site.
out=$(printf '%s' "$out" | sed "s#](\.\./#](${site_url}/#g")

if leftover=$(printf '%s' "$out" | grep -oE '\]\([^)]+\)' | grep -vE '^\]\((https?://)'); then
	echo "registry-overview: non-absolute links would break off-site:" >&2
	printf '%s\n' "$leftover" >&2
	exit 1
fi

printf '%s\n' "$out"

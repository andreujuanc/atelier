#!/bin/sh
# Prints the tag of the next release: v<VERSION>.<n>, n one past the
# highest patch already tagged for that MAJOR.MINOR (0 when none), from the
# tags of the git repository in the current directory. VERSION holds
# MAJOR.MINOR; raise it in a PR to start a new line (v0.2.0, …). The new
# tag must sort after every v* tag, else atl update would never offer it.
set -eu
base=$(tr -d ' \t\r\n' < VERSION)
case $base in
'' | *[!0-9.]* | .* | *. | *..* | *.*.*)
	echo "next-version: VERSION must be MAJOR.MINOR, not '$base'" >&2
	exit 1
	;;
*.*) ;;
*)
	echo "next-version: VERSION must be MAJOR.MINOR, not '$base'" >&2
	exit 1
	;;
esac
last=$(git tag --list "v$base.*" | sed -n "s/^v$(printf %s "$base" | sed 's/\./\\./g')\.\([0-9][0-9]*\)$/\1/p" | sort -n | tail -n 1)
if [ -z "$last" ]; then
	next="v$base.0"
else
	next="v$base.$((last + 1))"
fi
newest=$(git tag --list 'v[0-9]*.[0-9]*.[0-9]*' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -n 1 || true)
if [ -n "$newest" ] && [ "$(printf '%s\n%s\n' "$newest" "$next" | sort -V | tail -n 1)" != "$next" ]; then
	echo "next-version: $next would not be newer than $newest: raise VERSION" >&2
	exit 1
fi
echo "$next"

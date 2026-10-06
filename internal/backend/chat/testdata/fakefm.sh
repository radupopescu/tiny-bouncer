#!/bin/sh
# Deterministic stand-in for Apple's `fm` CLI, used by the afm transport tests.
# Supported:
#   fm available [--model M]  -> exit 0 (model ready)
#   fm respond ...            -> reads the command on stdin, prints one canned
#                                deny verdict (with one unknown category so the
#                                mapping's filtering is exercised)
# No network and no Apple Intelligence are involved.
sub="$1"
case "$sub" in
	available)
		echo "System model available"
		exit 0
		;;
	respond)
		cat >/dev/null
		printf '%s\n' '{"effect":"deny","confidence":0.9,"categories":["fs_destructive","bogus"],"reason":"fake fm deny"}'
		exit 0
		;;
	*)
		echo "fakefm: unsupported subcommand ${sub}" >&2
		exit 2
		;;
esac

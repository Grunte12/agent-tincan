#!/bin/sh
# Agent Tincan relay watchdog alert: sends TINCAN_WATCH_MESSAGE by iMessage.
#
# tincan relay-watch runs its --alert-cmd with the alert text in
# TINCAN_WATCH_MESSAGE (and TINCAN_WATCH_EVENT, TINCAN_WATCH_RELAY,
# TINCAN_WATCH_SINCE). This script sends that text from the Messages app on
# this Mac to TINCAN_ALERT_TO, a phone number or Apple ID email. Set the
# recipient in the alert command itself, for example:
#
#   tincan relay-watch install \
#     --alert-cmd 'TINCAN_ALERT_TO=you@example.com /path/to/imessage-alert.sh'
#
# Messages must be signed in to iMessage, and the first run asks to let the
# watchdog control Messages. Test it by hand first:
#
#   TINCAN_ALERT_TO=you@example.com TINCAN_WATCH_MESSAGE=test ./imessage-alert.sh
set -eu

to="${TINCAN_ALERT_TO:-}"
msg="${TINCAN_WATCH_MESSAGE:-}"
if [ -z "$to" ]; then
	echo "imessage-alert: set TINCAN_ALERT_TO to the recipient's phone number or Apple ID email" >&2
	exit 2
fi
if [ -z "$msg" ]; then
	echo "imessage-alert: TINCAN_WATCH_MESSAGE is empty (tincan relay-watch sets it)" >&2
	exit 2
fi

# The recipient and message go in as arguments, never into the script
# text, so quotes in either cannot change what runs.
exec osascript \
	-e 'on run argv' \
	-e '  tell application "Messages"' \
	-e '    set svc to 1st account whose service type = iMessage' \
	-e '    send (item 2 of argv) to participant (item 1 of argv) of svc' \
	-e '  end tell' \
	-e 'end run' \
	"$to" "$msg"

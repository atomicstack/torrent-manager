#!/usr/bin/env bash
# idempotent installer for seedctl on the seed host.
# - ensures the @reboot crontab line exists
# - starts the tmux session immediately if it isn't running
# all paths anchored on $HOME (we expect atmc).

set -euo pipefail

TMUX="$HOME/local/bin/tmux"
BIN="$HOME/bin/seedctl"
CFG="$HOME/etc/seedctl/seedctl.yaml"
SESSION=seedctl
CRON_LINE="@reboot $TMUX new-session -d -s $SESSION '$BIN --config $CFG'"

[[ -x "$TMUX" ]] || { echo "missing tmux at $TMUX" >&2; exit 1; }
[[ -x "$BIN" ]]  || { echo "missing seedctl at $BIN" >&2; exit 1; }
[[ -f "$CFG" ]]  || { echo "missing config at $CFG" >&2; exit 1; }

# crontab idempotent append
existing="$(crontab -l 2>/dev/null || true)"
if ! grep -Fxq "$CRON_LINE" <<<"$existing"; then
  { printf '%s\n' "$existing"; printf '%s\n' "$CRON_LINE"; } | crontab -
  echo "added crontab entry"
else
  echo "crontab entry already present"
fi

# start tmux session if absent
if ! "$TMUX" has-session -t "$SESSION" 2>/dev/null; then
  "$TMUX" new-session -d -s "$SESSION" "$BIN --config $CFG"
  echo "started tmux session $SESSION"
else
  echo "tmux session $SESSION already running"
fi

echo "done"

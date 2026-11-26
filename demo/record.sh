#!/bin/bash
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
work=${OHYA_DEMO_WORK:-/tmp/ohya-demo}
project=$HOME/code/shopfront
state=$work/state
pane=ohya-demo

claude_prompt="test_expired_code_is_rejected fails on CI every evening but passes in the morning. CI runs on LA time. What's going on? Fix it, and let's keep promo dates in UTC from now on."
codex_prompt="Next up: a happy-hour promo, 17:00-19:00 shop time. Did we run into time zone trouble with promos before? What did we decide?"

tmux() {
  command tmux -L ohya-demo -f "$here/tmux.conf" "$@"
}

seed_project() {
  rm -rf "$project"
  mkdir -p "$(dirname "$project")"
  cp -R "$here/shopfront" "$project"
  git -C "$project" init -q -b main
  git -C "$project" remote add origin git@github.com:acme/shopfront.git
  git -C "$project" add -A
  git -C "$project" -c user.name="Sam Rivera" -c user.email=sam@example.com \
    commit -qm "Add promotions with expiry dates"
}

type_text() {
  local text=$1 i
  for ((i = 0; i < ${#text}; i++)); do
    tmux send-keys -t "$pane" -l -- "${text:i:1}"
    sleep "0.0$((35 + RANDOM % 45))"
  done
}

screen() {
  tmux capture-pane -p -t "$pane"
}

wait_for() {
  local pattern=$1 deadline=$((SECONDS + ${2:-60}))
  until screen | grep -q -E "$pattern"; do
    ((SECONDS < deadline)) || { echo "timed out waiting for: $pattern" >&2; screen >&2; exit 1; }
    sleep 0.5
  done
}

wait_until_done() {
  local busy=$1 approve=$2 quiet=0 deadline=$((SECONDS + 600))
  while ((quiet < 6)); do
    ((SECONDS < deadline)) || { echo "agent did not finish" >&2; screen >&2; exit 1; }
    if screen | grep -q -E "$approve"; then
      sleep 1.2
      tmux send-keys -t "$pane" Enter
      quiet=0
    elif screen | grep -q -E "$busy"; then
      quiet=0
    else
      quiet=$((quiet + 1))
    fi
    sleep 0.5
  done
}

record_scene() {
  local cast=$1
  shift
  tmux kill-session -t "$pane" 2>/dev/null || true
  tmux new-session -d -s "$pane" -x 100 -y 30 -c "$project"
  tmux set -t "$pane" window-size manual
  tmux resize-window -t "$pane" -x 100 -y 30
  tmux send-keys -t "$pane" "clear; exec asciinema rec -q --overwrite -c $(printf '%q' "$(printf '%q ' "$@")") $cast" Enter
}

finish_scene() {
  sleep 4
  tmux send-keys -t "$pane" C-c 2>/dev/null || true
  sleep 0.5
  tmux send-keys -t "$pane" C-c 2>/dev/null || true
  sleep 2
  tmux kill-session -t "$pane" 2>/dev/null || true
}

mcp_args="[\"mcp\",\"--db\",\"$state/hub.db\",\"--state\",\"$state\"]"

record_claude() {
  seed_project
  rm -rf "$work"
  mkdir -p "$state" "$work/claude-projects"
  started=$(date +%s)
  record_scene "$work/claude.cast" claude --setting-sources project,local --permission-mode acceptEdits --strict-mcp-config \
    --mcp-config "{\"mcpServers\":{\"ohya\":{\"command\":\"ohya\",\"args\":$mcp_args}}}"
  wait_for '❯' 30
  sleep 2
  type_text "$claude_prompt"
  sleep 0.8
  tmux send-keys -t "$pane" Enter
  sleep 3
  wait_until_done 'esc to interrupt' 'Do you want to proceed\?'
  finish_scene

  session_dir=$HOME/.claude/projects/$(tr '/.' '--' <<<"$project")
  mkdir -p "$work/claude-projects/$(basename "$session_dir")"
  find "$session_dir" -maxdepth 1 -name '*.jsonl' -newermt "@$started" \
    -exec cp {} "$work/claude-projects/$(basename "$session_dir")/" \;
  ohya ingest --once --db "$state/hub.db" --state "$state" \
    --claude-root "$work/claude-projects" --codex-root '' --pi-root '' --antigravity-root ''
}

stop_codex_daemon() {
  pkill -f "$1/packages/app-server-daemon" || true
  sleep 1
}

record_codex() {
  local codex_home=$work/codex-home
  stop_codex_daemon "$codex_home"
  rm -rf "$codex_home"
  mkdir -p "$codex_home"
  cat >"$codex_home/config.toml" <<EOF
model = "gpt-6.1-sol"
model_reasoning_effort = "medium"

[projects."$project"]
trust_level = "trusted"

[mcp_servers.ohya]
command = "ohya"
args = $mcp_args
EOF
  printenv OPENAI_API_KEY | CODEX_HOME=$codex_home codex login --with-api-key >/dev/null
  record_scene "$work/codex.cast" env CODEX_HOME="$codex_home" codex
  wait_for 'Ask Codex' 30
  sleep 2
  type_text "$codex_prompt"
  sleep 0.8
  tmux send-keys -t "$pane" Enter
  sleep 3
  wait_until_done 'esc to interrupt' 'Allow|Approve|Yes, proceed'
  finish_scene
  stop_codex_daemon "$codex_home"
}

case ${1:-all} in
  claude) record_claude ;;
  codex) record_codex ;;
  all) record_claude && record_codex ;;
  *) echo "usage: $0 [claude|codex|all]" >&2; exit 2 ;;
esac
echo "recorded the casts under $work"

#!/bin/bash
export PATH="$HOME/.local/bin:$PATH"
PS1=''
typed() {
  printf '\033[1;32m$\033[0m '
  local -a chars
  while IFS= read -r -n1 c; do
    printf '%s' "$c"
    sleep 0.012
  done <<< "$1"
  sleep 0.25
  printf '\033[0m\n'
  eval "$1" 2>/dev/null
  sleep 0.5
}
clear
printf '\033[1;37m  ohya\033[0m — one memory for all your coding agents\n\n'
sleep 0.8
printf '\033[2;37m  the problem:\033[0m\n'
typed "ls ~/.claude/projects | wc -l; ls ~/.codex/sessions/2026 | wc -l"
printf '\033[2;37m  Claude, Codex, Pi — every agent forgets.\033[0m\n\n'
sleep 0.8
typed "ohya status | head -5"
printf '\033[2;37m  one hub, every session, searchable:\033[0m\n\n'
sleep 0.4
printf '\033[1;37m  now let an agent use it:\033[0m\n'
typed "echo 'Use your ohya memory tool (sql): why did ohya replace the embedded Headscale stack? Cite event ids.' | claude-zai -p --allowed-tools mcp__ohya__sql"
printf '\n\033[2;37m  it answered from shared memory — with evidence.\033[0m\n\n'
sleep 0.6
typed "ohya usage | sed -n '3,7p'"
printf '\033[2;37m  and you can watch agents use it, live.\033[0m\n\n'
sleep 0.8
printf '\033[1;37m  agents that remember their past work.\033[0m\n'
printf '\033[1;37m  github.com/nibzard/ohya\033[0m  ·  \033[2mv0.2.0\033[0m\n\n'
sleep 1.5

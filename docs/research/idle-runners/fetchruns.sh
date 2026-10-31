#!/bin/bash
# The most recent workflow runs (up to 1000) of some public repositories:
# creation time and event, newest first. These are the arrivals of CI work.
out=/tmp/claude-1000/-data-workspace-raccoonci/939c54fa-2146-5c42-b12b-5d51edacc123/scratchpad/traces
mkdir -p "$out"
for repo in cli/cli spf13/cobra vitejs/vite tokio-rs/tokio golangci/golangci-lint prometheus/prometheus pallets/flask psf/requests BurntSushi/ripgrep sharkdp/fd expressjs/express tj/commander.js; do
  f="$out/$(echo $repo | tr / _).tsv"
  : > "$f"
  for page in 1 2 3 4 5 6 7 8 9 10; do
    gh api "repos/$repo/actions/runs?per_page=100&page=$page" --jq '.workflow_runs[] | [.created_at, .event, .name] | @tsv' 2>/dev/null >> "$f" || break
  done
  echo "$repo $(wc -l < "$f")"
done

# wt-tui

Terminal UI for the git worktrees of the repo you run it from.

## Shows

For each worktree:

- branch (or detached dir), with the main worktree marked
- number of uncommitted files (`git status`, untracked files listed individually)
- upstream tracking: `=` in sync, `↑n↓m` ahead/behind, `gone`, `none`
- PR state and number from `gh pr view <branch>`; the detail pane shows its URL
- disk size (`du -sk`), human readable
- `●` when a tmux pane's cwd is inside it; the detail pane names the `session:window`

The detail pane below the list shows the path, last commit, PR link, tmux windows and the uncommitted files.
Status, size and PR load in the background, so slow `du`/`gh` calls never block the list.

## Keys

| key | action |
|---|---|
| `↑` `↓` / `k` `j` | move |
| `enter` / `d` | diff of uncommitted changes (tracked vs `HEAD`, plus untracked files as new files); `q`/`esc` back |
| `c` | copy uncommitted files to a directory |
| `m` | move uncommitted files to a directory (copy, then discard them from the worktree) |
| `x` | remove the worktree (asks `y/N`; dirty trees use `--force`) |
| `o` | open the PR in the browser |
| `r` | reload everything |
| `q` | quit |

Destination prompt (`c`/`m`): `tab` completes directories zsh-style. A unique match completes, several matches extend to
their common prefix and get listed, and further `tab`/`shift+tab` cycle through them. `~` expands, relative paths are
resolved against the directory `wt-tui` was started in, and missing directories are created.

Copy/move keep repo-relative paths (`dest/app/models/x.rb`), never overwrite an existing file in the destination, and
skip files deleted in the worktree. Move only discards changes after every copy succeeded; skipped files stay in the
worktree.

## Build

```sh
./build.sh          # runs the tests, then builds ./wt-tui
```

Copy `wt-tui` somewhere on `$PATH` and run it inside any worktree or the main checkout.

Requires Go 1.23+, `git`. Optional: `gh` (authenticated) for PRs, `tmux` for pane status. `o` uses macOS `open`.
# wt-tui

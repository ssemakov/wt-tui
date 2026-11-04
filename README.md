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
| `u` | undo the last copy/move |
| `x` | remove the worktree (asks `y/N`; dirty trees use `--force`); the row shows a spinner until git finishes |
| `o` | open the PR in the browser |
| `r` | reload everything |
| `q` | quit |

`c`/`m` first list the uncommitted files, all checked: `space` toggles one, `a` toggles all, `enter` goes on to the
destination prompt with the checked files, `esc` cancels.

Destination prompt (`c`/`m`): `tab` completes directories zsh-style. A unique match completes, several matches extend to
their common prefix and get listed, and further `tab`/`shift+tab` cycle through them. `~` expands, relative paths are
resolved against the directory `wt-tui` was started in, and missing directories are created.

Copy/move keep repo-relative paths (`dest/app/models/x.rb`), never overwrite an existing file in the destination, and
skip files deleted in the worktree. Move only discards changes after every copy succeeded; skipped files stay in the
worktree.

`u` undoes the last copy/move of the session. A move undo puts the files back exactly as they were, staged state and
renames included (from a `git stash create` snapshot taken before the discard), and refuses if those paths changed in
the worktree since. Then it deletes the destination copies that still match the worktree, plus the directories it
created for them; copies edited since are left in place.

## Build

```sh
./build.sh          # runs the tests, then builds ./wt-tui
```

Copy `wt-tui` somewhere on `$PATH` and run it inside any worktree or the main checkout.

Requires Go 1.23+, `git`. Optional: `gh` (authenticated) for PRs, `tmux` for pane status. `o` uses macOS `open`.
# wt-tui

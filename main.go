// wt-tui lists the git worktrees of the repo it is run from and helps clean them up.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

type change struct{ xy, path, orig string }

type pr struct {
	State  string `json:"state"`
	Number int    `json:"number"`
	URL    string `json:"url"`
}

type wt struct {
	path, real, branch, head     string
	main, locked, prunable       bool
	upstream, track              string
	tmux                         []string
	files                        []change
	last                         string
	size                         int64
	pr                           *pr
	statusErr, prErr             string
	statusDone, sizeDone, prDone bool
}

type (
	loadedMsg struct {
		mainPath string
		wts      []*wt
		err      error
	}
	statusMsg struct {
		path  string
		files []change
		last  string
		err   error
	}
	sizeMsg struct {
		path string
		size int64
	}
	prMsg struct {
		path string
		pr   *pr
		err  error
	}
	diffMsg struct{ path, text string }
	doneMsg struct {
		path, text string
		undo       *undo
	}
	removedMsg struct {
		path string
		err  error
	}
)

// gh and git status are network/IO bound; du on big checkouts is the slow one.
var (
	gitSem = make(chan struct{}, 8)
	duSem  = make(chan struct{}, 4)
)

func limited(sem chan struct{}, f func() tea.Msg) tea.Cmd {
	return func() tea.Msg {
		sem <- struct{}{}
		defer func() { <-sem }()
		return f()
	}
}

func git(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	return string(out), cmdErr(err)
}

func cmdErr(err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return errors.New(strings.TrimSpace(string(ee.Stderr)))
	}
	return err
}

func realpath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

func expand(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return home + p[1:]
	}
	return p
}

// loadWorktrees returns the main worktree path (where git worktree commands run) and the worktrees.
func loadWorktrees(dir string) (string, []*wt, error) {
	out, err := git(dir, "worktree", "list", "--porcelain")
	if err != nil {
		return "", nil, err
	}
	var all []*wt
	bare := false
	for _, l := range strings.Split(out, "\n") {
		k, v, _ := strings.Cut(l, " ")
		if k == "worktree" {
			all = append(all, &wt{path: v, real: realpath(v)})
			continue
		}
		if len(all) == 0 {
			continue
		}
		w := all[len(all)-1]
		switch k {
		case "HEAD":
			w.head = v
		case "branch":
			w.branch = strings.TrimPrefix(v, "refs/heads/")
		case "locked":
			w.locked = true
		case "prunable":
			w.prunable = true
		case "bare":
			bare = len(all) == 1
		}
	}
	if len(all) == 0 {
		return "", nil, errors.New("no worktrees")
	}
	mainPath := all[0].path
	if bare {
		all = all[1:]
	} else {
		all[0].main = true
	}
	rest := all
	if len(all) > 0 && all[0].main {
		rest = all[1:]
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i].path < rest[j].path })

	refs, _ := git(dir, "for-each-ref", "--format=%(refname:lstrip=2)%00%(upstream:short)%00%(upstream:track)", "refs/heads")
	up := map[string][2]string{}
	for _, l := range strings.Split(refs, "\n") {
		if f := strings.Split(l, "\x00"); len(f) == 3 {
			up[f[0]] = [2]string{f[1], f[2]}
		}
	}
	for _, w := range all {
		w.upstream, w.track = up[w.branch][0], up[w.branch][1]
	}

	// no tmux server -> no panes
	panes, _ := exec.Command("tmux", "list-panes", "-a", "-F",
		"#{pane_current_path}\t#{session_name}:#{window_index} #{window_name}").Output()
	for _, l := range strings.Split(string(panes), "\n") {
		p, label, ok := strings.Cut(l, "\t")
		if !ok {
			continue
		}
		p = realpath(p)
		// longest match wins so a pane in a nested worktree is not credited to its parent
		var best *wt
		for _, w := range all {
			if (p == w.real || strings.HasPrefix(p, w.real+"/")) && (best == nil || len(w.real) > len(best.real)) {
				best = w
			}
		}
		if best != nil && !slices.Contains(best.tmux, label) {
			best.tmux = append(best.tmux, label)
		}
	}
	return mainPath, all, nil
}

func status(path string) ([]change, error) {
	out, err := git(path, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	var cs []change
	parts := strings.Split(out, "\x00")
	for i := 0; i < len(parts); i++ {
		p := parts[i]
		if len(p) < 4 {
			continue
		}
		c := change{xy: p[:2], path: p[3:]}
		// -z prints a rename's source path as the next field
		if strings.ContainsAny(c.xy, "RC") && i+1 < len(parts) {
			i++
			c.orig = parts[i]
		}
		cs = append(cs, c)
	}
	return cs, nil
}

func loadCmd(dir string) tea.Cmd {
	return func() tea.Msg {
		mainPath, wts, err := loadWorktrees(dir)
		return loadedMsg{mainPath, wts, err}
	}
}

func statusCmd(path string) tea.Cmd {
	return limited(gitSem, func() tea.Msg {
		files, err := status(path)
		last, _ := git(path, "log", "-1", "--format=%h %s (%cr)")
		return statusMsg{path, files, strings.TrimSpace(last), err}
	})
}

func sizeCmd(path string) tea.Cmd {
	return limited(duSem, func() tea.Msg {
		// du exits non-zero on unreadable subdirs but still prints the total
		out, _ := exec.Command("du", "-sk", path).Output()
		var kb int64
		if f := strings.Fields(string(out)); len(f) > 0 {
			kb, _ = strconv.ParseInt(f[0], 10, 64)
		}
		return sizeMsg{path, kb * 1024}
	})
}

func prCmd(path, branch string) tea.Cmd {
	return limited(gitSem, func() tea.Msg {
		c := exec.Command("gh", "pr", "view", branch, "--json", "state,number,url")
		c.Dir = path
		out, err := c.Output()
		if err != nil {
			return prMsg{path: path, err: cmdErr(err)}
		}
		var p pr
		if err := json.Unmarshal(out, &p); err != nil {
			return prMsg{path: path, err: err}
		}
		return prMsg{path: path, pr: &p}
	})
}

const diffCap = 256 << 10

func capped(s string) string {
	if len(s) <= diffCap {
		return s
	}
	return s[:diffCap] + "\x1b[0m\n… truncated\n"
}

func diffCmd(path string, files []change) tea.Cmd {
	return func() tea.Msg {
		out, _ := git(path, "diff", "HEAD", "--color=always", "--no-ext-diff")
		parts := []string{capped(out)}
		for _, c := range files {
			if c.xy != "??" {
				continue
			}
			// --no-index exits 1 whenever the files differ
			d, _ := git(path, "diff", "--no-index", "--color=always", "--no-ext-diff", "--", "/dev/null", c.path)
			parts = append(parts, capped(d))
		}
		s := strings.ReplaceAll(strings.Join(parts, ""), "\t", "    ")
		if strings.TrimSpace(s) == "" {
			s = "no uncommitted changes"
		}
		return diffMsg{path, s}
	}
}

func removeCmd(mainPath, path string, force bool) tea.Cmd {
	return func() tea.Msg {
		args := []string{"worktree", "remove"}
		if force {
			args = append(args, "--force")
		}
		// git refuses to remove the worktree its cwd is in, so run from the main one
		_, err := git(mainPath, append(args, path)...)
		return removedMsg{path, err}
	}
}

func transferCmd(path string, files []change, dest string, move bool) tea.Cmd {
	return func() tea.Msg {
		u, text, err := transfer(path, files, dest, move)
		if err != nil {
			text = "error: " + err.Error()
		}
		if u != nil && len(u.files) == 0 {
			u = nil
		}
		return doneMsg{path, text, u}
	}
}

func undoCmd(u *undo) tea.Cmd {
	return func() tea.Msg {
		text, err := revert(u)
		if err != nil {
			// revert refuses to run over files changed since, so the record stays retryable
			return doneMsg{u.path, "error: " + err.Error(), u}
		}
		return doneMsg{u.path, text, nil}
	}
}

// undo records what a transfer did to dest and, for a move, the stash commit holding the discarded changes.
type undo struct {
	path, dest, snap string
	files            []change
	made             []string
	move             bool
}

// tracked lists the paths git restores for cs, rename sources included.
func tracked(cs []change) []string {
	var ps []string
	for _, c := range cs {
		if c.xy == "??" {
			continue
		}
		ps = append(ps, c.path)
		if c.orig != "" {
			ps = append(ps, c.orig)
		}
	}
	return ps
}

// transfer copies uncommitted files under dest keeping repo-relative paths; move then discards them from the worktree.
// Existing dest files are never overwritten, and nothing is discarded unless every copy succeeded.
func transfer(path string, files []change, dest string, move bool) (*undo, string, error) {
	dest, err := filepath.Abs(expand(dest))
	if err != nil {
		return nil, "", err
	}
	u := &undo{path: path, dest: dest}
	var exists, gone int
	for _, c := range files {
		src, dst := filepath.Join(path, c.path), filepath.Join(dest, c.path)
		if _, err := os.Lstat(src); err != nil {
			gone++
			continue
		}
		if _, err := os.Lstat(dst); err == nil {
			exists++
			continue
		}
		// copyPath creates these, so undo removes them
		for d := filepath.Dir(dst); ; d = filepath.Dir(d) {
			if _, err := os.Lstat(d); err == nil {
				break
			}
			u.made = append(u.made, d)
		}
		if err := copyPath(src, dst); err != nil {
			return u, "", fmt.Errorf("after %d file(s), nothing discarded: %w", len(u.files), err)
		}
		u.files = append(u.files, c)
	}
	if move && len(u.files) > 0 {
		// ponytail: the stash commit is unreferenced; gc keeps it ~2 weeks, ample for an in-session undo
		snap, err := git(path, "stash", "create")
		if err != nil {
			return u, "", err
		}
		u.snap, u.move = strings.TrimSpace(snap), true
		for _, c := range u.files {
			if c.xy == "??" {
				if err := os.Remove(filepath.Join(path, c.path)); err != nil {
					return u, "", err
				}
			}
		}
		if t := tracked(u.files); len(t) > 0 {
			args := append([]string{"--literal-pathspecs", "restore", "--source=HEAD", "--staged", "--worktree", "--"}, t...)
			if _, err := git(path, args...); err != nil {
				return u, "", err
			}
		}
	}
	verb := "copied"
	if move {
		verb = "moved"
	}
	s := fmt.Sprintf("%s %d file(s) to %s", verb, len(u.files), dest)
	if exists > 0 {
		s += fmt.Sprintf(", %d skipped (already in dest)", exists)
	}
	if gone > 0 {
		s += fmt.Sprintf(", %d skipped (deleted in worktree)", gone)
	}
	return u, s, nil
}

// revert puts moved files back exactly as they were, then deletes the dest copies that still match the worktree.
func revert(u *undo) (string, error) {
	if u.move {
		check := tracked(u.files)
		for _, c := range u.files {
			if c.xy == "??" {
				check = append(check, c.path)
			}
		}
		out, err := git(u.path, append([]string{"--literal-pathspecs", "status", "--porcelain", "--untracked-files=all", "--"}, check...)...)
		if err != nil {
			return "", err
		}
		if out != "" {
			return "", errors.New("moved files changed in the worktree since, nothing undone")
		}
		if t := tracked(u.files); len(t) > 0 {
			// worktree first: once the index drops a rename source, git no longer matches it to delete the file
			restore := func(src, where string) error {
				_, err := git(u.path, append([]string{"--literal-pathspecs", "restore", "--source=" + src, where, "--"}, t...)...)
				return err
			}
			if err := restore(u.snap, "--worktree"); err != nil {
				return "", err
			}
			// stash commits keep the index as their second parent
			if err := restore(u.snap+"^2", "--staged"); err != nil {
				return "", err
			}
		}
		for _, c := range u.files {
			if c.xy == "??" {
				if err := copyPath(filepath.Join(u.dest, c.path), filepath.Join(u.path, c.path)); err != nil {
					return "", err
				}
			}
		}
	}
	var kept int
	for _, c := range u.files {
		dst := filepath.Join(u.dest, c.path)
		if !same(filepath.Join(u.path, c.path), dst) || os.Remove(dst) != nil {
			kept++
		}
	}
	// children before parents; dirs that still hold other files fail and stay
	slices.SortFunc(u.made, func(a, b string) int { return len(b) - len(a) })
	for _, d := range u.made {
		os.Remove(d)
	}
	verb := "copy"
	if u.move {
		verb = "move"
	}
	s := fmt.Sprintf("undid %s to %s", verb, u.dest)
	if kept > 0 {
		s += fmt.Sprintf(", %d file(s) left there (differ from the worktree)", kept)
	}
	return s, nil
}

// same reports whether b still matches a, so undo never deletes a copy edited since.
func same(a, b string) bool {
	if ta, err := os.Readlink(a); err == nil {
		tb, err := os.Readlink(b)
		return err == nil && ta == tb
	}
	// ponytail: reads both files whole; fine for uncommitted files
	ba, errA := os.ReadFile(a)
	bb, errB := os.ReadFile(b)
	return errA == nil && errB == nil && bytes.Equal(ba, bb)
}

func copyPath(src, dst string) error {
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(t, dst)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s: not a regular file", src)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fi.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// dirCands splits input at its last slash and lists the directories in that dir matching the rest.
func dirCands(input string) (string, []string) {
	base := input[:strings.LastIndex(input, "/")+1]
	prefix := input[len(base):]
	dir := expand(base)
	if dir == "" {
		dir = "."
	}
	ents, _ := os.ReadDir(dir)
	var cands []string
	for _, e := range ents {
		n := e.Name()
		if !strings.HasPrefix(n, prefix) || (strings.HasPrefix(n, ".") && !strings.HasPrefix(prefix, ".")) {
			continue
		}
		// Stat follows symlinks so linked dirs complete too
		if fi, err := os.Stat(filepath.Join(dir, n)); err == nil && fi.IsDir() {
			cands = append(cands, n)
		}
	}
	return base, cands
}

func commonPrefix(ss []string) string {
	p := ss[0]
	for _, s := range ss[1:] {
		for !strings.HasPrefix(s, p) {
			p = p[:len(p)-1]
		}
	}
	return p
}

type mode int

const (
	modeList mode = iota
	modeDiff
	modePick
	modeDest
	modeConfirm
)

type completion struct {
	base    string
	cands   []string
	idx     int
	cycling bool
}

// picker keeps its own worktree and files so a reload or status refresh mid-pick can't retarget the transfer.
type picker struct {
	w     *wt
	files []change
	on    []bool
	cur   int
}

func (p picker) picked() []change {
	var cs []change
	for i, c := range p.files {
		if p.on[i] {
			cs = append(cs, c)
		}
	}
	return cs
}

type model struct {
	dir, mainPath string
	wts           []*wt
	cur, w, h     int
	mode          mode
	vp            viewport.Model
	diffPath      string
	in            textinput.Model
	comp          completion
	pick          picker
	move          bool
	undo          *undo
	lastDest      string
	msg           string
}

var (
	dim     = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	yellow  = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	red     = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	green   = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	magenta = lipgloss.NewStyle().Foreground(lipgloss.Color("5"))
	cyan    = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	plain   = lipgloss.NewStyle()
	bold    = lipgloss.NewStyle().Bold(true)
	selRow  = lipgloss.NewStyle().Reverse(true)
)

func newModel(dir string) model {
	in := textinput.New()
	in.Cursor.SetMode(cursor.CursorStatic)
	in.Focus()
	return model{dir: dir, in: in, vp: viewport.New(0, 0), msg: "loading…"}
}

func (m model) Init() tea.Cmd { return loadCmd(m.dir) }

func (m model) sel() *wt {
	if m.cur < len(m.wts) {
		return m.wts[m.cur]
	}
	return nil
}

func (m model) find(path string) *wt {
	for _, w := range m.wts {
		if w.path == path {
			return w
		}
	}
	return nil
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.vp.Width, m.vp.Height = m.w, m.h-2
	case loadedMsg:
		if msg.err != nil {
			m.msg = "error: " + msg.err.Error()
			return m, nil
		}
		prev := ""
		if w := m.sel(); w != nil {
			prev = w.path
		}
		m.mainPath, m.wts, m.cur, m.msg = msg.mainPath, msg.wts, 0, ""
		var cmds []tea.Cmd
		for i, w := range m.wts {
			if w.path == prev {
				m.cur = i
			}
			if w.prunable {
				w.statusDone, w.sizeDone, w.prDone = true, true, true
				continue
			}
			cmds = append(cmds, statusCmd(w.path), sizeCmd(w.path))
			if w.branch != "" {
				cmds = append(cmds, prCmd(w.path, w.branch))
			} else {
				w.prDone = true
			}
		}
		return m, tea.Batch(cmds...)
	case statusMsg:
		if w := m.find(msg.path); w != nil {
			w.files, w.last, w.statusDone = msg.files, msg.last, true
			if msg.err != nil {
				w.statusErr = msg.err.Error()
			}
		}
	case sizeMsg:
		if w := m.find(msg.path); w != nil {
			w.size, w.sizeDone = msg.size, true
		}
	case prMsg:
		if w := m.find(msg.path); w != nil {
			w.pr, w.prDone = msg.pr, true
			if msg.err != nil {
				w.prErr = msg.err.Error()
			}
		}
	case diffMsg:
		if m.mode == modeDiff && m.diffPath == msg.path {
			m.vp.SetContent(msg.text)
			m.vp.GotoTop()
		}
	case doneMsg:
		m.msg = msg.text
		if msg.undo != nil {
			m.undo = msg.undo
		}
		return m, statusCmd(msg.path)
	case removedMsg:
		if msg.err != nil {
			m.msg = "error: " + msg.err.Error()
			return m, nil
		}
		if w := m.find(msg.path); w != nil {
			m.msg = "removed " + w.name()
		}
		m.wts = slices.DeleteFunc(m.wts, func(w *wt) bool { return w.path == msg.path })
		m.cur = max(0, min(m.cur, len(m.wts)-1))
	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

func (m model) verb() string {
	if m.move {
		return "move"
	}
	return "copy"
}

func (m model) key(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	s := k.String()
	if s == "ctrl+c" {
		return m, tea.Quit
	}
	switch m.mode {
	case modeDiff:
		if s == "q" || s == "esc" {
			m.mode = modeList
			return m, nil
		}
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(k)
		return m, cmd
	case modeConfirm:
		m.mode, m.msg = modeList, "cancelled"
		if w := m.sel(); w != nil && s == "y" {
			m.msg = "removing " + w.name() + "…"
			return m, removeCmd(m.mainPath, w.path, len(w.files) > 0)
		}
		return m, nil
	case modePick:
		p := &m.pick
		switch s {
		case "esc", "q":
			m.mode = modeList
		case "up", "k":
			p.cur = max(0, p.cur-1)
		case "down", "j":
			p.cur = min(len(p.files)-1, p.cur+1)
		case " ":
			p.on[p.cur] = !p.on[p.cur]
		case "a":
			all := !slices.Contains(p.on, false)
			for i := range p.on {
				p.on[i] = !all
			}
		case "enter":
			n := len(p.picked())
			if n == 0 {
				return m, nil
			}
			m.mode = modeDest
			m.in.Prompt = fmt.Sprintf("%s %d file(s) to: ", m.verb(), n)
			m.in.Width = max(10, m.w-len(m.in.Prompt)-2)
			m.in.SetValue(m.lastDest)
			m.in.CursorEnd()
		}
		return m, nil
	case modeDest:
		switch s {
		case "esc":
			m.mode, m.comp = modeList, completion{}
			return m, nil
		case "tab":
			m.tab(1)
			return m, nil
		case "shift+tab":
			m.tab(-1)
			return m, nil
		case "enter":
			dest := strings.TrimSpace(m.in.Value())
			if dest == "" {
				return m, nil
			}
			m.mode, m.comp, m.lastDest, m.msg = modeList, completion{}, dest, "working…"
			return m, transferCmd(m.pick.w.path, m.pick.picked(), dest, m.move)
		}
		m.comp = completion{}
		var cmd tea.Cmd
		m.in, cmd = m.in.Update(k)
		return m, cmd
	}

	switch s {
	case "q":
		return m, tea.Quit
	case "up", "k":
		m.cur = max(0, m.cur-1)
	case "down", "j":
		m.cur = max(0, min(len(m.wts)-1, m.cur+1))
	case "r":
		m.msg = "refreshing…"
		return m, loadCmd(m.dir)
	case "u":
		if m.undo == nil {
			m.msg = "nothing to undo"
			return m, nil
		}
		u := m.undo
		m.undo, m.msg = nil, "undoing…"
		return m, undoCmd(u)
	}
	w := m.sel()
	if w == nil {
		return m, nil
	}
	m.msg = ""
	switch s {
	case "enter", "d":
		m.mode, m.diffPath = modeDiff, w.path
		m.vp.SetContent("loading…")
		return m, diffCmd(w.path, w.files)
	case "c", "m":
		if len(w.files) == 0 {
			m.msg = "no uncommitted files"
			return m, nil
		}
		m.mode, m.move = modePick, s == "m"
		m.pick = picker{w: w, files: w.files, on: slices.Repeat([]bool{true}, len(w.files))}
	case "x":
		if w.main {
			m.msg = "the main worktree can't be removed"
			return m, nil
		}
		m.mode = modeConfirm
	case "o":
		if w.pr == nil {
			m.msg = "no PR"
			return m, nil
		}
		if err := exec.Command("open", w.pr.URL).Start(); err != nil {
			m.msg = "error: " + err.Error()
		}
	}
	return m, nil
}

// tab mimics zsh: a unique match completes, several extend to their common prefix or list, the next tab cycles.
func (m *model) tab(step int) {
	c := &m.comp
	if c.cycling {
		c.idx = (c.idx + step + len(c.cands)) % len(c.cands)
		m.in.SetValue(c.base + c.cands[c.idx] + "/")
		m.in.CursorEnd()
		return
	}
	v := m.in.Value()
	if v == "~" {
		v = "~/"
	}
	listed := len(c.cands) > 1
	base, cands := dirCands(v)
	*c = completion{base: base, cands: cands}
	switch {
	case len(cands) == 0:
		m.msg = "no match"
	case len(cands) == 1:
		m.in.SetValue(base + cands[0] + "/")
		c.cands = nil
	default:
		if p := commonPrefix(cands); len(p) > len(v)-len(base) {
			m.in.SetValue(base + p)
		} else if listed {
			c.cycling = true
			c.idx = 0
			if step < 0 {
				c.idx = len(cands) - 1
			}
			m.in.SetValue(base + cands[c.idx] + "/")
		}
	}
	m.in.CursorEnd()
}

func pad(s string, w int) string {
	s = ansi.Truncate(s, w, "…")
	return s + strings.Repeat(" ", max(0, w-ansi.StringWidth(s)))
}

func human(b int64) string {
	const units = "KMGTP"
	f, i := float64(b), -1
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i < 0 {
		return fmt.Sprintf("%dB", b)
	}
	return fmt.Sprintf("%.1f%c", f, units[i])
}

func (w *wt) name() string {
	n := w.branch
	if n == "" {
		n = "(detached) " + filepath.Base(w.path)
	}
	if w.main {
		n += " (main)"
	}
	return n
}

func (w *wt) upText() string {
	switch {
	case w.branch == "":
		return ""
	case w.upstream == "":
		return "none"
	case w.track == "":
		return "="
	case w.track == "[gone]":
		return "gone"
	}
	// track looks like "[ahead 1, behind 2]"
	r := strings.NewReplacer("[", "", "]", "", ",", "", "ahead ", "↑", "behind ", "↓", " ", "")
	return r.Replace(w.track)
}

func (w *wt) prText() (string, lipgloss.Style) {
	switch {
	case !w.prDone:
		return "…", dim
	case w.pr == nil:
		return "-", dim
	}
	t := fmt.Sprintf("%s #%d", w.pr.State, w.pr.Number)
	switch w.pr.State {
	case "OPEN":
		return t, green
	case "MERGED":
		return t, magenta
	}
	return t, red
}

type cell struct {
	text  string
	width int
	style lipgloss.Style
}

func renderCells(cells []cell, sel bool) string {
	parts := make([]string, len(cells))
	for i, c := range cells {
		parts[i] = pad(c.text, c.width)
		if !sel {
			parts[i] = c.style.Render(parts[i])
		}
	}
	return strings.Join(parts, " ")
}

const fixedCols = 2 + 5 + 10 + 14 + 7 + 5

func (m model) nameW() int { return max(8, m.w-fixedCols) }

func (m model) row(w *wt, sel bool) string {
	chg, chgStyle := "…", dim
	switch {
	case w.statusErr != "":
		chg, chgStyle = "err", red
	case w.statusDone && len(w.files) == 0:
		chg = "-"
	case w.statusDone:
		chg, chgStyle = strconv.Itoa(len(w.files)), yellow
	}
	up, upStyle := w.upText(), dim
	if up == "gone" {
		upStyle = red
	} else if up != "=" && up != "none" {
		upStyle = yellow
	}
	prt, prStyle := w.prText()
	size := "…"
	if w.sizeDone {
		size = human(w.size)
	}
	tmux := ""
	if len(w.tmux) > 0 {
		tmux = "●"
	}
	line := renderCells([]cell{
		{w.name(), m.nameW(), plain},
		{chg, 4, chgStyle},
		{up, 9, upStyle},
		{prt, 13, prStyle},
		{size, 6, dim},
		{tmux, 4, cyan},
	}, sel)
	if sel {
		return selRow.Render(pad("> "+line, m.w))
	}
	return "  " + line
}

func (m model) detail(w *wt) []string {
	kv := func(k, v string) string { return dim.Render(pad(k, 9)) + v }
	branch := w.name()
	if w.upstream != "" {
		branch += dim.Render("  ⇄ " + w.upstream + " " + w.track)
	}
	head := w.last
	if head == "" && len(w.head) >= 7 {
		head = w.head[:7]
	}
	prLine := "…"
	if w.pr != nil {
		t, st := w.prText()
		prLine = st.Render(t) + "  " + w.pr.URL
	} else if w.prDone {
		prLine = dim.Render(firstLine(w.prErr, "-"))
	}
	size := "…"
	if w.sizeDone {
		size = human(w.size)
	}
	lines := []string{
		kv("path", w.path),
		kv("branch", branch),
		kv("head", head),
		kv("pr", prLine),
		kv("size", size),
		kv("tmux", firstLine(strings.Join(w.tmux, ", "), "-")),
	}
	if w.locked {
		lines = append(lines, kv("locked", red.Render("yes")))
	}
	if w.prunable {
		lines = append(lines, kv("prunable", red.Render("directory is missing, run git worktree prune")))
	}
	switch {
	case w.statusErr != "":
		lines = append(lines, kv("changes", red.Render(w.statusErr)))
	case !w.statusDone:
		lines = append(lines, kv("changes", "…"))
	case len(w.files) == 0:
		lines = append(lines, kv("changes", green.Render("clean")))
	default:
		lines = append(lines, kv("changes", yellow.Render(fmt.Sprintf("%d uncommitted", len(w.files)))))
		for _, c := range w.files {
			f := "  " + yellow.Render(c.xy) + " " + c.path
			if c.orig != "" {
				f += dim.Render(" ← " + c.orig)
			}
			lines = append(lines, f)
		}
	}
	return lines
}

func firstLine(s, empty string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if s == "" {
		return empty
	}
	return s
}

// candLines wraps completion candidates into at most 4 rows, scrolled to keep the selected one visible.
func (m model) candLines() []string {
	c := m.comp
	if len(c.cands) < 2 {
		return nil
	}
	var lines []string
	line, selLine := "", 0
	for i, n := range c.cands {
		s := n + "/"
		if c.cycling && i == c.idx {
			s = selRow.Render(s)
		} else {
			s = cyan.Render(s)
		}
		if line != "" && ansi.StringWidth(line)+2+ansi.StringWidth(s) > m.w {
			lines, line = append(lines, line), ""
		}
		if line != "" {
			line += "  "
		}
		line += s
		if c.cycling && i == c.idx {
			selLine = len(lines)
		}
	}
	lines = append(lines, line)
	const maxLines = 4
	if len(lines) <= maxLines {
		return lines
	}
	start := max(0, selLine-maxLines+1)
	return lines[start : start+maxLines]
}

func (m model) pickView() string {
	p := m.pick
	lines := []string{bold.Render(pad(fmt.Sprintf("%s from %s: %d/%d file(s) selected",
		m.verb(), p.w.name(), len(p.picked()), len(p.files)), m.w))}
	rows := m.h - 2
	top := max(0, min(p.cur-rows/2, len(p.files)-rows))
	for i := top; i < min(len(p.files), top+rows); i++ {
		c := p.files[i]
		box := "[ ] "
		if p.on[i] {
			box = "[x] "
		}
		s := box + c.xy + " " + c.path
		if c.orig != "" {
			s += " ← " + c.orig
		}
		switch {
		case i == p.cur:
			s = selRow.Render(pad("> "+s, m.w))
		case p.on[i]:
			s = pad("  "+s, m.w)
		default:
			s = dim.Render(pad("  "+s, m.w))
		}
		lines = append(lines, s)
	}
	for len(lines) < m.h-1 {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n") + "\n" + dim.Render(ansi.Truncate("↑↓/jk move · space toggle · a all/none · enter next · esc cancel", m.w, "…"))
}

func (m model) View() string {
	if m.w == 0 {
		return ""
	}
	if m.mode == modeDiff {
		return bold.Render(pad("diff "+m.diffPath, m.w)) + "\n" + m.vp.View() + "\n" +
			dim.Render("↑↓/jk pgup/pgdn ←→ scroll · q/esc back")
	}
	if m.mode == modePick {
		return m.pickView()
	}

	var foot []string
	switch m.mode {
	case modeDest:
		foot = append([]string{m.in.View()}, m.candLines()...)
		foot = append(foot, dim.Render("tab complete · shift+tab back · enter go · esc cancel"))
	case modeConfirm:
		w := m.sel()
		q := "remove " + w.name() + "?"
		if len(w.files) > 0 {
			q += fmt.Sprintf(" %d uncommitted file(s) will be lost.", len(w.files))
		} else if !w.statusDone {
			q += " status still loading."
		}
		if len(w.tmux) > 0 {
			q += " open in tmux " + strings.Join(w.tmux, ", ") + "."
		}
		foot = []string{red.Render(q + " [y/N]"), ""}
	default:
		foot = []string{m.msg, dim.Render("↑↓/jk move · enter diff · c copy · m move · u undo · x remove · o open PR · r refresh · q quit")}
	}

	header := bold.Render("  " + renderCells([]cell{
		{"WORKTREE", m.nameW(), plain}, {"CHG", 4, plain}, {"UPSTREAM", 9, plain},
		{"PR", 13, plain}, {"SIZE", 6, plain}, {"TMUX", 4, plain},
	}, true))
	lines := []string{header}
	body := m.h - 2 - len(foot)
	if len(m.wts) == 0 {
		lines = append(lines, dim.Render("  no worktrees"))
	} else {
		rows := min(len(m.wts), max(3, body/2))
		// the cursor stays centered so View needs no scroll state
		top := max(0, min(m.cur-rows/2, len(m.wts)-rows))
		for i := top; i < top+rows; i++ {
			lines = append(lines, m.row(m.wts[i], i == m.cur))
		}
		lines = append(lines, dim.Render(strings.Repeat("─", m.w)))
		d := m.detail(m.wts[m.cur])
		if room := body - rows; len(d) > room && room > 0 {
			d = append(d[:room-1], dim.Render(fmt.Sprintf("  … %d more", len(d)-room+1)))
		}
		lines = append(lines, d...)
	}
	for len(lines) < m.h-len(foot) {
		lines = append(lines, "")
	}
	lines = append(lines[:max(0, m.h-len(foot))], foot...)
	for i, l := range lines {
		lines[i] = ansi.Truncate(l, m.w, "…")
	}
	return strings.Join(lines, "\n")
}

func main() {
	dir, _ := os.Getwd()
	if _, err := git(dir, "rev-parse", "--git-dir"); err != nil {
		fmt.Fprintln(os.Stderr, "wt-tui:", err)
		os.Exit(1)
	}
	if _, err := tea.NewProgram(newModel(dir), tea.WithAltScreen()).Run(); err != nil {
		fmt.Fprintln(os.Stderr, "wt-tui:", err)
		os.Exit(1)
	}
}

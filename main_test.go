package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func sh(t *testing.T, dir, script string) {
	t.Helper()
	c := exec.Command("sh", "-c", script)
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", script, err, out)
	}
}

func TestTransferMove(t *testing.T) {
	// user config (signing, hooks) must not leak into the fixture repo
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	repo, dest := t.TempDir(), t.TempDir()
	sh(t, repo, `git init -q -b main &&
		echo a > a.txt && echo r > r.txt && echo k > keep.txt && echo d > del.txt && git add . &&
		git -c user.email=t@t -c user.name=t commit -qm init &&
		echo mod >> a.txt && echo n > new.txt && git add new.txt && git mv r.txt r2.txt && rm del.txt &&
		mkdir "sub dir" && echo u > "sub dir/u.md" && echo mod >> keep.txt`)
	if err := os.WriteFile(filepath.Join(dest, "keep.txt"), []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := status(repo)
	if err != nil {
		t.Fatal(err)
	}
	u, _, err := transfer(repo, files, dest, true)
	if err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]string{
		"a.txt": "a\nmod\n", "new.txt": "n\n", "r2.txt": "r\n", "sub dir/u.md": "u\n", "keep.txt": "existing",
	} {
		got, err := os.ReadFile(filepath.Join(dest, name))
		if err != nil || string(got) != want {
			t.Errorf("dest %s = %q, %v; want %q", name, got, err, want)
		}
	}
	// keep.txt collided in dest and del.txt had nothing to copy, so both stay dirty
	names := func(cs []change) []string {
		var ns []string
		for _, c := range cs {
			ns = append(ns, c.xy+" "+c.path+" "+c.orig)
		}
		return ns
	}
	left, _ := status(repo)
	if got, want := names(left), []string{" D del.txt ", " M keep.txt "}; !slices.Equal(got, want) {
		t.Errorf("worktree left with %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(repo, "r.txt")); err != nil {
		t.Errorf("rename source not restored: %v", err)
	}

	// an edited copy must survive the undo
	if err := os.WriteFile(filepath.Join(dest, "new.txt"), []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	// undo must not restore over worktree edits made after the move
	sh(t, repo, `echo later >> a.txt`)
	if _, err := revert(u); err == nil {
		t.Fatal("revert ran over a worktree edit")
	}
	sh(t, repo, `git checkout -q a.txt`)
	if _, err := revert(u); err != nil {
		t.Fatal(err)
	}
	back, _ := status(repo)
	if got, want := names(back), names(files); !slices.Equal(got, want) {
		t.Errorf("after undo worktree has %q, want %q", got, want)
	}
	if got, _ := os.ReadFile(filepath.Join(repo, "a.txt")); string(got) != "a\nmod\n" {
		t.Errorf("a.txt after undo = %q", got)
	}
	ents, _ := os.ReadDir(dest)
	var left2 []string
	for _, e := range ents {
		left2 = append(left2, e.Name())
	}
	if want := []string{"keep.txt", "new.txt"}; !slices.Equal(left2, want) {
		t.Errorf("dest after undo has %q, want %q", left2, want)
	}
}

func TestTabCompletion(t *testing.T) {
	d := t.TempDir()
	for _, n := range []string{"alpha", "alps", "beta", ".hidden"} {
		if err := os.Mkdir(filepath.Join(d, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(d, "alpaca.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	m := newModel(d)
	steps := []struct {
		input string // "" continues from the previous value
		step  int
		want  string
	}{
		{d + "/a", 1, d + "/alp"},
		{"", 1, d + "/alpha/"},
		{"", 1, d + "/alps/"},
		{"", 1, d + "/alpha/"},
		{"", -1, d + "/alps/"},
		{d + "/b", 1, d + "/beta/"},
		{d + "/.h", 1, d + "/.hidden/"},
		{d + "/", 1, d + "/"},
		{"", 1, d + "/alpha/"},
	}
	for i, s := range steps {
		if s.input != "" {
			m.comp = completion{}
			m.in.SetValue(s.input)
		}
		m.tab(s.step)
		if got := m.in.Value(); got != s.want {
			t.Fatalf("step %d: got %q, want %q", i, got, s.want)
		}
	}
}

func TestPick(t *testing.T) {
	var m tea.Model = newModel(t.TempDir())
	m, _ = m.Update(loadedMsg{mainPath: "/wt", wts: []*wt{{path: "/wt", branch: "b", statusDone: true,
		files: []change{{"??", "a", ""}, {"??", "b", ""}, {"??", "c", ""}}}}})
	for _, k := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune("m")},
		{Type: tea.KeyRunes, Runes: []rune("a")},
		{Type: tea.KeyEnter}, // nothing selected, stays in the picker
		{Type: tea.KeyRunes, Runes: []rune("a")},
		{Type: tea.KeyDown},
		{Type: tea.KeySpace, Runes: []rune(" ")},
		{Type: tea.KeyEnter},
	} {
		m, _ = m.Update(k)
	}
	got := m.(model)
	var paths []string
	for _, c := range got.pick.picked() {
		paths = append(paths, c.path)
	}
	if got.mode != modeDest || !slices.Equal(paths, []string{"a", "c"}) || got.in.Prompt != "move 2 file(s) to: " {
		t.Errorf("mode %d, picked %q, prompt %q", got.mode, paths, got.in.Prompt)
	}
}

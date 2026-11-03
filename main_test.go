package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
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
	if _, err := transfer(repo, files, dest, true); err != nil {
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
	left, _ := status(repo)
	var got []string
	for _, c := range left {
		got = append(got, c.xy+" "+c.path)
	}
	if want := []string{" D del.txt", " M keep.txt"}; !slices.Equal(got, want) {
		t.Errorf("worktree left with %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(repo, "r.txt")); err != nil {
		t.Errorf("rename source not restored: %v", err)
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

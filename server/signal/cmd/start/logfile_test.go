package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotatingFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "vox-signal.log")
	r, err := openRotatingFile(p, 10)
	if err != nil {
		t.Fatal(err)
	}
	opened := 0
	r.SetOnOpen(func(*os.File) { opened++ })

	write := func(s string) {
		if _, err := r.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	write("aaaaaa") // 6
	write("bbbbbb") // 12 > 10 → ротация
	write("cccccc") // снова ротация, прежний .1 заменён
	r.Close()

	cur, _ := os.ReadFile(p)
	prev, _ := os.ReadFile(p + ".1")
	if string(cur) != "cccccc" || string(prev) != "bbbbbb" {
		t.Fatalf("cur=%q prev=%q", cur, prev)
	}
	if opened != 3 {
		t.Fatalf("onOpen вызван %d раз, want 3", opened)
	}

	// Файл больше предела на старте — сразу уходит в .1.
	if err := os.WriteFile(p, []byte(strings.Repeat("x", 20)), 0644); err != nil {
		t.Fatal(err)
	}
	r, err = openRotatingFile(p, 10)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	if info, _ := os.Stat(p); info.Size() != 0 {
		t.Fatalf("на старте не ротирован, size=%d", info.Size())
	}
}

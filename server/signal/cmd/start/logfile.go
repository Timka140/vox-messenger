package main

import (
	"os"
	"sync"
)

// logFileMax — при таком размере vox-signal.log уходит в .1 (прежний .1
// удаляется): диск узла маленький, лог не должен расти бесконечно.
const logFileMax = 50 << 20

// rotatingFile — лог-файл с ротацией по размеру. onOpen вызывается для
// каждого нового файла (перенаправление stderr, чтобы паника попала в
// текущий лог).
type rotatingFile struct {
	mu     sync.Mutex
	path   string
	max    int64
	f      *os.File
	size   int64
	onOpen func(*os.File)
}

func openRotatingFile(path string, max int64) (*rotatingFile, error) {
	r := &rotatingFile{path: path, max: max}
	if err := r.open(); err != nil {
		return nil, err
	}
	if r.size >= r.max {
		if err := r.rotate(); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (r *rotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	var size int64
	if info, err := f.Stat(); err == nil {
		size = info.Size()
	}
	r.f, r.size = f, size
	if r.onOpen != nil {
		r.onOpen(f)
	}
	return nil
}

func (r *rotatingFile) rotate() error {
	r.f.Close()
	if err := os.Rename(r.path, r.path+".1"); err != nil {
		// Не вышло переименовать — начинаем файл заново, лишь бы не расти.
		_ = os.Truncate(r.path, 0)
	}
	return r.open()
}

// SetOnOpen задаёт onOpen и сразу вызывает его для текущего файла.
func (r *rotatingFile) SetOnOpen(fn func(*os.File)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onOpen = fn
	fn(r.f)
}

func (r *rotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size > 0 && r.size+int64(len(p)) > r.max {
		if err := r.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

func (r *rotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.f.Close()
}

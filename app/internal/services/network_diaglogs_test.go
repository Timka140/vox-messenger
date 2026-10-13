package services

import (
	"regexp"
	"testing"
)

func TestDiagLogID(t *testing.T) {
	n, store, _ := newBlockTestNetwork(t)
	id, err := n.diagLogID()
	if err != nil {
		t.Fatal(err)
	}
	// Формат должен совпадать с logstore.ValidClientSource на сигналке.
	if !regexp.MustCompile(`^anon-[0-9a-f]{16}$`).MatchString(id) {
		t.Fatalf("id %q", id)
	}
	again, _ := n.GetDiagLogID()
	if again != id {
		t.Fatalf("id сменился: %q → %q", id, again)
	}
	// Испорченное значение заменяется новым id.
	store.DB().Exec("UPDATE settings SET value = 'Alice#aa' WHERE key = ?", settingDiagLogID)
	if fresh, _ := n.diagLogID(); fresh == "Alice#aa" || !validDiagLogID(fresh) {
		t.Fatalf("испорченный id не заменён: %q", fresh)
	}
}

func TestDiagLogsEnabledDefault(t *testing.T) {
	n, store, _ := newBlockTestNetwork(t)
	if !n.diagLogsEnabled() {
		t.Fatal("по умолчанию отправка должна быть включена")
	}
	set := func(v string) {
		if _, err := store.DB().Exec("INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)", settingDiagLogsEnabled, v); err != nil {
			t.Fatal(err)
		}
	}
	set("0")
	if n.diagLogsEnabled() {
		t.Fatal(`"0" — выключено`)
	}
	set("1")
	if !n.diagLogsEnabled() {
		t.Fatal(`"1" — включено`)
	}
}

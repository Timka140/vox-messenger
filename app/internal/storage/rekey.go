package storage

import (
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Файлы, которые готовит создание профиля из секретной фразы: база, перешифрованная
// ключом из фразы, и сам ключ. Подменяются при следующем старте, до открытия БД.
const (
	PendingDBName  = "vox.db.new"
	PendingKeyName = "vox.key.new"
)

// SwapResult — что сделала ApplyPendingRekey.
type SwapResult int

const (
	SwapNone    SwapResult = iota // подменять нечего
	SwapApplied                   // новые база и ключ на месте
	SwapPartial                   // был только один из двух файлов — удалён, старые не тронуты
	SwapResumed                   // прошлая подмена оборвалась после переноса базы — ключ доставлен
)

// ApplyPendingRekey ставит vox.db.new и vox.key.new на место vox.db и vox.key.
// Ключ переименовывается последним: если процесс оборвётся между шагами, при
// следующем старте база с новым ключом не откроется старым ключом, и сработает
// обычное восстановление через .bak. Одинокий vox.key.new, который открывает
// vox.db, — как раз такой обрыв: доводим подмену, переименовывая ключ.
func ApplyPendingRekey(dataDir string) (SwapResult, error) {
	const op = "storage.ApplyPendingRekey"
	dbPath := filepath.Join(dataDir, "vox.db")
	keyPath := filepath.Join(dataDir, "vox.key")
	newDB := filepath.Join(dataDir, PendingDBName)
	newKey := filepath.Join(dataDir, PendingKeyName)

	hasDB := fileExists(newDB)
	hasKey := fileExists(newKey)
	switch {
	case !hasDB && !hasKey:
		return SwapNone, nil
	case hasKey && !hasDB:
		if key, err := readHexKey(newKey); err == nil && KeyOpens(dbPath, key) {
			if err := os.Rename(newKey, keyPath); err != nil {
				return SwapNone, fmt.Errorf("%s: rename key: %w", op, err)
			}
			return SwapResumed, nil
		}
		_ = os.Remove(newKey)
		return SwapPartial, nil
	case hasDB && !hasKey:
		_ = os.Remove(newDB)
		_ = os.Remove(newKey)
		return SwapPartial, nil
	}

	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return SwapNone, fmt.Errorf("%s: remove %s: %w", op, filepath.Base(p), err)
		}
	}
	if err := os.Rename(newDB, dbPath); err != nil {
		return SwapNone, fmt.Errorf("%s: rename db: %w", op, err)
	}
	if err := os.Rename(newKey, keyPath); err != nil {
		return SwapNone, fmt.Errorf("%s: rename key: %w", op, err)
	}
	return SwapApplied, nil
}

// KeyOpens быстро проверяет, открывается ли зашифрованная БД ключом key
// (без повторов, в отличие от Open). Параметр mode=ro драйвер для пути без
// «file:» отбрасывает, поэтому его нет: проверка только читает sqlite_master.
func KeyOpens(path string, key []byte) bool {
	if !fileExists(path) {
		return false
	}
	dsn := path + fmt.Sprintf("?_pragma_key=x'%s'&_pragma_cipher_page_size=4096", hex.EncodeToString(key))
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return false
	}
	defer db.Close()
	var n int
	return db.QueryRow("SELECT count(*) FROM sqlite_master").Scan(&n) == nil
}

func readHexKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("invalid key file %s", filepath.Base(path))
	}
	return key, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

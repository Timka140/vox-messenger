package files

import (
	"fmt"

	"vox_app/internal/services/files/wire"
	"vox_app/internal/storage/cas"
)

// persistDM транзакционно вставляет message + message_files и инкрементирует
// refcount каждого file (а через него — chunk_blobs).
func (s *FilesService) persistDM(msgID, peerID, myID, caption, fwd string, now int64,
	items []FileItem, manifests []wire.FileManifestEntry) error {

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if _, err := tx.Exec(
		`INSERT INTO messages (id, peer_id, sender_id, content, type, read, delivered, created_at, forwarded_from)
		 VALUES (?, ?, ?, ?, 'attachment', 1, 0, ?, ?)`,
		msgID, peerID, myID, caption, now, fwd); err != nil {
		return fmt.Errorf("insert messages: %w", err)
	}
	for i, m := range manifests {
		if _, err := tx.Exec(
			`INSERT INTO message_files (message_id, file_sha, ord, name, kind)
			 VALUES (?, ?, ?, ?, ?)`,
			msgID, m.Sha, i, m.Name, items[i].Kind); err != nil {
			return fmt.Errorf("insert message_files[%d]: %w", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true

	// Refcount файлов после commit (отдельные tx внутри cas.IncFileRefcount).
	for _, m := range manifests {
		if err := cas.IncFileRefcount(s.db, m.Sha); err != nil {
			s.log.Warn("inc refcount", "file_sha", safeShort8(m.Sha), "err", err.Error())
		}
	}
	return nil
}

// persistGroup — то же для group_messages + group_message_delivery.
func (s *FilesService) persistGroup(msgID, groupID, myID, caption, fwd string, now int64,
	items []FileItem, manifests []wire.FileManifestEntry, members []string) error {

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if _, err := tx.Exec(
		`INSERT INTO group_messages (id, group_id, sender_id, content, type, created_at, forwarded_from)
		 VALUES (?, ?, ?, ?, 'attachment', ?, ?)`,
		msgID, groupID, myID, caption, now, fwd); err != nil {
		return fmt.Errorf("insert group_messages: %w", err)
	}
	for _, peerID := range members {
		if peerID == myID {
			continue
		}
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO group_message_delivery (msg_id, peer_id, delivered) VALUES (?, ?, 0)`,
			msgID, peerID); err != nil {
			return fmt.Errorf("insert delivery: %w", err)
		}
	}
	for i, m := range manifests {
		if _, err := tx.Exec(
			`INSERT INTO message_files (group_msg_id, file_sha, ord, name, kind)
			 VALUES (?, ?, ?, ?, ?)`,
			msgID, m.Sha, i, m.Name, items[i].Kind); err != nil {
			return fmt.Errorf("insert message_files[%d]: %w", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true

	for _, m := range manifests {
		if err := cas.IncFileRefcount(s.db, m.Sha); err != nil {
			s.log.Warn("inc refcount", "file_sha", safeShort8(m.Sha), "err", err.Error())
		}
	}
	return nil
}

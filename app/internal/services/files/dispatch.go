package files

import (
	"log/slog"

	"vox_app/internal/services/files/wire"
)

// HandleFrame — публичный entry-point для входящих бинарных фреймов от
// NetworkService. Вызывается из адаптера в main.go (см. attachMessageRouter).
//
// Возвращает true если frame был обработан этим сервисом (type 0x10..0x14),
// false — если это legacy attachments (0x01..0x02), которые роутятся в
// MessagingService.handleIncomingAttachBinary.
func (s *FilesService) HandleFrame(fromPeer string, frame []byte) bool {
	if len(frame) == 0 {
		return false
	}
	switch wire.FrameType(frame) {
	case wire.TypeFileManifest:
		s.onFileManifest(fromPeer, frame)
	case wire.TypeChunkRequest:
		s.onChunkRequest(fromPeer, frame)
	case wire.TypeChunkData:
		s.onChunkData(fromPeer, frame)
	case wire.TypeChunkNak:
		s.onChunkNak(fromPeer, frame)
	case wire.TypeFileDecline:
		s.onFileDecline(fromPeer, frame)
	default:
		s.log.Debug("files dispatch: unknown frame type",
			slog.String("peer", safeShort(fromPeer)),
			slog.Int("type", int(frame[0])))
		return false
	}
	return true
}

// OnPeerOnline — внешний хук для NetworkService: peer появился в сети,
// перезапускаем все pending downloads с этим peer'ом.
func (s *FilesService) OnPeerOnline(peerID string) {
	rows, err := s.db.Query(
		`SELECT file_sha FROM file_downloads
		 WHERE source_peer = ? AND status IN ('pending', 'active')`,
		peerID)
	if err != nil {
		return
	}
	defer rows.Close()
	var shas []string
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err == nil {
			shas = append(shas, sha)
		}
	}
	rows.Close()
	if len(shas) == 0 {
		return
	}
	s.log.Info("📡 RESUME downloads on peer-online",
		slog.String("peer", safeShort(peerID)),
		slog.Int("count", len(shas)))
	go s.ResumePendingDownloads()
}

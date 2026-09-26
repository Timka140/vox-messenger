package services

import (
	"vox_app/internal/services/files"
)

// FilesService — Wails-обёртка над files.FilesService. Тонкий fasade чтобы
// Wails-binding нашёл методы через хардкод-префикс "vox_app/internal/services."
// в core.js. Реальная логика — в files-package.
//
// Не путать с files.FilesService (внутренний implementation, не binding'ует напрямую).
type FilesService struct {
	impl *files.FilesService
}

// NewFilesService создаёт обёртку. Вызывается в main.go после files.New().
func NewFilesService(impl *files.FilesService) *FilesService {
	return &FilesService{impl: impl}
}

// SendFilesByPath — DM. Делегирует в files.FilesService.
func (s *FilesService) SendFilesByPath(peerID string, items []files.FileItem, caption string) (*files.SendResult, error) {
	return s.impl.SendFilesByPath(peerID, items, caption)
}

// SendGroupFilesByPath — группа.
func (s *FilesService) SendGroupFilesByPath(groupID string, items []files.FileItem, caption string) (*files.SendResult, error) {
	return s.impl.SendGroupFilesByPath(groupID, items, caption)
}

// ApproveFileDownload — юзер принял большой файл (>10 MB) в approval-модалке.
func (s *FilesService) ApproveFileDownload(peerID, msgID string) error {
	return s.impl.ApproveFileDownload(peerID, msgID)
}

// DeclineFileDownload — юзер отклонил.
func (s *FilesService) DeclineFileDownload(peerID, msgID string) error {
	return s.impl.DeclineFileDownload(peerID, msgID)
}

// StartFileDownload — юзер нажал «Скачать» на bubble файла. Backend стартует
// pull у указанного peer'а (по умолчанию sender сообщения; в будущем — swarm).
func (s *FilesService) StartFileDownload(peerID, fileSha string) error {
	return s.impl.StartFileDownload(peerID, fileSha)
}

// CancelFileDownload — юзер нажал «×» во время скачки. Прерываем pull-loop,
// сохраняем текущий bitmap для возможного retry.
func (s *FilesService) CancelFileDownload(fileSha string) error {
	return s.impl.CancelFileDownload(fileSha)
}

// RetryUploadDM — пере-отправка FILE_MANIFEST для DM-сообщения с delivered=0.
// Вызывается из MessagingService.DeliverPendingMessages по реконнекту.
// Не Wails-метод, не binding'ует во фронт.
func (s *FilesService) RetryUploadDM(peerID, msgID string) {
	s.impl.RetryUploadDM(peerID, msgID)
}

// ForwardFiles — пересылка вложений нового формата без повторной загрузки:
// новое сообщение ссылается на те же файлы, получатель тянет их у нас.
// srcKind: "dm" | "group"; ровно один из dstPeerID / dstGroupID.
// Ошибка "not_downloaded" — у нас нет локальной копии какого-то файла.
func (s *FilesService) ForwardFiles(srcMsgID, srcKind, dstPeerID, dstGroupID, caption, fwdName string) (*files.SendResult, error) {
	return s.impl.ForwardFiles(srcMsgID, srcKind, dstPeerID, dstGroupID, caption, fwdName)
}

// ForwardFileShas — пересылка отдельных файлов нового формата одним
// сообщением (выбор в «Медиа», «Переслать» у вложения). Ровно один из
// dstPeerID / dstGroupID; "not_downloaded" — файла нет у нас на диске.
func (s *FilesService) ForwardFileShas(shas []string, dstPeerID, dstGroupID, caption string) (*files.SendResult, error) {
	return s.impl.ForwardFileShas(shas, dstPeerID, dstGroupID, caption)
}

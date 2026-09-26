//go:build windows

package services

import (
	"os/exec"
	"syscall"
)

// makeHiddenCmd создаёт exec.Cmd со скрытым окном консоли. Без этого при
// `cmd.exe /c start ... file.pdf` (OpenAttachment) Windows на ~200 мс мигает
// пустое чёрное окно cmd перед тем как закрыться — на быстрых машинах это
// визуальный артефакт.
//
// CREATE_NO_WINDOW подавляет появление консоли у создаваемого процесса,
// HideWindow дополнительно прячет главное окно если процесс GUI-шный.
func makeHiddenCmd(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
	return cmd
}

// revealInFolder открывает Проводник с выделенным файлом. Командную строку
// собираем сами: exec.Command взял бы в кавычки весь аргумент
// «/select,C:\a b\f.txt», а explorer такой вид не понимает и открывает
// «Документы». Код выхода explorer всегда 1 — ждать его бессмысленно.
func revealInFolder(path string) error {
	cmd := exec.Command("explorer.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine: `explorer.exe /select,"` + path + `"`,
	}
	return cmd.Start()
}

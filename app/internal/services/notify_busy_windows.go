//go:build windows

package services

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// QUERY_USER_NOTIFICATION_STATE (shellapi.h).
const (
	qunsBusy               = 2 // полноэкранное приложение
	qunsRunningD3DFullScr  = 3 // Direct3D в полноэкранном режиме
	qunsPresentationMode   = 4 // режим презентации
	shQueryUserNotifyState = "SHQueryUserNotificationState"
)

var procSHQueryUserNotificationState = windows.NewLazySystemDLL("shell32.dll").NewProc(shQueryUserNotifyState)

// userBusy — пользователь в полноэкранной игре/приложении или презентации
// (то же, по чему Windows сама прячет свои уведомления). Ошибка вызова —
// «не занят»: лучше лишняя карточка, чем потерянная.
func userBusy() bool {
	if procSHQueryUserNotificationState.Find() != nil {
		return false
	}
	var state int32
	hr, _, _ := procSHQueryUserNotificationState.Call(uintptr(unsafe.Pointer(&state)))
	if int32(hr) < 0 {
		return false
	}
	switch state {
	case qunsBusy, qunsRunningD3DFullScr, qunsPresentationMode:
		return true
	}
	return false
}

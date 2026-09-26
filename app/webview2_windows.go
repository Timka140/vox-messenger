package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"github.com/wailsapp/go-webview2/webviewloader"
)

func ensureWebView2() {
	_, err := webviewloader.GetAvailableCoreWebView2BrowserVersionString("")
	if err == nil {
		return
	}

	user32 := syscall.NewLazyDLL("user32.dll")
	messageBox := user32.NewProc("MessageBoxW")
	title, _ := syscall.UTF16PtrFromString("Nexus — требуется WebView2")
	text, _ := syscall.UTF16PtrFromString(
		"Для работы приложения нужен Microsoft Edge WebView2 Runtime.\n\n" +
			"Скачать и установить автоматически?")
	// MB_YESNO=4 | MB_ICONWARNING=0x30
	ret, _, _ := messageBox.Call(0, uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(title)), 0x04|0x30)
	if ret == 6 { // IDYES
		url := "https://go.microsoft.com/fwlink/p/?LinkId=2124703"
		exec.Command("cmd", "/c", "start", url).Start()
		fmt.Println("Откройте приложение после установки WebView2.")
	}
	os.Exit(1)
}

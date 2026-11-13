//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

// Use the native message box even if walk cannot initialize.
func showStartupError(err error) {
	text := fmt.Sprintf("图形界面启动失败：\n%v", err)
	if dir, e := os.UserCacheDir(); e == nil {
		logPath := filepath.Join(dir, "pdf-to-ofd-startup.log")
		if os.WriteFile(logPath, []byte(text), 0600) == nil {
			text += "\n\n错误日志：" + logPath
		}
	}
	fmt.Fprintln(os.Stderr, text)
	msg, _ := syscall.UTF16PtrFromString(text)
	title, _ := syscall.UTF16PtrFromString("PDF ↔ OFD 启动错误")
	syscall.NewLazyDLL("user32.dll").NewProc("MessageBoxW").Call(0, uintptr(unsafe.Pointer(msg)), uintptr(unsafe.Pointer(title)), 0x10)
}

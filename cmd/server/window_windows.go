//go:build windows

package main

import (
	"fmt"
	"syscall"
	"unsafe"
)

// 直接用标准库 syscall 调用 user32.dll 控制应用窗口，
// 不派生 PowerShell 等任何子进程（避免触发安全软件的“隐藏执行 PowerShell”拦截）。
var (
	user32           = syscall.NewLazyDLL("user32.dll")
	pEnumWindows     = user32.NewProc("EnumWindows")
	pGetWindowTextW  = user32.NewProc("GetWindowTextW")
	pGetClassNameW   = user32.NewProc("GetClassNameW")
	pIsWindowVisible = user32.NewProc("IsWindowVisible")
	pShowWindow      = user32.NewProc("ShowWindow")
	pPostMessageW    = user32.NewProc("PostMessageW")
)

const (
	swMinimize = 6
	swMaximize = 3
	swRestore  = 9
	wmClose    = 0x0010
	appTitle   = "Weread Hot Booklists"
)

func winString(proc *syscall.LazyProc, hwnd uintptr) string {
	buf := make([]uint16, 512)
	n, _, _ := proc.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return syscall.UTF16ToString(buf[:n])
}

// findAppWindows 枚举顶层可见窗口，按标题匹配应用窗口（Edge/Chrome 应用模式类名 Chrome_WidgetWin_1）
func findAppWindows() []uintptr {
	var found []uintptr
	cb := syscall.NewCallback(func(hwnd, _ uintptr) uintptr {
		visible, _, _ := pIsWindowVisible.Call(hwnd)
		if visible == 0 {
			return 1
		}
		if winString(pGetWindowTextW, hwnd) == appTitle &&
			winString(pGetClassNameW, hwnd) == "Chrome_WidgetWin_1" {
			found = append(found, hwnd)
		}
		return 1
	})
	pEnumWindows.Call(cb, 0)
	return found
}

// winOp 执行窗口操作：min / max / restore / close
func winOp(op string) error {
	var cmd uintptr
	switch op {
	case "min":
		cmd = swMinimize
	case "max":
		cmd = swMaximize
	case "restore":
		cmd = swRestore
	case "close":
	default:
		return fmt.Errorf("未知操作")
	}
	hwnds := findAppWindows()
	if len(hwnds) == 0 {
		return fmt.Errorf("未找到应用窗口")
	}
	for _, h := range hwnds {
		if op == "close" {
			pPostMessageW.Call(h, wmClose, 0, 0)
		} else {
			pShowWindow.Call(h, cmd)
		}
	}
	return nil
}

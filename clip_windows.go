//go:build windows
// +build windows

// Copyright 2024 clipboard-sync Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"fmt"
	"log"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"golang.design/x/clipboard"

	"golang.org/x/sys/windows"
)

var (
	userDLL     = windows.NewLazyDLL("user32.dll")
	kernel32DLL = windows.NewLazyDLL("kernel32.dll")

	addClipboardFormatListener    = userDLL.NewProc("AddClipboardFormatListener")
	removeClipboardFormatListener = userDLL.NewProc("RemoveClipboardFormatListener")
	registerClassW                = userDLL.NewProc("RegisterClassW")
	isClipboardFormatAvailable    = userDLL.NewProc("IsClipboardFormatAvailable")
	createWindowExW               = userDLL.NewProc("CreateWindowExW")
	destroyWindow                 = userDLL.NewProc("DestroyWindow")
	getMessageW                   = userDLL.NewProc("GetMessageW")
	dispatchMessageW              = userDLL.NewProc("DispatchMessageW")
	translateMessage              = userDLL.NewProc("TranslateMessage")
	defWindowProcW                = userDLL.NewProc("DefWindowProcW")
	getModuleHandleW              = kernel32DLL.NewProc("GetModuleHandleW")
	messageBoxW                   = userDLL.NewProc("MessageBoxW")
)

const (
	WM_CLIPBOARDUPDATE = 0x031D
	WS_EX_TOOLWINDOW   = 0x00000080
	WS_EX_NOACTIVATE   = 0x08000000
	WS_POPUP           = 0x80000000
)

// Windows clipboard formats
const (
	CF_TEXT        = 1
	CF_BITMAP      = 2
	CF_METAFILE    = 3
	CF_DIB         = 8
	CF_UNICODETEXT = 13
	CF_ENHMETAFILE = 14
	CF_HDROP       = 15
	CF_DIBV5       = 17
)

// Clipboard content type IDs for sync protocol
const (
	ClipboardTypeText  = 1
	ClipboardTypeImage = 2
	ClipboardTypeFiles = 3
)

type wndClassExW struct {
	size         uint32
	style        uint32
	wndProc      uintptr
	clsExtra     int32
	wndExtra     int32
	instance     windows.Handle
	icon         windows.Handle
	cursor       windows.Handle
	brBackground windows.Handle
	menuName     *uint16
	className    *uint16
	iconSm       windows.Handle
}

// WNDCLASS is the older window class structure (48 bytes) used with RegisterClass
type wndClassW struct {
	style        uint32
	wndProc      uintptr
	clsExtra     int32
	wndExtra     int32
	instance     windows.Handle
	icon         windows.Handle
	cursor       windows.Handle
	brBackground windows.Handle
	menuName     *uint16
	className    *uint16
}

type point struct {
	x int32
	y int32
}

type msg struct {
	hwnd    windows.Handle
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point
}

var hwnd windows.Handle
var wndProc uintptr

//export wndProcCallback
func wndProcCallback(hWnd windows.Handle, Msg uint32, wParam uintptr, lParam uintptr) uintptr {
	switch Msg {
	case WM_CLIPBOARDUPDATE:
		if clipboardNotify != nil {
			clipboardNotify()
		}
	}
	r, _, _ := defWindowProcW.Call(uintptr(hWnd), uintptr(Msg), wParam, lParam)
	return r
}

func initClipboard() {
	log.Println("windows剪贴板初始化")
	err := clipboard.Init()
	if err != nil {
		msg := fmt.Sprintf("剪贴板初始化失败: %v", err)
		log.Printf("[ERROR] %s", msg)
		showErrorDialog("clipboard-sync - 环境检查失败", msg)
		log.Fatalf("Exiting: clipboard init failed: %v", err)
	}
}

// showErrorDialog shows a Win32 MessageBox error dialog.
func showErrorDialog(title, message string) {
	titlePtr, _ := windows.UTF16PtrFromString(title)
	messagePtr, _ := windows.UTF16PtrFromString(message)
	// MB_ICONERROR = 0x10, MB_OK = 0, MB_SETFOREGROUND = 0x10000
	messageBoxW.Call(0, uintptr(unsafe.Pointer(messagePtr)), uintptr(unsafe.Pointer(titlePtr)), 0x10|0x10000)
}

// ReadClipboardContent 依据类型读取剪贴板
func ReadClipboardContent(mime string) ([]byte, error) {
	if strings.HasPrefix(mime, "image/") {
		return clipboard.Read(clipboard.FmtImage), nil
	} else {
		return clipboard.Read(clipboard.FmtText), nil
	}
}

// SetClipboardContentText 设置剪贴板文本内容
func SetClipboardContentText(content string) error {
	clipboard.Write(clipboard.FmtText, []byte(content))
	return nil
}

// SetClipboardContentImage 设置剪贴板图片内容
func SetClipboardContentImage(image []byte) error {
	clipboard.Write(clipboard.FmtImage, image)
	return nil
}

// ListenClipboardChanges 监听剪贴板返回
func ListenClipboardChanges() <-chan ClipboardChange {
	changes := make(chan ClipboardChange, 1)

	go runMessageLoop(changes)

	log.Println("开始监听剪贴板变化 (Windows native)")

	return changes
}

// clipboardNotify sends a notification that clipboard changed
var clipboardNotify func()

func runMessageLoop(changes chan<- ClipboardChange) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	callback := windows.NewCallback(wndProcCallback)
	wndProc = callback

	instanceRaw, _, err := getModuleHandleW.Call(0)
	if instanceRaw == 0 {
		log.Printf("获取模块handle失败: %v", err)
		return
	}
	instance := windows.Handle(instanceRaw)

	className, _ := windows.UTF16PtrFromString("ClipboardMonitorClass")

	wndClass := wndClassW{
		style:        0x0020, // CS_OWNDC, matches C++ code
		wndProc:      wndProc,
		clsExtra:     0,
		wndExtra:     0,
		instance:     instance,
		icon:         0,
		cursor:       0,
		brBackground: 0,
		menuName:     nil,
		className:    className,
	}

	ret, _, _ := registerClassW.Call(uintptr(unsafe.Pointer(&wndClass)))
	if ret == 0 {
		log.Printf("注册窗口类失败: %v", windows.GetLastError())
		return
	}

	windowName, _ := windows.UTF16PtrFromString("ClipboardMonitor")
	hwndRaw, _, _ := createWindowExW.Call(
		WS_EX_TOOLWINDOW|WS_EX_NOACTIVATE,
		uintptr(ret),
		uintptr(unsafe.Pointer(windowName)),
		WS_POPUP,
		0, 0, 0, 0,
		0,
		0,
		uintptr(instance),
		0,
	)
	hwnd = windows.Handle(hwndRaw)
	if hwnd == 0 {
		log.Printf("创建窗口失败: %v", windows.GetLastError())
		return
	}

	ret, _, _ = addClipboardFormatListener.Call(uintptr(hwnd))
	if ret == 0 {
		log.Printf("注册剪贴板监听失败: %v", windows.GetLastError())
		destroyWindow.Call(uintptr(hwnd))
		return
	}

	log.Printf("剪贴板监听窗口已创建: %d", hwnd)

	events := make(chan struct{}, 1)
	workerStopCh := make(chan struct{})
	workerDoneCh := make(chan struct{})
	go func() {
		defer close(workerDoneCh)
		consumeClipboardEvents(events, workerStopCh, clipboardEventDebounce, func() {
			change := readWindowsClipboardChange()
			select {
			case changes <- change:
			default:
			}
		})
	}()
	defer func() {
		clipboardNotify = nil
		close(workerStopCh)
		<-workerDoneCh
	}()

	clipboardNotify = func() {
		notifyClipboardEvent(events)
	}

	var msg msg
	for {
		ret, _, _ := getMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if ret == 0 {
			break
		}
		translateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		dispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}

	removeClipboardFormatListener.Call(uintptr(hwnd))
	destroyWindow.Call(uintptr(hwnd))
}

func readWindowsClipboardChange() ClipboardChange {
	if debugClipboard {
		log.Printf("剪贴板内容变化，正在串行处理...")
	}

	mime := "text/plain"
	var content []byte

	// 使用Windows API检测剪贴板格式，比直接读取更高效
	if ok, _, _ := isClipboardFormatAvailable.Call(CF_HDROP); ok != 0 {
		mime = "application/x-file-list"
		// 优先读取图片内容，然后识别是否为图片
		content = clipboard.Read(clipboard.FmtImage)
		if len(content) > 0 {
			mime = "image/png"
		}
	} else if ok, _, _ := isClipboardFormatAvailable.Call(CF_UNICODETEXT); ok != 0 {
		mime = "text/plain"
		content = clipboard.Read(clipboard.FmtText)
	} else if ok, _, _ := isClipboardFormatAvailable.Call(CF_BITMAP); ok != 0 {
		mime = "image/png"
		content = clipboard.Read(clipboard.FmtImage)
	} else if ok, _, _ := isClipboardFormatAvailable.Call(CF_DIB); ok != 0 {
		mime = "image/bmp"
		content = clipboard.Read(clipboard.FmtImage)
	} else if ok, _, _ := isClipboardFormatAvailable.Call(CF_ENHMETAFILE); ok != 0 {
		mime = "image/emf"
		content = clipboard.Read(clipboard.FmtImage)
	} else {
		// 回退：尝试读取内容
		content = clipboard.Read(clipboard.FmtText)
		imageContent := clipboard.Read(clipboard.FmtImage)
		if len(imageContent) > 0 {
			mime = "image/png"
			content = imageContent
		} else if len(content) > 0 {
			mime = "text/plain"
		} else {
			mime = "unknown"
		}
	}

	if debugClipboard {
		displayContent := string(content)
		if len(displayContent) > 100 {
			displayContent = displayContent[:100] + "..."
		}
		log.Printf("[DEBUG] 剪贴板变更 - MIME: %s, 内容长度: %d, 内容: %s",
			mime, len(content), displayContent)
	}
	return ClipboardChange{
		Timestamp: time.Now().Unix(),
		Mime:      mime,
		Content:   content,
	}
}

func debugLog(format string, args ...interface{}) {
	if debugClipboard {
		log.Printf("[DEBUG] "+format, args...)
	}
}

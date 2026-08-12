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
	"context"
	"errors"
	"fmt"
	"log"
	"runtime"
	"strings"
	"sync"
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
	openClipboard                 = userDLL.NewProc("OpenClipboard")
	closeClipboard                = userDLL.NewProc("CloseClipboard")
	getClipboardData              = userDLL.NewProc("GetClipboardData")
	setClipboardData              = userDLL.NewProc("SetClipboardData")
	registerClipboardFormatW      = userDLL.NewProc("RegisterClipboardFormatW")
	registerClassW                = userDLL.NewProc("RegisterClassW")
	isClipboardFormatAvailable    = userDLL.NewProc("IsClipboardFormatAvailable")
	createWindowExW               = userDLL.NewProc("CreateWindowExW")
	destroyWindow                 = userDLL.NewProc("DestroyWindow")
	getMessageW                   = userDLL.NewProc("GetMessageW")
	dispatchMessageW              = userDLL.NewProc("DispatchMessageW")
	translateMessage              = userDLL.NewProc("TranslateMessage")
	defWindowProcW                = userDLL.NewProc("DefWindowProcW")
	postMessageW                  = userDLL.NewProc("PostMessageW")
	postQuitMessage               = userDLL.NewProc("PostQuitMessage")
	getClipboardSequenceNumber    = userDLL.NewProc("GetClipboardSequenceNumber")
	getModuleHandleW              = kernel32DLL.NewProc("GetModuleHandleW")
	globalAlloc                   = kernel32DLL.NewProc("GlobalAlloc")
	globalFree                    = kernel32DLL.NewProc("GlobalFree")
	globalLock                    = kernel32DLL.NewProc("GlobalLock")
	globalUnlock                  = kernel32DLL.NewProc("GlobalUnlock")
	globalSize                    = kernel32DLL.NewProc("GlobalSize")
	messageBoxW                   = userDLL.NewProc("MessageBoxW")
)

const (
	WM_CLIPBOARDUPDATE = 0x031D
	WM_CLOSE           = 0x0010
	WM_DESTROY         = 0x0002
	WS_EX_TOOLWINDOW   = 0x00000080
	WS_EX_NOACTIVATE   = 0x08000000
	WS_POPUP           = 0x80000000
	GMEM_MOVEABLE      = 0x0002
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
var windowsOriginFormat uintptr
var windowsOriginFormatOnce sync.Once
var windowsOriginFormatErr error

//export wndProcCallback
func wndProcCallback(hWnd windows.Handle, Msg uint32, wParam uintptr, lParam uintptr) uintptr {
	switch Msg {
	case WM_CLIPBOARDUPDATE:
		if clipboardNotify != nil {
			sequence, _, _ := getClipboardSequenceNumber.Call()
			clipboardNotify(uint64(uint32(sequence)))
		}
	case WM_DESTROY:
		postQuitMessage.Call(0)
		return 0
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
	if ensureWindowsOriginFormat() == 0 {
		log.Printf("[CLIPBOARD] origin-marker register-error=%v", windowsOriginFormatErr)
	}
}

func ensureWindowsOriginFormat() uintptr {
	windowsOriginFormatOnce.Do(func() {
		name, err := windows.UTF16PtrFromString(clipboardOriginMIME)
		if err != nil {
			windowsOriginFormatErr = err
			return
		}
		windowsOriginFormat, _, _ = registerClipboardFormatW.Call(uintptr(unsafe.Pointer(name)))
		if windowsOriginFormat == 0 {
			windowsOriginFormatErr = windows.GetLastError()
		}
	})
	return windowsOriginFormat
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
func SetClipboardContentText(content string, origin ...string) error {
	clipboard.Write(clipboard.FmtText, []byte(content))
	writeWindowsClipboardOrigin(firstClipboardOrigin(origin))
	return nil
}

// SetClipboardContentImage 设置剪贴板图片内容
func SetClipboardContentImage(image []byte, origin ...string) error {
	clipboard.Write(clipboard.FmtImage, image)
	writeWindowsClipboardOrigin(firstClipboardOrigin(origin))
	return nil
}

func writeWindowsClipboardOrigin(origin string) {
	if origin == "" || ensureWindowsOriginFormat() == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(appConfig.Clipboard.ReadTimeoutMS)*time.Millisecond)
	defer cancel()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := openWindowsClipboard(ctx); err != nil {
		log.Printf("[CLIPBOARD] origin-marker write-error=%v", err)
		return
	}
	defer closeClipboard.Call()

	data := []byte(origin)
	handle, _, callErr := globalAlloc.Call(GMEM_MOVEABLE, uintptr(len(data)))
	if handle == 0 {
		log.Printf("[CLIPBOARD] origin-marker alloc-error=%v", callErr)
		return
	}
	owned := false
	defer func() {
		if !owned {
			globalFree.Call(handle)
		}
	}()
	pointer, _, callErr := globalLock.Call(handle)
	if pointer == 0 {
		log.Printf("[CLIPBOARD] origin-marker lock-error=%v", callErr)
		return
	}
	copy(unsafe.Slice((*byte)(unsafe.Pointer(pointer)), len(data)), data)
	globalUnlock.Call(handle)
	result, _, callErr := setClipboardData.Call(windowsOriginFormat, handle)
	if result == 0 {
		log.Printf("[CLIPBOARD] origin-marker set-error=%v", callErr)
		return
	}
	owned = true
}

func openWindowsClipboard(ctx context.Context) error {
	for {
		if result, _, _ := openClipboard.Call(0); result != 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func currentClipboardGeneration() uint64 {
	sequence, _, _ := getClipboardSequenceNumber.Call()
	return uint64(uint32(sequence))
}

// ListenClipboardChanges 监听剪贴板返回
func ListenClipboardChanges() <-chan ClipboardChange {
	changes := make(chan ClipboardChange, 1)
	processor := newClipboardProcessor(appConfig.Clipboard, changes)
	setActiveClipboardProcessor(processor)
	startClipboardWorker(func() {
		processor.Run(stopCh)
		setActiveClipboardProcessor(nil)
		close(changes)
	})
	ready := make(chan error, 1)
	startClipboardWorker(func() { runMessageLoop(processor, ready) })
	select {
	case err := <-ready:
		if err != nil {
			log.Fatalf("创建 Windows 剪贴板监听器失败: %v", err)
		}
	case <-time.After(5 * time.Second):
		log.Fatal("创建 Windows 剪贴板监听器超时")
	}

	log.Println("开始监听剪贴板变化 (Windows native)")

	return changes
}

// clipboardNotify sends a notification that clipboard changed
var clipboardNotify func(uint64)

func runMessageLoop(processor *clipboardProcessor, ready chan<- error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	var readyOnce sync.Once
	reportReady := func(err error) {
		readyOnce.Do(func() { ready <- err })
	}
	defer reportReady(fmt.Errorf("message loop exited before listener was ready"))

	callback := windows.NewCallback(wndProcCallback)
	wndProc = callback

	instanceRaw, _, err := getModuleHandleW.Call(0)
	if instanceRaw == 0 {
		reportReady(fmt.Errorf("获取模块 handle: %w", err))
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
		reportReady(fmt.Errorf("注册窗口类: %w", windows.GetLastError()))
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
		reportReady(fmt.Errorf("创建窗口: %w", windows.GetLastError()))
		return
	}

	ret, _, _ = addClipboardFormatListener.Call(uintptr(hwnd))
	if ret == 0 {
		reportReady(fmt.Errorf("注册剪贴板监听: %w", windows.GetLastError()))
		destroyWindow.Call(uintptr(hwnd))
		return
	}
	log.Printf("剪贴板监听窗口已创建: %d", hwnd)

	defer func() {
		clipboardNotify = nil
	}()

	clipboardNotify = func(sequence uint64) {
		processor.Notify(clipboardPlatformEvent{
			Generation: sequence,
			MIMEs:      windowsClipboardMIMEs(),
			Backend:    "native-windows",
			Read:       readWindowsClipboardSelection,
		})
	}
	go func() {
		<-stopCh
		if hwnd != 0 {
			postMessageW.Call(uintptr(hwnd), WM_CLOSE, 0, 0)
		}
	}()
	reportReady(nil)

	var msg msg
	for {
		ret, _, _ := getMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(ret) == -1 {
			log.Printf("GetMessageW 失败: %v", windows.GetLastError())
			break
		}
		if ret == 0 {
			break
		}
		translateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		dispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}

	removeClipboardFormatListener.Call(uintptr(hwnd))
	destroyWindow.Call(uintptr(hwnd))
}

func windowsClipboardMIMEs() []string {
	var mimes []string
	if ensureWindowsOriginFormat() != 0 {
		if ok, _, _ := isClipboardFormatAvailable.Call(windowsOriginFormat); ok != 0 {
			mimes = append(mimes, clipboardOriginMIME)
		}
	}
	if windowsClipboardImageAvailable() {
		mimes = append(mimes, "image/png")
	}
	if ok, _, _ := isClipboardFormatAvailable.Call(CF_UNICODETEXT); ok != 0 {
		mimes = append(mimes, "text/plain")
	}
	return mimes
}

func windowsClipboardImageAvailable() bool {
	for _, format := range []uintptr{CF_BITMAP, CF_DIBV5, CF_DIB} {
		if ok, _, _ := isClipboardFormatAvailable.Call(format); ok != 0 {
			return true
		}
	}
	return false
}

func readWindowsClipboardSelection(ctx context.Context, requestedMIME string, maxBytes int64) (string, []byte, error) {
	if debugClipboard {
		log.Printf("剪贴板内容变化，正在串行处理...")
	}

	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	if requestedMIME == clipboardOriginMIME {
		content, err := readWindowsClipboardOrigin(ctx, maxBytes)
		return clipboardOriginMIME, content, err
	}
	mime := ""
	var content []byte

	// Images have priority when applications publish both bitmap and text
	// representations of one selection.
	if ok, _, _ := isClipboardFormatAvailable.Call(CF_BITMAP); ok != 0 {
		mime = "image/png"
		content = clipboard.Read(clipboard.FmtImage)
	} else if ok, _, _ := isClipboardFormatAvailable.Call(CF_DIBV5); ok != 0 {
		mime = "image/png"
		content = clipboard.Read(clipboard.FmtImage)
	} else if ok, _, _ := isClipboardFormatAvailable.Call(CF_DIB); ok != 0 {
		mime = "image/png"
		content = clipboard.Read(clipboard.FmtImage)
	} else if ok, _, _ := isClipboardFormatAvailable.Call(CF_UNICODETEXT); ok != 0 {
		mime = "text/plain"
		content = clipboard.Read(clipboard.FmtText)
	}
	if err := ctx.Err(); err != nil {
		return mime, nil, err
	}
	if int64(len(content)) > maxBytes {
		return mime, nil, errClipboardContentTooLarge
	}
	return mime, content, nil
}

func readWindowsClipboardOrigin(ctx context.Context, maxBytes int64) ([]byte, error) {
	if ensureWindowsOriginFormat() == 0 {
		return nil, errors.New("origin marker format is unavailable")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := openWindowsClipboard(ctx); err != nil {
		return nil, err
	}
	defer closeClipboard.Call()
	handle, _, callErr := getClipboardData.Call(windowsOriginFormat)
	if handle == 0 {
		return nil, fmt.Errorf("get origin marker: %w", callErr)
	}
	size, _, _ := globalSize.Call(handle)
	if int64(size) > maxBytes {
		return nil, errClipboardContentTooLarge
	}
	if size == 0 {
		return nil, nil
	}
	pointer, _, callErr := globalLock.Call(handle)
	if pointer == 0 {
		return nil, fmt.Errorf("lock origin marker: %w", callErr)
	}
	defer globalUnlock.Call(handle)
	content := append([]byte(nil), unsafe.Slice((*byte)(unsafe.Pointer(pointer)), int(size))...)
	return content, nil
}

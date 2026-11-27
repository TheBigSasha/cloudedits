//go:build windows

package setupui

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"
)

type comGUID struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

type comVtbl [32]uintptr

type comObject struct {
	vtbl *comVtbl
}

var (
	// CLSID_FileOpenDialog = {DC1C5A9C-E88A-4dde-A5A1-60F82A20AEF7}
	clsidFileOpenDialog = comGUID{
		Data1: 0xDC1C5A9C,
		Data2: 0xE88A,
		Data3: 0x4DDE,
		Data4: [8]byte{0xA5, 0xA1, 0x60, 0xF8, 0x2A, 0x20, 0xAE, 0xF7},
	}
	// IID_IFileOpenDialog = {D57C7288-D4AD-4768-BE02-9D969532D960}
	iidIFileOpenDialog = comGUID{
		Data1: 0xD57C7288,
		Data2: 0xD4AD,
		Data3: 0x4768,
		Data4: [8]byte{0xBE, 0x02, 0x9D, 0x96, 0x95, 0x32, 0xD9, 0x60},
	}

	ole32DLL             = syscall.NewLazyDLL("ole32.dll")
	procCoInitializeEx   = ole32DLL.NewProc("CoInitializeEx")
	procCoUninitialize   = ole32DLL.NewProc("CoUninitialize")
	procCoCreateInstance = ole32DLL.NewProc("CoCreateInstance")
	procCoTaskMemFree    = ole32DLL.NewProc("CoTaskMemFree")
)

const (
	coinitApartmentThreaded = 0x2
	coinitDisableOLE1DDE    = 0x4
	clsctxInprocServer      = 0x1

	fosPickFolders     uint32 = 0x00000020
	fosForceFileSystem uint32 = 0x00000040
	fosPathMustExist   uint32 = 0x00000800

	sigdnFileSysPath uint32 = 0x80058000

	hrOK          uint32 = 0x00000000
	hrFalse       uint32 = 0x00000001
	hrChangedMode uint32 = 0x80010106
	hrCancelled   uint32 = 0x800704C7 // HRESULT_FROM_WIN32(ERROR_CANCELLED)

	// COM vtable indices (shobjidl_core.h):
	vtblRelease        = 2
	vtblDialogShow     = 3
	vtblSetOptions     = 9
	vtblGetOptions     = 10
	vtblSetTitle       = 17
	vtblGetResult      = 20
	vtblGetDisplayName = 5
)

func pickFolderNative(ctx context.Context) (FolderPickResult, error) {
	if ctx.Err() != nil {
		return FolderPickResult{Supported: true, Cancelled: true}, nil
	}
	type outcome struct {
		res FolderPickResult
		err error
	}
	ch := make(chan outcome, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		res, err := runWindowsFolderDialog()
		ch <- outcome{res: res, err: err}
	}()
	select {
	case <-ctx.Done():
		return FolderPickResult{Supported: true, Cancelled: true}, nil
	case out := <-ch:
		return out.res, out.err
	}
}

func runWindowsFolderDialog() (FolderPickResult, error) {
	hr, _, _ := procCoInitializeEx.Call(0, uintptr(coinitApartmentThreaded|coinitDisableOLE1DDE))
	code := uint32(hr)
	if code == hrOK || code == hrFalse {
		defer func() { _, _, _ = procCoUninitialize.Call() }()
	} else if code != hrChangedMode && int32(code) < 0 {
		return FolderPickResult{Supported: true}, fmt.Errorf("CoInitializeEx failed: 0x%08X", code)
	}

	var dlg *comObject
	hr, _, _ = procCoCreateInstance.Call(
		uintptr(unsafe.Pointer(&clsidFileOpenDialog)),
		0,
		uintptr(clsctxInprocServer),
		uintptr(unsafe.Pointer(&iidIFileOpenDialog)),
		uintptr(unsafe.Pointer(&dlg)),
	)
	if int32(uint32(hr)) < 0 || dlg == nil {
		return FolderPickResult{Supported: true}, fmt.Errorf("CoCreateInstance(IFileOpenDialog) failed: 0x%08X", uint32(hr))
	}
	defer func() {
		_, _, _ = syscall.SyscallN(dlg.vtbl[vtblRelease], uintptr(unsafe.Pointer(dlg)))
	}()

	var opts uint32
	hr, _, _ = syscall.SyscallN(dlg.vtbl[vtblGetOptions], uintptr(unsafe.Pointer(dlg)), uintptr(unsafe.Pointer(&opts)))
	if int32(uint32(hr)) < 0 {
		return FolderPickResult{Supported: true}, fmt.Errorf("IFileOpenDialog::GetOptions failed: 0x%08X", uint32(hr))
	}
	opts |= fosPickFolders | fosForceFileSystem | fosPathMustExist
	hr, _, _ = syscall.SyscallN(dlg.vtbl[vtblSetOptions], uintptr(unsafe.Pointer(dlg)), uintptr(opts))
	if int32(uint32(hr)) < 0 {
		return FolderPickResult{Supported: true}, fmt.Errorf("IFileOpenDialog::SetOptions failed: 0x%08X", uint32(hr))
	}

	if title, err := syscall.UTF16PtrFromString("Choose a photo folder for cloudedits"); err == nil {
		_, _, _ = syscall.SyscallN(dlg.vtbl[vtblSetTitle], uintptr(unsafe.Pointer(dlg)), uintptr(unsafe.Pointer(title)))
	}

	hr, _, _ = syscall.SyscallN(dlg.vtbl[vtblDialogShow], uintptr(unsafe.Pointer(dlg)), 0)
	showCode := uint32(hr)
	if showCode == hrCancelled {
		return FolderPickResult{Supported: true, Cancelled: true}, nil
	}
	if int32(showCode) < 0 {
		return FolderPickResult{Supported: true}, fmt.Errorf("IFileOpenDialog::Show failed: 0x%08X", showCode)
	}

	var item *comObject
	hr, _, _ = syscall.SyscallN(dlg.vtbl[vtblGetResult], uintptr(unsafe.Pointer(dlg)), uintptr(unsafe.Pointer(&item)))
	if int32(uint32(hr)) < 0 || item == nil {
		return FolderPickResult{Supported: true}, fmt.Errorf("IFileOpenDialog::GetResult failed: 0x%08X", uint32(hr))
	}
	defer func() {
		_, _, _ = syscall.SyscallN(item.vtbl[vtblRelease], uintptr(unsafe.Pointer(item)))
	}()

	var pszName *uint16
	hr, _, _ = syscall.SyscallN(
		item.vtbl[vtblGetDisplayName],
		uintptr(unsafe.Pointer(item)),
		uintptr(sigdnFileSysPath),
		uintptr(unsafe.Pointer(&pszName)),
	)
	if int32(uint32(hr)) < 0 || pszName == nil {
		return FolderPickResult{Supported: true}, fmt.Errorf("IShellItem::GetDisplayName failed: 0x%08X", uint32(hr))
	}
	defer func() {
		_, _, _ = procCoTaskMemFree.Call(uintptr(unsafe.Pointer(pszName)))
	}()

	picked := filepath.Clean(utf16PtrToString(pszName))
	if picked == "" {
		return FolderPickResult{Supported: true, Cancelled: true}, nil
	}
	return FolderPickResult{Supported: true, Cancelled: false, Path: picked}, nil
}

func utf16PtrToString(p *uint16) string {
	if p == nil {
		return ""
	}
	const maxLen = 32768
	src := unsafe.Slice(p, maxLen)
	n := 0
	for n < maxLen && src[n] != 0 {
		n++
	}
	return syscall.UTF16ToString(src[:n])
}

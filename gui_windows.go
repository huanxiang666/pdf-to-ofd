//go:build windows

// gui_windows.go — Windows 图形界面 (lxn/walk, 纯 Go 无 cgo)
// 双击 exe(无参数) 打开本窗口; 带参数运行时走 main.go 的命令行流程。
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
)

func guiAvailable() bool { return true }

// ensureConsole 命令行模式下准备控制台:
// 有父控制台(从 cmd/终端运行)就附着上去; 没有(如拖文件夹到 exe 上)就新开一个,
// 返回 true 表示这是新开的控制台, 结束时需要等回车再退出以免窗口闪退。
func ensureConsole() (waitEnter bool) {
	k32 := syscall.NewLazyDLL("kernel32.dll")
	// 标准输出已被重定向(管道/文件, 如脚本里 `exe 目录 > log.txt`)时直接写管道, 不开控制台
	hOut, _, _ := k32.NewProc("GetStdHandle").Call(^uintptr(10)) // STD_OUTPUT_HANDLE = -11
	if ft, _, _ := k32.NewProc("GetFileType").Call(hOut); ft == 1 || ft == 3 {
		return false // FILE_TYPE_DISK / FILE_TYPE_PIPE
	}
	if r, _, _ := k32.NewProc("AttachConsole").Call(^uintptr(0)); r == 0 {
		// ATTACH_PARENT_PROCESS 失败 -> 新开控制台
		if r2, _, _ := k32.NewProc("AllocConsole").Call(); r2 == 0 {
			return false
		}
		waitEnter = true
	}
	if out, err := os.OpenFile("CONOUT$", os.O_RDWR, 0); err == nil {
		os.Stdout = out
		os.Stderr = out
	}
	if waitEnter {
		if in, err := os.OpenFile("CONIN$", os.O_RDONLY, 0); err == nil {
			os.Stdin = in
		}
	}
	return waitEnter
}

// pickFolder 系统目录选择对话框 (SHBrowseForFolderW)
func pickFolder(owner walk.Window) string {
	const (
		bifReturnOnlyFSDirs = 0x0001
		bifNewDialogStyle   = 0x0040
		bifEditBox          = 0x0010
	)
	type browseInfoW struct {
		hwndOwner      uintptr
		pidlRoot       uintptr
		pszDisplayName *uint16
		lpszTitle      *uint16
		ulFlags        uint32
		lpfn           uintptr
		lParam         uintptr
		iImage         int32
	}
	shell32 := syscall.NewLazyDLL("shell32.dll")
	ole32 := syscall.NewLazyDLL("ole32.dll")
	title, _ := syscall.UTF16PtrFromString("选择文件夹")
	display := make([]uint16, 1<<15)
	bi := browseInfoW{
		hwndOwner:      uintptr(owner.Handle()),
		pszDisplayName: &display[0],
		lpszTitle:      title,
		ulFlags:        bifReturnOnlyFSDirs | bifNewDialogStyle | bifEditBox,
	}
	pidl, _, _ := shell32.NewProc("SHBrowseForFolderW").Call(uintptr(unsafe.Pointer(&bi)))
	if pidl == 0 {
		return ""
	}
	defer ole32.NewProc("CoTaskMemFree").Call(pidl)
	buf := make([]uint16, 1<<15)
	if r, _, _ := shell32.NewProc("SHGetPathFromIDListW").Call(pidl, uintptr(unsafe.Pointer(&buf[0]))); r == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf)
}

func runGUI() {
	var mw *walk.MainWindow
	var dirLE, outLE *walk.LineEdit
	var logTE *walk.TextEdit
	var recCB, forceCB *walk.CheckBox
	var dpiNE, qNE *walk.NumberEdit
	var startBtn *walk.PushButton
	var settings *walk.Composite
	var busy bool

	logf := func(format string, args ...interface{}) {
		line := fmt.Sprintf(format, args...)
		mw.Synchronize(func() { logTE.AppendText(line + "\r\n") })
	}

	start := func() {
		if busy {
			return
		}
		dir := strings.TrimSpace(dirLE.Text())
		if dir == "" {
			walk.MsgBox(mw, "提示", "请先选择或拖入一个文件夹", walk.MsgBoxIconInformation)
			return
		}
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			walk.MsgBox(mw, "错误", "目录不存在:\n"+dir, walk.MsgBoxIconError)
			return
		}
		absDir, _ := filepath.Abs(dir)
		o := batchOpts{
			recursive: recCB.Checked(),
			dpi:       int(dpiNE.Value()),
			quality:   int(qNE.Value()),
			outDir:    outLE.Text(),
			force:     forceCB.Checked(),
		}
		busy = true
		settings.SetEnabled(false)
		recCB.SetEnabled(false)
		forceCB.SetEnabled(false)
		startBtn.SetEnabled(false)
		logf("目录: %s%s", absDir, map[bool]string{true: " (递归)", false: ""}[o.recursive])
		logf("参数: DPI=%d  JPEG质量=%d%s", o.dpi, o.quality,
			map[bool]string{true: "  覆盖已有输出", false: ""}[o.force])
		logf("--------------------------------------------------")
		go func() {
			ok, fail := runBatch(absDir, o, logf)
			mw.Synchronize(func() {
				busy = false
				settings.SetEnabled(true)
				recCB.SetEnabled(true)
				forceCB.SetEnabled(true)
				startBtn.SetEnabled(true)
				if fail > 0 {
					walk.MsgBox(mw, "完成(有失败)", fmt.Sprintf("成功 %d 个, 失败 %d 个\n详见日志", ok, fail), walk.MsgBoxIconWarning)
				} else {
					walk.MsgBox(mw, "完成", fmt.Sprintf("已处理 %d 个（包含已存在而跳过的文件）\n详见日志", ok), walk.MsgBoxIconInformation)
				}
			})
		}()
	}

	mwDef := MainWindow{
		AssignTo: &mw,
		Title:    "pdf-to-ofd — PDF ↔ 图片型 OFD 双向转换",
		MinSize:  Size{Width: 640, Height: 480},
		Size:     Size{Width: 720, Height: 540},
		Layout:   VBox{},
		OnDropFiles: func(paths []string) {
			if busy {
				return
			}
			for _, p := range paths {
				if fi, err := os.Stat(p); err == nil && fi.IsDir() {
					dirLE.SetText(p)
					return
				}
			}
			// 拖入的是文件 -> 取所在目录
			if len(paths) > 0 {
				dirLE.SetText(filepath.Dir(paths[0]))
			}
		},
		Children: []Widget{
			Composite{
				AssignTo: &settings,
				Layout:   Grid{Columns: 3, Spacing: 8},
				Children: []Widget{
					Label{Text: "文件夹:"},
					LineEdit{AssignTo: &dirLE, CueBanner: "选择文件夹, 或把文件夹拖进本窗口"},
					PushButton{Text: "浏览...", OnClicked: func() {
						if d := pickFolder(mw); d != "" {
							dirLE.SetText(d)
						}
					}},

					Label{Text: "输出到:"},
					LineEdit{AssignTo: &outLE, CueBanner: "留空 = 与源文件同目录"},
					PushButton{Text: "浏览...", OnClicked: func() {
						if d := pickFolder(mw); d != "" {
							outLE.SetText(d)
						}
					}},

					Label{Text: "参数:"},
					Composite{
						Layout: HBox{MarginsZero: true},
						Children: []Widget{
							Label{Text: "DPI"},
							NumberEdit{AssignTo: &dpiNE, MinValue: 72, MaxValue: 600, Decimals: 0},
							Label{Text: " JPEG质量"},
							NumberEdit{AssignTo: &qNE, MinValue: 1, MaxValue: 100, Decimals: 0},
							HSpacer{},
						},
					},
					HSpacer{},
				},
			},
			Composite{
				Layout: HBox{MarginsZero: true},
				Children: []Widget{
					CheckBox{AssignTo: &recCB, Text: "递归子目录"},
					CheckBox{AssignTo: &forceCB, Text: "覆盖已存在的输出"},
					HSpacer{},
					PushButton{AssignTo: &startBtn, Text: "开始转换", OnClicked: func() { start() }},
					PushButton{Text: "清空日志", OnClicked: func() { logTE.SetText("") }},
				},
			},
			TextEdit{AssignTo: &logTE, ReadOnly: true, VScroll: true},
			Label{Text: "方向自动识别: xx.pdf → xx.ofd,  xx.ofd → xx.pdf;  OFD→PDF 仅还原图片型内容, 文字/矢量对象会跳过"},
		},
	}

	if err := mwDef.Create(); err != nil {
		showStartupError(err)
		return
	}
	defer mw.Dispose()
	// SetValue only accepts float64; initialize after the range has been applied.
	if err := dpiNE.SetValue(150); err != nil {
		showStartupError(err)
		return
	}
	if err := qNE.SetValue(85); err != nil {
		showStartupError(err)
		return
	}
	mw.Closing().Attach(func(canceled *bool, reason walk.CloseReason) {
		if busy {
			*canceled = true
			walk.MsgBox(mw, "正在转换", "请等待当前转换结束后再关闭窗口。", walk.MsgBoxIconInformation)
		}
	})
	logTE.SetText("请选择输入文件夹，再点击“开始转换”。\r\n默认保留已有输出；OFD → PDF 仅支持图片型内容。\r\n")
	mw.Run()
}

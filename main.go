// pdf-to-ofd — PDF 与 图片型 OFD 双向转换工具
// 用法:
//
//	pdf-to-ofd                转换当前目录所有 PDF / OFD
//	pdf-to-ofd <目录>         转换指定目录（支持把文件夹拖到程序上）
//	pdf-to-ofd -r <目录>      递归子目录
//
// PDF -> OFD: 每一页渲染成一张 JPEG 打包进 OFD，不做 OCR、不编辑内容。
// OFD -> PDF: 提取页面图片按版式摆进 PDF（仅图片型内容，文字/矢量对象跳过并提示）。
package main

import (
	"archive/zip"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
)

// ======================= PDFium 绑定 (purego, 无 cgo) =======================

var (
	pdfLib          uintptr
	fnInitLibrary   func()
	fnLoadMemDoc    func(data unsafe.Pointer, size int32, pwd unsafe.Pointer) unsafe.Pointer
	fnGetPageCount  func(doc unsafe.Pointer) int32
	fnLoadPage      func(doc unsafe.Pointer, idx int32) unsafe.Pointer
	fnGetPageWidth  func(page unsafe.Pointer) float64
	fnGetPageHeight func(page unsafe.Pointer) float64
	fnClosePage     func(page unsafe.Pointer)
	fnCloseDoc      func(doc unsafe.Pointer)
	fnDestroyLib    func()
	fnBmpCreateEx   func(w, h, format int32, first unsafe.Pointer, stride int32) unsafe.Pointer
	fnBmpDestroy    func(bmp unsafe.Pointer)
	fnRenderBmp     func(bmp, page unsafe.Pointer, sx, sy, sw, sh, rotate, flags int32)
	fnBmpGetBuffer  func(bmp unsafe.Pointer) unsafe.Pointer
)

const (
	fpdfBitmapBGRA = 4    // FPDFBitmap_BGRA
	fpdfAnnot      = 0x01 // FPDF_ANNOT
	fpdfLcdText    = 0x02 // FPDF_LCD_TEXT
)

func loadPdfium() error {
	lib, err := platformLoadLibrary()
	if err != nil {
		return err
	}
	pdfLib = lib
	reg := func(dst interface{}, name string) {
		purego.RegisterLibFunc(dst, pdfLib, name)
	}
	reg(&fnInitLibrary, "FPDF_InitLibrary")
	reg(&fnLoadMemDoc, "FPDF_LoadMemDocument")
	reg(&fnGetPageCount, "FPDF_GetPageCount")
	reg(&fnLoadPage, "FPDF_LoadPage")
	reg(&fnGetPageWidth, "FPDF_GetPageWidth")
	reg(&fnGetPageHeight, "FPDF_GetPageHeight")
	reg(&fnClosePage, "FPDF_ClosePage")
	reg(&fnCloseDoc, "FPDF_CloseDocument")
	reg(&fnDestroyLib, "FPDF_DestroyLibrary")
	reg(&fnBmpCreateEx, "FPDFBitmap_CreateEx")
	reg(&fnBmpDestroy, "FPDFBitmap_Destroy")
	reg(&fnRenderBmp, "FPDF_RenderPageBitmap")
	reg(&fnBmpGetBuffer, "FPDFBitmap_GetBuffer")
	// 新版 PDFium 强制要求: 任何处理函数调用前必须先初始化
	fnInitLibrary()
	return nil
}

// renderPage 从已打开的文档渲染单页为 JPEG 字节流, 返回图片与页面原始尺寸(pt)
func renderPage(doc unsafe.Pointer, pageIdx int, dpi, quality int) ([]byte, float64, float64, error) {
	page := fnLoadPage(doc, int32(pageIdx))
	if page == nil {
		return nil, 0, 0, fmt.Errorf("无法加载第 %d 页", pageIdx+1)
	}
	defer fnClosePage(page)

	ptW := fnGetPageWidth(page)
	ptH := fnGetPageHeight(page)
	if ptW <= 0 || ptH <= 0 {
		ptW, ptH = 595, 842 // 兜底 A4
	}
	scale := float64(dpi) / 72.0
	pxW := int(ptW*scale + 0.5)
	pxH := int(ptH*scale + 0.5)

	// 透明背景的 PDF(如电子发票)未绘制像素会呈黑色/透明, 预填不透明白底
	bg := make([]byte, pxW*pxH*4)
	for i := 0; i < len(bg); i += 4 {
		bg[i], bg[i+1], bg[i+2], bg[i+3] = 0xFF, 0xFF, 0xFF, 0xFF
	}
	defer runtime.KeepAlive(bg)

	bmp := fnBmpCreateEx(int32(pxW), int32(pxH), fpdfBitmapBGRA, unsafe.Pointer(&bg[0]), int32(pxW*4))
	if bmp == nil {
		return nil, 0, 0, fmt.Errorf("位图创建失败")
	}
	defer fnBmpDestroy(bmp)

	fnRenderBmp(bmp, page, 0, 0, int32(pxW), int32(pxH), 0, fpdfAnnot|fpdfLcdText)

	buf := fnBmpGetBuffer(bmp)
	if buf == nil {
		return nil, 0, 0, fmt.Errorf("取位图缓冲区失败")
	}
	raw := unsafe.Slice((*byte)(buf), pxW*pxH*4)

	// BGRA -> RGBA (swizzle)
	rgba := image.NewRGBA(image.Rect(0, 0, pxW, pxH))
	dst := rgba.Pix
	for i := 0; i < len(raw); i += 4 {
		dst[i] = raw[i+2]   // R
		dst[i+1] = raw[i+1] // G
		dst[i+2] = raw[i]   // B
		dst[i+3] = 0xFF
	}

	var out bytes.Buffer
	if err := jpeg.Encode(&out, rgba, &jpeg.Options{Quality: quality}); err != nil {
		return nil, 0, 0, err
	}
	return out.Bytes(), ptW, ptH, nil
}

// ======================= OFD 打包器 (GB/T 33190) =======================
// OFD 文档是 zip 包，结构:
//   OFD.xml                  主入口（文档类型、DocRoot）
//   Doc_0/Document.xml       文档定义（页尺寸、页列表）
//   Doc_0/Pages/Page_N/Content.xml   每页内容（引用图片）
//   Doc_0/Res/image_N.jpeg   图片资源
//   Doc_0/DocumentRes.xml    资源索引

type ofdBuilder struct {
	zw      *zip.Writer
	mediaN  int
	pages   []ofdPage
	pageWmm float64 // 页面宽（毫米）
	pageHmm float64
}

type ofdPage struct {
	imgPath string // zip 内图片路径
	resID   string // 资源 ID
}

// mmPerPoint 是 1pt = 25.4/72 mm
const mmPerPoint = 25.4 / 72.0

func newOFDBuilder(w io.Writer, pageWpt, pageHpt float64) *ofdBuilder {
	return &ofdBuilder{
		zw:      zip.NewWriter(w),
		pageWmm: pageWpt * mmPerPoint,
		pageHmm: pageHpt * mmPerPoint,
	}
}

func (b *ofdBuilder) writeFile(name, content string) error {
	fw, err := b.zw.Create(name)
	if err != nil {
		return err
	}
	_, err = io.WriteString(fw, content)
	return err
}

// addPage 加入一页 JPEG 图片
func (b *ofdBuilder) addPage(jpegData []byte) error {
	b.mediaN++
	imgPath := fmt.Sprintf("Doc_0/Res/image_%d.jpeg", b.mediaN)
	fw, err := b.zw.Create(imgPath)
	if err != nil {
		return err
	}
	if _, err := fw.Write(jpegData); err != nil {
		return err
	}
	b.pages = append(b.pages, ofdPage{
		imgPath: imgPath,
		resID:   fmt.Sprintf("%d", b.mediaN),
	})
	return nil
}

func (b *ofdBuilder) close() error {
	if err := b.writeFile("OFD.xml", b.ofdXML()); err != nil {
		return err
	}
	if err := b.writeFile("Doc_0/Document.xml", b.documentXML()); err != nil {
		return err
	}
	if err := b.writeFile("Doc_0/DocumentRes.xml", b.documentResXML()); err != nil {
		return err
	}
	// 每页 Content.xml
	for i := range b.pages {
		name := fmt.Sprintf("Doc_0/Pages/Page_%d/Content.xml", i+1)
		if err := b.writeFile(name, b.pageContentXML(i+1)); err != nil {
			return err
		}
	}
	return b.zw.Close()
}

func (b *ofdBuilder) ofdXML() string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<ofd:OFD xmlns:ofd="http://www.ofdspec.org/2016" DocType="OFD" Version="1.0"><ofd:DocBody><ofd:DocInfo><ofd:DocID>` +
		fmt.Sprintf("%d", time.Now().UnixNano()) + `</ofd:DocID><ofd:Title>pdf-to-ofd</ofd:Title><ofd:Author>pdf-to-ofd</ofd:Author><ofd:CreationDate>` +
		time.Now().Format("2006-01-02") + `</ofd:CreationDate></ofd:DocInfo><ofd:DocRoot>Doc_0/Document.xml</ofd:DocRoot></ofd:DocBody></ofd:OFD>`
}

func (b *ofdBuilder) documentXML() string {
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	sb.WriteString(`<ofd:Document xmlns:ofd="http://www.ofdspec.org/2016">`)
	sb.WriteString(fmt.Sprintf(`<ofd:CommonData><ofd:MaxUnitID>%d</ofd:MaxUnitID><ofd:PageArea><ofd:PhysicalBox>0 0 %.2f %.2f</ofd:PhysicalBox></ofd:PageArea></ofd:CommonData>`,
		len(b.pages), b.pageWmm, b.pageHmm))
	sb.WriteString(`<ofd:Pages>`)
	for i := range b.pages {
		sb.WriteString(fmt.Sprintf(`<ofd:Page ID="%d" BaseLoc="Pages/Page_%d/Content.xml"/>`, i+1, i+1))
	}
	sb.WriteString(`</ofd:Pages></ofd:Document>`)
	return sb.String()
}

func (b *ofdBuilder) documentResXML() string {
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	sb.WriteString(`<ofd:Res xmlns:ofd="http://www.ofdspec.org/2016" BaseLoc="Res">`)
	for i, p := range b.pages {
		sb.WriteString(fmt.Sprintf(`<ofd:MultiMedia ID="%s" Type="Image"><ofd:MediaFile>%s</ofd:MediaFile></ofd:MultiMedia>`, p.resID, filepath.Base(p.imgPath)))
		_ = i
	}
	sb.WriteString(`</ofd:Res>`)
	return sb.String()
}

func (b *ofdBuilder) pageContentXML(pageIdx int) string {
	p := b.pages[pageIdx-1]
	return `<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		fmt.Sprintf(`<ofd:Page xmlns:ofd="http://www.ofdspec.org/2016"><ofd:Content><ofd:Layer ID="1"><ofd:ImageObject ID="%s" CTM="1 0 0 1 0 0" Boundary="0 0 %.2f %.2f"><ofd:ResourceID>%s</ofd:ResourceID></ofd:ImageObject></ofd:Layer></ofd:Content></ofd:Page>`,
			p.resID, b.pageWmm, b.pageHmm, p.resID)
}

// ======================= 主流程 =======================

func convertPDF2OFD(pdfPath, outPath string, dpi, quality int) (pages int, sizeMB float64, err error) {
	data, err := os.ReadFile(pdfPath)
	if err != nil {
		return 0, 0, err
	}
	if len(data) == 0 {
		return 0, 0, fmt.Errorf("空文件")
	}

	doc := fnLoadMemDoc(unsafe.Pointer(&data[0]), int32(len(data)), nil)
	if doc == nil {
		return 0, 0, fmt.Errorf("PDFium 无法打开文档（文件损坏或加密）")
	}
	n := int(fnGetPageCount(doc))
	p0 := fnLoadPage(doc, 0)
	if p0 == nil {
		fnCloseDoc(doc)
		return 0, 0, fmt.Errorf("无法打开第一页")
	}
	p0w, p0h := fnGetPageWidth(p0), fnGetPageHeight(p0)
	fnClosePage(p0)

	f, err := os.Create(outPath)
	if err != nil {
		fnCloseDoc(doc)
		return 0, 0, err
	}
	defer func() {
		f.Close()
		fnCloseDoc(doc)
		// 关键: PDFium 全程持有 &data[0] 指针, Go 代码里 data 在 LoadMemDoc 后不再被引用,
		// GC 可能中途回收 data 底层数组导致后续页解析失败. KeepAlive 保活到文档关闭.
		runtime.KeepAlive(data)
	}()

	builder := newOFDBuilder(f, p0w, p0h)
	for i := 0; i < n; i++ {
		jpg, _, _, err := renderPage(doc, i, dpi, quality)
		if err != nil {
			return 0, 0, fmt.Errorf("第 %d 页: %v", i+1, err)
		}
		if err := builder.addPage(jpg); err != nil {
			return 0, 0, err
		}
	}
	if err := builder.close(); err != nil {
		return 0, 0, err
	}
	st, _ := os.Stat(outPath)
	return n, float64(st.Size()) / 1024 / 1024, nil
}

type batchOpts struct {
	recursive bool
	dpi       int
	quality   int
	outDir    string
	force     bool
	preview   bool
}

// errInputCollision 表示输出路径将覆盖本批输入文件, 属于保护性拒绝而非程序错误。
var errInputCollision = errors.New("输出与输入冲突")

// batchPathKey 解析已有祖先的符号链接，也适用于尚未创建的输出路径。
func batchPathKey(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	var tail []string
	for {
		resolved, err := filepath.EvalSymlinks(path)
		if err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			if runtime.GOOS == "windows" {
				resolved = strings.ToLower(resolved)
			}
			return resolved, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", err
		}
		tail = append(tail, filepath.Base(path))
		path = parent
	}
}

// batchConvertFile 只在转换成功后提交输出，失败时仅清理自己的临时文件。
func batchConvertFile(outPath string, convert func(string) error) error {
	f, err := os.CreateTemp(filepath.Dir(outPath), ".pdf-to-ofd-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Close(); err != nil {
		return err
	}
	if err := convert(tmp); err != nil {
		return err
	}
	return os.Rename(tmp, outPath)
}

// runBatch 扫描目录并双向转换, 日志通过 logf 输出 (CLI 打印到控制台, GUI 显示在窗口里)
func runBatch(absDir string, o batchOpts, logf func(string, ...interface{})) (okCount, failCount int) {
	t0 := time.Now()
	absDir = filepath.Clean(absDir)
	type task struct {
		src, rel, out, key string
		info               os.FileInfo
		pdf                bool
		err                error
		skip               string // 保护性跳过原因(非错误)
	}
	var tasks []task
	// 扫描不完整时不能保证输入集合完整，因此在任何写入之前终止整批。
	err := filepath.WalkDir(absDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if !o.recursive && path != absDir {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".pdf" && ext != ".ofd" {
			return nil
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("不是普通输入文件: %s", path)
		}
		key, err := batchPathKey(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(absDir, path)
		if err != nil {
			return err
		}
		tasks = append(tasks, task{src: path, rel: rel, key: key, info: info, pdf: ext == ".pdf"})
		return nil
	})
	if err != nil {
		logf("[错误] 扫描失败，未执行转换: %v", err)
		return 0, 1
	}
	if len(tasks) == 0 {
		logf("[完成] 该目录下没有 PDF / OFD 文件")
		return 0, 0
	}
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].pdf != tasks[j].pdf {
			return tasks[i].pdf
		}
		return tasks[i].src < tasks[j].src
	})
	inputs := make(map[string]string, len(tasks))
	for _, t := range tasks {
		inputs[t.key] = t.src
	}
	// 路径键保护大小写/符号链接别名；SameFile 额外保护硬链接输入。
	checkOutput := func(out string) (string, error) {
		key, err := batchPathKey(out)
		if err != nil {
			return "", err
		}
		if src, exists := inputs[key]; exists {
			return key, fmt.Errorf("%w，禁止覆盖: %s", errInputCollision, src)
		}
		info, err := os.Stat(out)
		if err != nil && !os.IsNotExist(err) {
			return key, err
		}
		if err == nil {
			for _, t := range tasks {
				if os.SameFile(info, t.info) {
					return key, fmt.Errorf("%w，禁止覆盖: %s", errInputCollision, t.src)
				}
			}
		}
		return key, nil
	}
	outputs := make(map[string]int, len(tasks))
	for i := range tasks {
		t := &tasks[i]
		ext := ".pdf"
		if t.pdf {
			ext = ".ofd"
		}
		base := t.src
		if o.outDir != "" {
			base = filepath.Join(o.outDir, t.rel)
		}
		t.out = strings.TrimSuffix(base, filepath.Ext(base)) + ext
		key, err := checkOutput(t.out)
		switch {
		case errors.Is(err, errInputCollision) && o.outDir == "":
			// 同名 PDF/OFD 同在输入目录且未指定输出目录: 两个方向都会覆盖对方源文件, 保护性跳过
			t.skip = "同名 PDF/OFD 同在输入目录, 转换将覆盖源文件, 已保护; 请指定独立输出目录(-o 或 GUI「输出到」)"
		case err != nil:
			t.err = err
		default:
			if j, exists := outputs[key]; exists {
				conflict := fmt.Errorf("多个输入指向同一输出: %s", t.out)
				t.err, tasks[j].err = conflict, conflict
			} else {
				outputs[key] = i
			}
		}
	}

	var pdfiumAttempted, pdfiumLoaded bool
	var pdfiumErr error
	pairSkip, existingSkip := 0, 0
	defer func() {
		if pdfiumLoaded {
			fnDestroyLib()
		}
	}()
	for i, t := range tasks {
		if t.skip != "" {
			logf("[%d/%d] 跳过: %s: %s", i+1, len(tasks), t.rel, t.skip)
			pairSkip++
			continue
		}
		if t.err != nil {
			logf("[%d/%d] 失败: %s: %v", i+1, len(tasks), t.rel, t.err)
			failCount++
			continue
		}
		st, err := os.Lstat(t.out)
		if err != nil && !os.IsNotExist(err) {
			logf("[%d/%d] 失败: %s: %v", i+1, len(tasks), t.rel, err)
			failCount++
			continue
		}
		if err == nil && !st.Mode().IsRegular() {
			logf("[%d/%d] 失败: 输出不是普通文件: %s", i+1, len(tasks), t.out)
			failCount++
			continue
		}
		if err == nil && !o.force {
			logf("[%d/%d] 跳过(已存在): %s", i+1, len(tasks), t.rel)
			existingSkip++
			okCount++
			continue
		}
		if err := os.MkdirAll(filepath.Dir(t.out), 0o755); err != nil {
			logf("[%d/%d] 失败: 创建输出目录: %v", i+1, len(tasks), err)
			failCount++
			continue
		}
		logf("[%d/%d] 转换中: %s ...", i+1, len(tasks), t.rel)
		var pages, skipped int
		var sizeMB float64
		err = batchConvertFile(t.out, func(tmp string) error {
			var err error
			if t.pdf {
				// 直到输出检查通过、临时文件可创建后，才需要 PDFium。
				if !pdfiumAttempted {
					pdfiumAttempted = true
					pdfiumErr = loadPdfium()
					pdfiumLoaded = pdfiumErr == nil
				}
				if pdfiumErr != nil {
					return pdfiumErr
				}
				pages, sizeMB, err = convertPDF2OFD(t.src, tmp, o.dpi, o.quality)
			} else {
				pages, skipped, sizeMB, err = convertOFD2PDF(t.src, tmp, o.dpi, o.quality)
			}
			if err != nil {
				return err
			}
			// 提交前重新检查，避免转换期间出现的输入别名或旧输出被覆盖。
			if _, err := checkOutput(t.out); err != nil {
				return err
			}
			if !o.force {
				if _, err := os.Lstat(t.out); err == nil {
					return fmt.Errorf("输出已出现，未覆盖: %s", t.out)
				} else if !os.IsNotExist(err) {
					return err
				}
			}
			return nil
		})
		if err != nil {
			logf("    失败: %v", err)
			failCount++
			continue
		}
		extra := ""
		if skipped > 0 {
			extra = fmt.Sprintf(" (跳过 %d 个文字/矢量对象)", skipped)
		}
		logf("    完成  %d 页, %.1f MB -> %s%s", pages, sizeMB, t.out, extra)
		if o.preview {
			// 两个方向的预览图分别命名, 同名 PDF/OFD 并存时互不覆盖
			ofdSrc := t.src
			suffix := ".preview-pdf.jpg" // 验证 OFD->PDF: 生成 PDF 内嵌的图
			if t.pdf {
				ofdSrc = t.out // 刚生成的 OFD
				suffix = ".preview-ofd.jpg"
			}
			pvp := strings.TrimSuffix(t.out, filepath.Ext(t.out)) + suffix
			if pv, perr := ofdFirstPageImage(ofdSrc); perr == nil {
				if werr := os.WriteFile(pvp, pv, 0o644); werr == nil {
					logf("    预览: %s (打开肉眼验证转换结果)", pvp)
				} else {
					logf("    预览写入失败: %v", werr)
				}
			} else {
				logf("    预览: %v", perr)
			}
		}
		okCount++
	}

	logf("--------------------------------------------------")
	logf("===== 汇总: 成功 %d, 失败 %d, 总耗时 %.1f 秒 =====", okCount, failCount, time.Since(t0).Seconds())
	if existingSkip > 0 {
		logf("提示: %d 个因输出已存在而跳过; 需重新转换请勾选「覆盖已存在的输出」或加 -f", existingSkip)
	}
	if pairSkip > 0 {
		logf("提示: %d 个因同名 PDF/OFD 同在输入目录而保护性跳过; 请指定独立输出目录(-o 或 GUI「输出到」)再转换", pairSkip)
	}
	return okCount, failCount
}

func main() {
	// 双击(无任何参数) -> GUI; 带参数 -> 命令行
	if len(os.Args) == 1 && guiAvailable() {
		runGUI()
		return
	}
	waitEnter := ensureConsole()

	var (
		recursive = flag.Bool("r", false, "递归子目录")
		dpi       = flag.Int("dpi", 150, "渲染 DPI (默认150)")
		quality   = flag.Int("q", 85, "JPEG 质量 1-100 (默认85)")
		outDir    = flag.String("o", "", "输出目录 (默认与源文件同目录)")
		force     = flag.Bool("f", false, "覆盖已存在的输出文件")
		preview   = flag.Bool("preview", true, "转换后生成第一页预览图(.preview-ofd.jpg / .preview-pdf.jpg)用于验证")
	)
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "pdf-to-ofd — PDF <-> 图片型 OFD 双向转换")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "用法:")
		fmt.Fprintln(os.Stderr, "  pdf-to-ofd                双击打开图形界面")
		fmt.Fprintln(os.Stderr, "  pdf-to-ofd <目录>         命令行转换指定目录 (可把文件夹拖到程序上)")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "方向按扩展名自动识别: xx.pdf -> xx.ofd,  xx.ofd -> xx.pdf")
		fmt.Fprintln(os.Stderr, "注: OFD->PDF 仅还原图片型内容, 文字/矢量对象会跳过并提示")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "选项:")
		flag.PrintDefaults()
	}
	flag.Parse()

	dir := "."
	if flag.NArg() > 0 {
		dir = flag.Arg(0)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		fmt.Printf("[错误] 目录不存在: %s\n", dir)
		os.Exit(1)
	}
	absDir, _ := filepath.Abs(dir)

	fmt.Printf("pdf-to-ofd 启动  目录: %s\n", absDir)
	if *recursive {
		fmt.Println("模式: 递归子目录")
	}
	fmt.Printf("参数: DPI=%d  JPEG质量=%d\n", *dpi, *quality)
	fmt.Println("--------------------------------------------------")

	_, failCount := runBatch(absDir, batchOpts{
		recursive: *recursive,
		dpi:       *dpi,
		quality:   *quality,
		outDir:    *outDir,
		force:     *force,
		preview:   *preview,
	}, func(format string, args ...interface{}) {
		fmt.Printf(format+"\n", args...)
	})

	if waitEnter {
		fmt.Print("按回车键退出...")
		fmt.Scanln()
	}
	if failCount > 0 {
		os.Exit(2)
	}
}

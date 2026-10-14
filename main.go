// pdf-to-ofd — PDF 转图片型 OFD 工具
// 用法:
//   pdf-to-ofd                转换当前目录所有 PDF
//   pdf-to-ofd <目录>         转换指定目录（支持把文件夹拖到程序上）
//   pdf-to-ofd -r <目录>      递归子目录
// 每个 PDF 转出同名 .ofd，每一页是一张 JPEG 图片，不做 OCR、不编辑内容。
package main

import (
	"archive/zip"
	"bytes"
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

	bmp := fnBmpCreateEx(int32(pxW), int32(pxH), fpdfBitmapBGRA, nil, 0)
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

func convertOne(pdfPath, outPath string, dpi, quality int) (pages int, sizeMB float64, err error) {
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

func main() {
	var (
		recursive = flag.Bool("r", false, "递归子目录")
		dpi       = flag.Int("dpi", 150, "渲染 DPI (默认150)")
		quality   = flag.Int("q", 85, "JPEG 质量 1-100 (默认85)")
		outDir    = flag.String("o", "", "输出目录 (默认与 PDF 同目录)")
		force     = flag.Bool("f", false, "覆盖已存在的 ofd")
	)
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "pdf-to-ofd — PDF 转图片型 OFD (每页一张图, 不 OCR, 不编辑)")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "用法:")
		fmt.Fprintln(os.Stderr, "  pdf-to-ofd                转换当前目录所有 PDF")
		fmt.Fprintln(os.Stderr, "  pdf-to-ofd <目录>         转换指定目录 (可把文件夹拖到程序上)")
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

	if err := loadPdfium(); err != nil {
		fmt.Printf("[错误] %v\n", err)
		fmt.Println("提示: Windows 用户请把 pdfium.dll 放到本程序同目录")
		os.Exit(1)
	}
	defer fnDestroyLib()

	var pdfs []string
	_ = filepath.WalkDir(absDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if !*recursive && path != absDir {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.ToLower(filepath.Ext(path)) == ".pdf" {
			pdfs = append(pdfs, path)
		}
		return nil
	})
	sort.Strings(pdfs)

	if len(pdfs) == 0 {
		fmt.Println("[完成] 该目录下没有 PDF 文件")
		return
	}

	okCount, failCount := 0, 0
	t0 := time.Now()
	for i, pdfPath := range pdfs {
		rel, _ := filepath.Rel(absDir, pdfPath)
		outPath := strings.TrimSuffix(pdfPath, filepath.Ext(pdfPath)) + ".ofd"
		if *outDir != "" {
			os.MkdirAll(*outDir, 0o755)
			outPath = filepath.Join(*outDir, strings.TrimSuffix(filepath.Base(pdfPath), filepath.Ext(pdfPath))+".ofd")
		}
		if !*force {
			if _, err := os.Stat(outPath); err == nil {
				fmt.Printf("[%d/%d] 跳过(已存在): %s\n", i+1, len(pdfs), rel)
				okCount++
				continue
			}
		}
		fmt.Printf("[%d/%d] 转换中: %s ... ", i+1, len(pdfs), rel)
		pages, sizeMB, err := convertOne(pdfPath, outPath, *dpi, *quality)
		if err != nil {
			fmt.Printf("失败: %v\n", err)
			os.Remove(outPath)
			failCount++
			continue
		}
		fmt.Printf("完成  %d 页, %.1f MB -> %s\n", pages, sizeMB, filepath.Base(outPath))
		okCount++
	}
	fmt.Println("--------------------------------------------------")
	fmt.Printf("===== 汇总: 成功 %d, 失败 %d, 总耗时 %.1f 秒 =====\n", okCount, failCount, time.Since(t0).Seconds())
	if failCount > 0 {
		os.Exit(2)
	}
}

// ofd2pdf.go — OFD(图片型) 转 PDF
//
// 原理: OFD 是 zip 包(OFD.xml -> Document.xml -> DocumentRes.xml -> Pages/*/Content.xml),
// 解析出每页的 ImageObject(Boundary/CTM/ResourceID), 把图片按版式摆进 PDF 页面。
//
// 快车道: 页面全是 JPEG 时直接嵌入原始字节 (DCTDecode), 零重压缩, 往返无损。
// 慢车道: 含透明 PNG/GIF 或无法直接嵌入的图片时, 整页按 DPI 合成渲染(仿射+双线性+alpha 混合)。
// 不支持 TextObject / PathObject / CompositeObject(跳过并计数提示)。
package main

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"math"
	"os"
	"path"
	"strconv"
	"strings"
)

// ======================= OFD 解析 =======================

type xOfdRoot struct {
	DocRoots []string `xml:"DocBody>DocRoot"`
}

type xDocument struct {
	PhysicalBox string     `xml:"CommonData>PageArea>PhysicalBox"`
	DocRes      []string   `xml:"CommonData>DocumentRes"`
	Pages       []xDocPage `xml:"Pages>Page"`
}

type xDocPage struct {
	BaseLoc string `xml:"BaseLoc,attr"`
}

type xRes struct {
	BaseLoc string `xml:"BaseLoc,attr"`
	Media   []xMed `xml:"MultiMedia"`
}

type xMed struct {
	ID   string `xml:"ID,attr"`
	File string `xml:"MediaFile"`
}

type xPageContent struct {
	PhysicalBox string    `xml:"Area>PhysicalBox"`
	Images      []xImgObj `xml:"Content>Layer>ImageObject"`
	Texts       []xAny    `xml:"Content>Layer>TextObject"`
	Paths       []xAny    `xml:"Content>Layer>PathObject"`
	Composites  []xAny    `xml:"Content>Layer>CompositeObject"`
}

type xAny struct{}

type xImgObj struct {
	Boundary  string `xml:"Boundary,attr"`
	CTM       string `xml:"CTM,attr"`
	ResIDAttr string `xml:"ResourceID,attr"` // 规范写法: 属性
	ResIDElem string `xml:"ResourceID"`      // 兼容写法: 子元素(本工具正向生成的)
}

type ofdImage struct {
	data     []byte
	boundary [4]float64 // x y w h (毫米, 页面坐标系)
	ctm      mat
}

func (im ofdImage) isJPEG() bool {
	return len(im.data) >= 2 && im.data[0] == 0xFF && im.data[1] == 0xD8
}

type ofdPageData struct {
	x0, y0   float64 // PhysicalBox 原点 (毫米)
	wmm, hmm float64 // 页面尺寸 (毫米)
	images   []ofdImage
}

func parseNums(s string) []float64 {
	f := strings.Fields(strings.ReplaceAll(s, ",", " "))
	out := make([]float64, 0, len(f))
	for _, t := range f {
		v, err := strconv.ParseFloat(t, 64)
		if err != nil {
			return nil
		}
		out = append(out, v)
	}
	return out
}

func cleanZipPath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	p = path.Clean(strings.TrimPrefix(p, "/"))
	for strings.HasPrefix(p, "../") {
		p = strings.TrimPrefix(p, "../")
	}
	return p
}

// ======================= 2D 仿射矩阵 =======================
// mat = [a b c d e f]: x' = a*x + c*y + e,  y' = b*x + d*y + f

type mat [6]float64

var matIdentity = mat{1, 0, 0, 1, 0, 0}

// matMul 返回 m1∘m2 (先应用 m2, 再应用 m1)
func matMul(m1, m2 mat) mat {
	return mat{
		m1[0]*m2[0] + m1[2]*m2[1],
		m1[1]*m2[0] + m1[3]*m2[1],
		m1[0]*m2[2] + m1[2]*m2[3],
		m1[1]*m2[2] + m1[3]*m2[3],
		m1[0]*m2[4] + m1[2]*m2[5] + m1[4],
		m1[1]*m2[4] + m1[3]*m2[5] + m1[5],
	}
}

func matApply(m mat, x, y float64) (float64, float64) {
	return m[0]*x + m[2]*y + m[4], m[1]*x + m[3]*y + m[5]
}

// ======================= 页面读取 =======================

func readOFDPage(files map[string]*zip.File, readXML func(string, interface{}) error,
	docDir, baseLoc string, docBox []float64, resMap map[string]string) (ofdPageData, int, error) {

	var pg ofdPageData
	cp := cleanZipPath(path.Join(docDir, baseLoc))
	var xc xPageContent
	if err := readXML(cp, &xc); err != nil {
		return pg, 0, fmt.Errorf("页面 %s: %v", cp, err)
	}
	box := docBox
	if b := parseNums(xc.PhysicalBox); len(b) == 4 && b[2] > 0 && b[3] > 0 {
		box = b
	}
	pg.x0, pg.y0, pg.wmm, pg.hmm = box[0], box[1], box[2], box[3]
	skipped := len(xc.Texts) + len(xc.Paths) + len(xc.Composites)

	for _, obj := range xc.Images {
		resID := obj.ResIDAttr
		if resID == "" {
			resID = strings.TrimSpace(obj.ResIDElem)
		}
		rp, ok := resMap[resID]
		if !ok {
			skipped++
			continue
		}
		f, ok := files[rp]
		if !ok {
			skipped++
			continue
		}
		rc, err := f.Open()
		if err != nil {
			skipped++
			continue
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil || len(data) == 0 {
			skipped++
			continue
		}
		img := ofdImage{
			data:     data,
			boundary: [4]float64{0, 0, pg.wmm, pg.hmm},
			ctm:      matIdentity,
		}
		if b := parseNums(obj.Boundary); len(b) == 4 {
			img.boundary = [4]float64{b[0], b[1], b[2], b[3]}
		}
		if c := parseNums(obj.CTM); len(c) == 6 {
			img.ctm = mat{c[0], c[1], c[2], c[3], c[4], c[5]}
		}
		pg.images = append(pg.images, img)
	}
	return pg, skipped, nil
}

// ======================= 图片嵌入信息 =======================

type imgInfo struct {
	w, h   int
	space  string // DeviceRGB / DeviceGray / DeviceCMYK
	decode string // CMYK(Adobe) 需要反相
}

func imgDimensions(b []byte) (imgInfo, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return imgInfo{}, err
	}
	info := imgInfo{w: cfg.Width, h: cfg.Height, space: "DeviceRGB"}
	switch cfg.ColorModel {
	case color.GrayModel, color.Gray16Model:
		info.space = "DeviceGray"
	case color.CMYKModel:
		info.space = "DeviceCMYK"
		info.decode = "[1 0 1 0 1 0 1 0]"
	}
	return info, nil
}

func imageDictStr(w, h int, colorspace, decode string) string {
	d := fmt.Sprintf("/Type/XObject/Subtype/Image/Width %d/Height %d/ColorSpace/%s/BitsPerComponent 8/Filter/DCTDecode",
		w, h, colorspace)
	if decode != "" {
		d += "/Decode " + decode
	}
	return d
}

func imageOpaque(m image.Image) bool {
	type opaquer interface{ Opaque() bool }
	if o, ok := m.(opaquer); ok {
		return o.Opaque()
	}
	return false // 未知类型保守按不透明处理 -> 走整页合成
}

// ======================= 慢车道: 整页合成渲染 =======================

// bilinear 双线性采样, x/y 为以 src 左上角为原点的像素坐标, 返回 16bit 通道值
func bilinear(src image.Image, x, y float64) (r, g, b, a uint32) {
	sb := src.Bounds()
	w, h := sb.Dx(), sb.Dy()
	x0 := int(math.Floor(x))
	y0 := int(math.Floor(y))
	tx := x - float64(x0)
	ty := y - float64(y0)
	if x0 < 0 {
		x0 = 0
		tx = 0
	}
	if y0 < 0 {
		y0 = 0
		ty = 0
	}
	x1, y1 := x0+1, y0+1
	if x1 >= w {
		x1 = w - 1
	}
	if y1 >= h {
		y1 = h - 1
	}
	at := func(px, py int) color.NRGBA {
		return color.NRGBAModel.Convert(src.At(sb.Min.X+px, sb.Min.Y+py)).(color.NRGBA)
	}
	c00, c10 := at(x0, y0), at(x1, y0)
	c01, c11 := at(x0, y1), at(x1, y1)
	w00 := (1 - tx) * (1 - ty)
	w10 := tx * (1 - ty)
	w01 := (1 - tx) * ty
	w11 := tx * ty
	lerp := func(a, b, c, d uint8) uint32 {
		v := w00*float64(a) + w10*float64(b) + w01*float64(c) + w11*float64(d)
		return uint32(v+0.5) * 257 // 8bit -> 16bit
	}
	return lerp(c00.R, c10.R, c01.R, c11.R),
		lerp(c00.G, c10.G, c01.G, c11.G),
		lerp(c00.B, c10.B, c01.B, c11.B),
		lerp(c00.A, c10.A, c01.A, c11.A)
}

// drawAffine 把 src 的单位正方形(OFD 图像空间, y 向下)经矩阵 m 画到 dst 像素空间
func drawAffine(dst *image.RGBA, src image.Image, m mat) {
	det := m[0]*m[3] - m[1]*m[2]
	if det == 0 {
		return
	}
	inv := mat{m[3] / det, -m[1] / det, -m[2] / det, m[0] / det, 0, 0}
	inv[4] = -(inv[0]*m[4] + inv[2]*m[5])
	inv[5] = -(inv[1]*m[4] + inv[3]*m[5])

	c0x, c0y := matApply(m, 0, 0)
	c1x, c1y := matApply(m, 1, 0)
	c2x, c2y := matApply(m, 0, 1)
	c3x, c3y := matApply(m, 1, 1)
	minX := int(math.Floor(math.Min(math.Min(c0x, c1x), math.Min(c2x, c3x))))
	maxX := int(math.Ceil(math.Max(math.Max(c0x, c1x), math.Max(c2x, c3x))))
	minY := int(math.Floor(math.Min(math.Min(c0y, c1y), math.Min(c2y, c3y))))
	maxY := int(math.Ceil(math.Max(math.Max(c0y, c1y), math.Max(c2y, c3y))))
	db := dst.Bounds()
	if minX < db.Min.X {
		minX = db.Min.X
	}
	if minY < db.Min.Y {
		minY = db.Min.Y
	}
	if maxX > db.Max.X {
		maxX = db.Max.X
	}
	if maxY > db.Max.Y {
		maxY = db.Max.Y
	}

	sw, sh := float64(src.Bounds().Dx()), float64(src.Bounds().Dy())
	for dy := minY; dy < maxY; dy++ {
		for dx := minX; dx < maxX; dx++ {
			u := inv[0]*(float64(dx)+0.5) + inv[2]*(float64(dy)+0.5) + inv[4]
			v := inv[1]*(float64(dx)+0.5) + inv[3]*(float64(dy)+0.5) + inv[5]
			if u < 0 || u >= 1 || v < 0 || v >= 1 {
				continue
			}
			r, g, b, a := bilinear(src, u*sw, v*sh)
			if a == 0 {
				continue
			}
			di := dst.PixOffset(dx, dy)
			if a >= 0xFFFF {
				dst.Pix[di+0] = uint8(r >> 8)
				dst.Pix[di+1] = uint8(g >> 8)
				dst.Pix[di+2] = uint8(b >> 8)
				dst.Pix[di+3] = 0xFF
				continue
			}
			// src-over (dst 始终不透明)
			fa := float64(a) / 65535.0
			dst.Pix[di+0] = uint8(float64(r>>8)*fa + float64(dst.Pix[di+0])*(1-fa) + 0.5)
			dst.Pix[di+1] = uint8(float64(g>>8)*fa + float64(dst.Pix[di+1])*(1-fa) + 0.5)
			dst.Pix[di+2] = uint8(float64(b>>8)*fa + float64(dst.Pix[di+2])*(1-fa) + 0.5)
			dst.Pix[di+3] = 0xFF
		}
	}
}

// renderPageCanvas 整页合成: 白底画布上按 z 序画所有图片
func renderPageCanvas(pg *ofdPageData, decoded []image.Image, dpi int) *image.RGBA {
	k := float64(dpi) / 25.4
	w := int(pg.wmm*k + 0.5)
	h := int(pg.hmm*k + 0.5)
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	canvas := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range canvas.Pix {
		canvas.Pix[i] = 0xFF
	}
	F := mat{k, 0, 0, k, 0, 0} // 毫米 -> 像素 (同为 y 向下, 无需翻转)
	for j, im := range pg.images {
		m := decoded[j]
		if m == nil {
			mm, _, err := image.Decode(bytes.NewReader(im.data))
			if err != nil {
				continue
			}
			m = mm
		}
		T := mat{1, 0, 0, 1, im.boundary[0] - pg.x0, im.boundary[1] - pg.y0}
		S := mat{im.boundary[2], 0, 0, im.boundary[3], 0, 0}
		M := matMul(matMul(matMul(F, T), S), im.ctm)
		drawAffine(canvas, m, M)
	}
	return canvas
}

// ======================= PDF 生成 (极简写入器) =======================

const ptPerMM = 72.0 / 25.4

type pdfWriter struct {
	objs [][]byte // 1 号起用
}

func (p *pdfWriter) add(body string) int {
	p.objs = append(p.objs, []byte(body))
	return len(p.objs) - 1
}

func (p *pdfWriter) addStream(dict string, data []byte) int {
	var b bytes.Buffer
	fmt.Fprintf(&b, "<<%s /Length %d>>\nstream\n", dict, len(data))
	b.Write(data)
	b.WriteString("\nendstream")
	p.objs = append(p.objs, b.Bytes())
	return len(p.objs) - 1
}

func (p *pdfWriter) set(num int, body string) {
	p.objs[num] = []byte(body)
}

func (p *pdfWriter) bytes() []byte {
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.7\n%\xE2\xE3\xCF\xD3\n")
	offsets := make([]int, len(p.objs))
	for i := 1; i < len(p.objs); i++ {
		offsets[i] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n", i)
		buf.Write(p.objs[i])
		buf.WriteString("\nendobj\n")
	}
	xref := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n", len(p.objs))
	buf.WriteString("0000000000 65535 f \n")
	for i := 1; i < len(p.objs); i++ {
		fmt.Fprintf(&buf, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&buf, "trailer\n<</Size %d/Root 1 0 R>>\nstartxref\n%d\n%%%%EOF\n", len(p.objs), xref)
	return buf.Bytes()
}

// pageToPDFMatrix 计算图片在 PDF 内容流里的 cm 矩阵。
// OFD 图像单位正方形 (0,0)=左上/y向下, PDF 图像空间 (0,1)=左上, 差一个垂直翻转。
func pageToPDFMatrix(pg *ofdPageData, im ofdImage, Hpt float64) mat {
	k := ptPerMM
	flipV := mat{1, 0, 0, -1, 0, 1} // PDF 图像空间 -> OFD 图像空间
	F := mat{k, 0, 0, -k, 0, Hpt}   // 毫米(y向下) -> pt(y向上)
	T := mat{1, 0, 0, 1, im.boundary[0] - pg.x0, im.boundary[1] - pg.y0}
	S := mat{im.boundary[2], 0, 0, im.boundary[3], 0, 0}
	return matMul(matMul(matMul(matMul(F, T), S), im.ctm), flipV)
}

func buildPDF(pages []ofdPageData, dpi, quality int) ([]byte, error) {
	pd := &pdfWriter{objs: [][]byte{nil, nil, nil}} // 1=Catalog 2=Pages 占位
	kids := make([]string, 0, len(pages))
	rq := quality
	if rq < 90 {
		rq = 90 // 重编码保底质量
	}

	for i := range pages {
		pg := &pages[i]
		Wpt := pg.wmm * ptPerMM
		Hpt := pg.hmm * ptPerMM

		// 预解码非 JPEG 图片, 判断是否需要整页合成
		decoded := make([]image.Image, len(pg.images))
		needCanvas := false
		for j, im := range pg.images {
			if im.isJPEG() {
				continue
			}
			m, _, err := image.Decode(bytes.NewReader(im.data))
			if err != nil {
				continue
			}
			decoded[j] = m
			if !imageOpaque(m) {
				needCanvas = true
			}
		}

		var content bytes.Buffer
		var xobjs []string

		if needCanvas {
			canvas := renderPageCanvas(pg, decoded, dpi)
			var jb bytes.Buffer
			if err := jpeg.Encode(&jb, canvas, &jpeg.Options{Quality: rq}); err != nil {
				return nil, err
			}
			num := pd.addStream(imageDictStr(canvas.Bounds().Dx(), canvas.Bounds().Dy(), "DeviceRGB", ""), jb.Bytes())
			xobjs = append(xobjs, fmt.Sprintf("/Im0 %d 0 R", num))
			fmt.Fprintf(&content, "q\n%.4f 0 0 %.4f 0 0 cm\n/Im0 Do\nQ\n", Wpt, Hpt)
		} else {
			for j, im := range pg.images {
				data := im.data
				if !im.isJPEG() {
					// 非 JPEG 不透明图 -> 重编码为 JPEG 再嵌入
					m := decoded[j]
					if m == nil {
						continue
					}
					var jb bytes.Buffer
					if err := jpeg.Encode(&jb, m, &jpeg.Options{Quality: rq}); err != nil {
						return nil, err
					}
					data = jb.Bytes()
				}
				info, err := imgDimensions(data)
				if err != nil {
					continue // 损坏的图片, 跳过
				}
				name := fmt.Sprintf("/Im%d", j)
				num := pd.addStream(imageDictStr(info.w, info.h, info.space, info.decode), data)
				xobjs = append(xobjs, fmt.Sprintf("%s %d 0 R", name, num))
				m := pageToPDFMatrix(pg, im, Hpt)
				fmt.Fprintf(&content, "q\n%.4f %.4f %.4f %.4f %.4f %.4f cm\n%s Do\nQ\n",
					m[0], m[1], m[2], m[3], m[4], m[5], name)
			}
		}

		contentNum := pd.addStream("", content.Bytes())
		pageNum := pd.add(fmt.Sprintf("<</Type/Page/Parent 2 0 R/MediaBox[0 0 %.4f %.4f]/Resources<</XObject<<%s>>>>/Contents %d 0 R>>",
			Wpt, Hpt, strings.Join(xobjs, " "), contentNum))
		kids = append(kids, fmt.Sprintf("%d 0 R", pageNum))
	}

	pd.set(1, "<</Type/Catalog/Pages 2 0 R>>")
	pd.set(2, fmt.Sprintf("<</Type/Pages/Kids[%s]/Count %d>>", strings.Join(kids, " "), len(kids)))
	return pd.bytes(), nil
}

// ======================= 预览验证: 提取 OFD 第一页图片 =======================

// ofdFirstPageImage 取出 OFD 第一页的第一张图片, 用于转换结果肉眼验证。
// JPEG 原样返回; 其他格式重编码为 JPEG(q90)。页内无图片(矢量型 OFD)时返回错误。
func ofdFirstPageImage(ofdPath string) ([]byte, error) {
	zr, err := zip.OpenReader(ofdPath)
	if err != nil {
		return nil, fmt.Errorf("无法打开 OFD: %v", err)
	}
	defer zr.Close()
	files := map[string]*zip.File{}
	for _, f := range zr.File {
		files[f.Name] = f
	}
	readXML := func(name string, v interface{}) error {
		f, ok := files[name]
		if !ok {
			return fmt.Errorf("包内缺少 %s", name)
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		defer rc.Close()
		data, err := io.ReadAll(rc)
		if err != nil {
			return err
		}
		return xml.Unmarshal(data, v)
	}
	var root xOfdRoot
	if err := readXML("OFD.xml", &root); err != nil || len(root.DocRoots) == 0 {
		return nil, fmt.Errorf("OFD.xml 缺少 DocRoot")
	}
	docPath := cleanZipPath(root.DocRoots[0])
	docDir := path.Dir(docPath)
	if docDir == "." {
		docDir = ""
	}
	var doc xDocument
	if err := readXML(docPath, &doc); err != nil || len(doc.Pages) == 0 {
		return nil, fmt.Errorf("文档没有页面")
	}
	box := parseNums(doc.PhysicalBox)
	if len(box) != 4 {
		box = []float64{0, 0, 210, 297}
	}
	resMap := map[string]string{}
	resPaths := doc.DocRes
	if len(resPaths) == 0 {
		resPaths = []string{"DocumentRes.xml"}
	}
	for _, rp := range resPaths {
		rp = cleanZipPath(path.Join(docDir, rp))
		if _, ok := files[rp]; !ok {
			continue
		}
		var rs xRes
		if err := readXML(rp, &rs); err != nil {
			continue
		}
		base := path.Dir(rp)
		if rs.BaseLoc != "" {
			base = path.Join(base, cleanZipPath(rs.BaseLoc))
		}
		for _, m := range rs.Media {
			resMap[m.ID] = cleanZipPath(path.Join(base, m.File))
		}
	}
	pg, _, err := readOFDPage(files, readXML, docDir, doc.Pages[0].BaseLoc, box, resMap)
	if err != nil {
		return nil, err
	}
	if len(pg.images) == 0 {
		return nil, fmt.Errorf("第一页没有图片(矢量/文字型 OFD 无法预览)")
	}
	data := pg.images[0].data
	if len(data) >= 2 && data[0] == 0xFF && data[1] == 0xD8 {
		return data, nil
	}
	m, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	var jb bytes.Buffer
	if err := jpeg.Encode(&jb, m, &jpeg.Options{Quality: 90}); err != nil {
		return nil, err
	}
	return jb.Bytes(), nil
}

// ======================= 主入口: OFD -> PDF =======================

func convertOFD2PDF(ofdPath, pdfPath string, dpi, quality int) (pages, skipped int, sizeMB float64, err error) {
	zr, err := zip.OpenReader(ofdPath)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("无法打开（不是有效的 OFD/zip）: %v", err)
	}
	defer zr.Close()

	files := map[string]*zip.File{}
	for _, f := range zr.File {
		files[f.Name] = f
	}
	readXML := func(name string, v interface{}) error {
		f, ok := files[name]
		if !ok {
			return fmt.Errorf("包内缺少 %s", name)
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		defer rc.Close()
		data, err := io.ReadAll(rc)
		if err != nil {
			return err
		}
		return xml.Unmarshal(data, v)
	}

	var root xOfdRoot
	if err := readXML("OFD.xml", &root); err != nil {
		return 0, 0, 0, err
	}
	if len(root.DocRoots) == 0 {
		return 0, 0, 0, fmt.Errorf("OFD.xml 中没有 DocRoot")
	}

	var allPages []ofdPageData
	totalSkipped := 0
	for _, dr := range root.DocRoots {
		docPath := cleanZipPath(dr)
		docDir := path.Dir(docPath)
		if docDir == "." {
			docDir = ""
		}
		var doc xDocument
		if err := readXML(docPath, &doc); err != nil {
			return 0, 0, 0, fmt.Errorf("%s: %v", docPath, err)
		}
		box := parseNums(doc.PhysicalBox)
		if len(box) != 4 || box[2] <= 0 || box[3] <= 0 {
			box = []float64{0, 0, 210, 297} // 兜底 A4
		}

		// 资源索引: DocumentRes.xml (可能多个, 缺省尝试同名文件)
		resMap := map[string]string{}
		resPaths := doc.DocRes
		if len(resPaths) == 0 {
			resPaths = []string{"DocumentRes.xml"}
		}
		for _, rp := range resPaths {
			rp = cleanZipPath(path.Join(docDir, rp))
			if _, ok := files[rp]; !ok {
				continue
			}
			var rs xRes
			if err := readXML(rp, &rs); err != nil {
				continue
			}
			base := path.Dir(rp)
			if rs.BaseLoc != "" {
				base = path.Join(base, cleanZipPath(rs.BaseLoc))
			}
			for _, m := range rs.Media {
				resMap[m.ID] = cleanZipPath(path.Join(base, m.File))
			}
		}

		for _, p := range doc.Pages {
			pg, sk, err := readOFDPage(files, readXML, docDir, p.BaseLoc, box, resMap)
			if err != nil {
				return 0, 0, 0, err
			}
			totalSkipped += sk
			allPages = append(allPages, pg)
		}
	}
	if len(allPages) == 0 {
		return 0, 0, 0, fmt.Errorf("文档没有页面")
	}

	pdfBytes, err := buildPDF(allPages, dpi, quality)
	if err != nil {
		return 0, 0, 0, err
	}
	if err := os.WriteFile(pdfPath, pdfBytes, 0o644); err != nil {
		return 0, 0, 0, err
	}
	st, _ := os.Stat(pdfPath)
	return len(allPages), totalSkipped, float64(st.Size()) / 1024 / 1024, nil
}

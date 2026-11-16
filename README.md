# pdf-to-ofd

PDF 与**图片型 OFD**（GB/T 33190，国产版式文档格式）**双向转换**——正向每页渲染成图片嵌入 OFD，反向提取图片按版式摆回 PDF，不做 OCR、不编辑内容。

单文件可执行，支持 Windows / Linux / macOS。

[English](#english) · [中文](#中文)

---

## 中文

### 特性

- **双向转换**：`xx.pdf → xx.ofd`，`xx.ofd → xx.pdf`，按扩展名自动识别方向
- **图片型转换**：每页 PDF 渲染为 JPEG 嵌入 OFD，不 OCR、不改内容
- **批量**：扫描整个目录（可选递归子目录），每个 PDF/OFD 生成同名对应文件
- **单文件**：Go 编译出独立可执行文件，无需安装运行时
- **跨平台**：Windows / Linux / macOS（x64 和 ARM64）
- **快**：和 pdf2word 同架构，113 页扫描件约 12 秒转完
- **OFD→PDF 零依赖**：反向转换是纯 Go 实现，不需要 PDFium 引擎库
- **预览验证**：转换后生成预览图（OFD 第一页），打开即可肉眼验证结果：PDF→OFD 生成 `<同名>.preview-ofd.jpg`，OFD→PDF 生成 `<同名>.preview-pdf.jpg`（`-preview=0` 或 GUI 取消勾选可关闭）
- **白底渲染**：透明背景的 PDF（如电子发票）渲染时预填白底，不会出现黑底/空白页

### OFD → PDF 说明

- 仅还原**图片型内容**（ImageObject）；文字（TextObject）、矢量图形（PathObject）会跳过并在输出中提示数量。电子发票等矢量型 OFD 不适用
- JPEG 图片**原始字节直通**嵌入 PDF（DCTDecode），零重压缩，往返无损
- 含透明通道的 PNG 等图片按 `-dpi` 整页合成渲染（仿射变换 + 双线性采样 + alpha 混合），印章叠加效果正确
- 支持多文档（多个 DocRoot）、每页独立尺寸（Area/PhysicalBox）、CTM 仿射变换
- 往返说明：PDF → OFD 本身就是"渲染成图片"，再转回 PDF 后是图片型 PDF，文字不可再编辑

> 同名 PDF/OFD 同时存在时，请选择独立输出目录。程序禁止覆盖本批输入文件，包括勾选覆盖或使用 `-f` 时。转换先写临时文件，成功后才提交输出；递归输出保留子目录结构。

### Windows 图形界面

本地修改版：双击 `pdf-to-ofd.exe` 打开窗口，无需安装 Go。将 `pdfium.dll` 放在程序同目录供 PDF→OFD 使用；仅 OFD→PDF 不需要它。

1. 点击“浏览...”选择输入文件夹，也可将文件夹拖入窗口。
2. 选择输出目录（建议单独指定）；设置 DPI、JPEG 质量和递归选项。
3. 点击“开始转换”，在窗口中查看日志和完成提示。

转换时禁止重复启动，完成后可以再次转换。初始化出错会弹窗并写入用户缓存目录的 `pdf-to-ofd-startup.log`。GUI 仅支持 Windows；其他平台使用命令行。

### 下载

到 [Releases](../../releases) 下载对应平台的压缩包，每个包含主程序 + PDFium 引擎库 + 说明文档。**两者放同一目录。**

| 文件 | 适用系统 |
|---|---|
| `pdf-to-ofd_windows_amd64.zip` | Windows 64位 |
| `pdf-to-ofd_linux_amd64.tar.gz` | Linux 64位 |
| `pdf-to-ofd_macos_amd64.tar.gz` | macOS Intel |
| `pdf-to-ofd_macos_arm64.tar.gz` | macOS Apple Silicon |

### 用法

```
pdf-to-ofd                Windows 打开 GUI；其他平台转换当前目录
pdf-to-ofd <目录>         转换指定目录（可把文件夹拖到程序上）
pdf-to-ofd -r <目录>      递归子目录
pdf-to-ofd -o 输出目录 <目录>   指定输出位置
pdf-to-ofd -dpi 200 <目录>     渲染清晰度（默认 150）
pdf-to-ofd -q 90 <目录>        JPEG 质量 1-100（默认 85）
pdf-to-ofd -f <目录>           覆盖已存在的输出文件
```

### 输出

`xx.pdf → xx.ofd`、`xx.ofd → xx.pdf`，默认与源文件同目录。已存在的输出默认跳过（不覆盖，加 `-f` 强制覆盖）。

### OFD 结构

生成的 OFD 是符合 GB/T 33190 的最小合法包：

```
OFD.xml                    主入口（文档类型、DocRoot）
Doc_0/Document.xml         文档定义（页尺寸、页列表）
Doc_0/DocumentRes.xml      资源索引
Doc_0/Pages/Page_N/Content.xml   每页内容（引用图片）
Doc_0/Res/image_N.jpeg     图片资源
```

### 从源码构建

当前修改版需要 Go 1.26+。PDF→OFD 运行时需要对应平台的 [PDFium 预编译库](https://github.com/bblanchon/pdfium-binaries)。Windows x64 在本目录运行 `powershell -ExecutionPolicy Bypass -File .\build-windows.ps1`，会嵌入 GUI 必需的 Common Controls v6 清单并生成无控制台窗口的 EXE。以下命令用于其他平台；远程原仓库不包含本地 GUI 修改：

```bash
git clone https://github.com/huanxiang666/pdf-to-ofd.git
cd pdf-to-ofd
# 放入 PDFium 库（与可执行文件同目录）：
#   Windows: pdfium.dll    Linux: libpdfium.so    macOS: libpdfium.dylib
go build -ldflags="-s -w" -o pdf-to-ofd .
```

### 协议

[MIT](LICENSE)。PDFium 引擎遵循 [Apache-2.0](https://pdfium.googlesource.com/pdfium/+/refs/heads/main/LICENSE)。

---

## English

**Bidirectional** converter between PDF and **image-based OFD** (GB/T 33190, China's national format) — PDF pages are rendered to images; OFD images are placed back into PDF pages. No OCR, no editing.

Single-file binary, cross-platform (Windows / Linux / macOS).

Direction is auto-detected by extension: `xx.pdf → xx.ofd`, `xx.ofd → xx.pdf`.

OFD→PDF notes: image objects only (text/vector objects are skipped with a notice); JPEG bytes are embedded verbatim (lossless round-trip); transparent PNGs are page-composited at `-dpi`; the PDFium library is NOT required for this direction.

### Usage

```
pdf-to-ofd                convert all PDFs and OFDs in current directory
pdf-to-ofd <folder>       convert a specific folder (drag-and-drop supported)
pdf-to-ofd -r <folder>    recurse into subdirectories
pdf-to-ofd -dpi 200 <folder>    render DPI (default 150)
```

### Build

Requires Go 1.26+. On Windows x64, run `powershell -ExecutionPolicy Bypass -File .\build-windows.ps1` to embed the required manifest and build the GUI. Double-click opens the GUI; arguments select CLI mode. PDF→OFD requires the [PDFium prebuilt library](https://github.com/bblanchon/pdfium-binaries).

```bash
git clone https://github.com/huanxiang666/pdf-to-ofd.git
cd pdf-to-ofd
go build -ldflags="-s -w" -o pdf-to-ofd .
```

### License

[MIT](LICENSE). PDFium is [Apache-2.0](https://pdfium.googlesource.com/pdfium/+/refs/heads/main/LICENSE).

# pdf-to-ofd

PDF 转**图片型 OFD**（GB/T 33190，国产版式文档格式）——每一页渲染成图片嵌入 OFD，不做 OCR、不编辑内容。

单文件可执行，支持 Windows / Linux / macOS。

[English](#english) · [中文](#中文)

---

## 中文

### 特性

- **图片型转换**：每页 PDF 渲染为 JPEG 嵌入 OFD，不 OCR、不改内容
- **批量**：扫描整个目录（可选递归子目录），每个 PDF 生成同名 OFD
- **单文件**：Go 编译出独立可执行文件，无需安装运行时
- **跨平台**：Windows / Linux / macOS（x64 和 ARM64）
- **快**：和 pdf2word 同架构，113 页扫描件约 12 秒转完

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
pdf-to-ofd                转换当前目录所有 PDF
pdf-to-ofd <目录>         转换指定目录（可把文件夹拖到程序上）
pdf-to-ofd -r <目录>      递归子目录
pdf-to-ofd -o 输出目录 <目录>   指定输出位置
pdf-to-ofd -dpi 200 <目录>     渲染清晰度（默认 150）
pdf-to-ofd -q 90 <目录>        JPEG 质量 1-100（默认 85）
pdf-to-ofd -f <目录>           覆盖已存在的 OFD
```

### 输出

每个 `xx.pdf` 在同目录生成 `xx.ofd`。已存在的 OFD 默认跳过（不覆盖，加 `-f` 强制覆盖）。

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

需要 Go 1.20+ 和对应平台的 [PDFium 预编译库](https://github.com/bblanchon/pdfium-binaries)：

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

Convert PDFs to **image-based OFD** (GB/T 33190, China's national format) — every page rendered as an image, no OCR, no editing.

Single-file binary, cross-platform (Windows / Linux / macOS).

### Usage

```
pdf-to-ofd                convert all PDFs in current directory
pdf-to-ofd <folder>       convert a specific folder (drag-and-drop supported)
pdf-to-ofd -r <folder>    recurse into subdirectories
pdf-to-ofd -dpi 200 <folder>    render DPI (default 150)
```

### Build

Requires Go 1.20+ and the [PDFium prebuilt library](https://github.com/bblanchon/pdfium-binaries):

```bash
git clone https://github.com/huanxiang666/pdf-to-ofd.git
cd pdf-to-ofd
go build -ldflags="-s -w" -o pdf-to-ofd .
```

### License

[MIT](LICENSE). PDFium is [Apache-2.0](https://pdfium.googlesource.com/pdfium/+/refs/heads/main/LICENSE).

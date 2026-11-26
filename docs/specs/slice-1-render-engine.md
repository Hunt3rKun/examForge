# MVP 切片 1：确定性渲染内核（冻结技术规格）

- **状态：** 冻结设计规格；不是现状文档，也不表示功能已经实现。
- **范围：** Go 内联分词、RenderPayload 预编译、本地资产解析、Typst 渲染器、模板布局、CLI 与验证门禁的实现约定。
- **依据：** 当前 Exam IR v1 与 Layout Spec v1 schema、Stage 0 Go DTO/验证器及黄金样例。
- **实现边界：** 切片 1 编码阶段可以依本规格新增/修改实现代码、测试、模板、CLI、门禁及同步现状文档；本规格冻结阶段不修改这些内容。

## 1. 目标

将当前最小 Typst smoke template 升级为确定、可测试、资源有界的渲染流水线：

```text
ExamDocumentIR + LayoutSpec
        │ Validate
        ▼
 Go Precompiler ── Inline Tokenizer
        │            AssetResolver
        │            layout hints / option grid
        ▼
 RenderPayload (versioned JSON) + isolated workspace
        │
        ▼
 TypstRenderer ── bounded subprocess ── templates/exam.typ
        │
        ▼
 Artifact { bytes, MIME, warnings }
```

IR 是内容，Layout Spec 是版式，两者始终是独立输入。Typst 接收已预处理的 RenderPayload，只负责声明式排版；不得在模板内解析 Markdown/LaTeX/占位符、查找原始资产 URI，或承载复杂正则逻辑。

## 2. Explicit Non-Goals

切片 1 不实现：HTTP/API 服务、DB 或 River 队列、S3/SeaweedFS 网络下载、Word/DOCX 导出、LaTeX/PDF 以外的渲染后端实现、VLM/OCR/源文档解析、持久化模型、通用完整 Markdown、任意用户 Typst 代码执行、远程包运行时拉取。接口允许后续接入资产后端和其他 Renderer，但本切片只交付本地资产解析与 Typst PDF。

## 3. 现有输入约束与兼容原则

- `schemas/exam-ir-v1.json` 是 Exam IR 数据形状的权威来源；包含题干、选项内容、答案短值与 detail、解析、递归子题、text block Markdown、math_block、image_ref、table cell 文本与 colspan/rowspan。
- `schemas/layout-spec-v1.json` 是布局输入的权威来源；已有 `paper_size`（a4/a3/b4）、`columns`（1/2）、`mode`（student/teacher）、`show_answer`、`show_analysis`、`show_binding_line`、`option_layout`（auto/1x4/2x2/4x1）和可选 header/footer。
- 不为渲染 payload 把字段塞入 Exam IR 或 Layout Spec v1。Payload 是独立、内部使用的版本化 DTO；若后续确需更改交换 schema，须先变更 schema，再变更 Go DTO 与 fixture。
- 保留 schema 验证与渲染器边界：调用方先验证文件/值，再调用 `Renderer.Render(ctx, exam, layout)`；渲染器也应拒绝无法安全处理的运行时输入。

## 4. Go 内联分词器（`internal/ir/inline`）

### 4.1 API 与 Token

建议公开 API：

```go
package inline

type Token struct {
    Type  string `json:"type"`
    Text  string `json:"text,omitempty"`
    Index int    `json:"index,omitempty"` // blank 序号
}

type Diagnostic struct {
    Code       string `json:"code"`
    Message    string `json:"message"`
    StartByte  int    `json:"start_byte"` // 0-based，UTF-8 byte offset
    EndByte    int    `json:"end_byte"`   // exclusive
}

type Result struct {
    Tokens      []Token      `json:"tokens"`
    Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
}

func Parse(source string) Result
```

Token `Type` 固定为 `text`、`math_inline`、`bold`、`italic`、`underline`、`blank`。`Text` 对应文字/公式/样式内容；`blank.Index` 保存 `{{blank:N}}` 中的正整数 N，绝不丢弃原序号。`Parse` 对任何 UTF-8 字符串都不 panic，不返回因语法不完整导致的致命错误；畸形标记保留可见原文并给出诊断。实现应在入口验证 UTF-8；非法字节按替换字符 `�` 转为文本并附 `invalid_utf8` 诊断，以保证 JSON 可编码。诊断偏移定义为输入原始字节位置，非 rune 索引。

所有连续普通字符合并为单个 `text` token；输出不得产生空 `text` token。相邻且相同样式的有效标记可保留为相邻 token，不跨越任何 token 合并；该行为须由测试固定。

### 4.2 语法、优先级与降级

支持的语法是有限子集：

| 语法 | Token | 示例 |
|---|---|---|
| `$...$` | `math_inline` | `$a_n = 1$` |
| `**...**` | `bold` | `**重点**` |
| `*...*` | `italic` | `*强调*` |
| `__...__` | `underline` | `__下划线__` |
| `{{blank:N}}`，N 为正整数 | `blank` | `{{blank:1}}` |

扫描器按 UTF-8 rune 边界前进、以字节偏移记录诊断。不是通用 Markdown 实现。开始标记候选按以下顺序识别：反斜杠转义、完整 blank、`**`、`__`、`$`、`*`，其余按普通文本累积。强标记优先于单字符标记，确保 `**` 不被识别为两个 `*`。

- `\$`、`\*`、`\_`、`\\` 是转义：去掉一个转义反斜杠，转义后的字符作为纯文本，不开启标记。反斜杠后若不是上述字符，反斜杠本身保持为文本。
- blank 只在完整匹配 `{{blank:` + 正整数十进制 + `}}` 时识别；0、负数、非数字、溢出或未闭合均原样输出为文本并产生 `invalid_blank` 诊断。
- 未闭合 `$`、`**`、`*`、`__` 不吞掉后续文本：从该候选开始至扫描恢复点均按普通文本保留，并产生对应 `unclosed_*` 诊断。解析必须继续处理其后独立、可识别的标记，不得 panic。
- 匹配采用最早未转义同类闭合定界符；禁止跨段回溯式猜测或通过复杂正则解析。为了避免误吞空内容，零长度样式/公式不产生样式 token，整个定界内容按原文 text 输出并给 `empty_span` 诊断。
- 支持样式中嵌套 inline math，且规则固定：例如 `**已知 $a_n = 1$**` 解析为 `bold(Text:"已知 ")`、`math_inline(Text:"a_n = 1")`、`bold(Text:"")`，其语义是公式仍按公式渲染，公式前后文字使用粗体。若闭合粗体内部包含更多文字，则公式两侧分别输出带 `bold` 类型 token（例如 `**x $y$ z**` → bold("x "), math_inline("y"), bold(" z")）。这条规则允许且只允许样式范围内的 `$...$` 中断当前样式 token；不建立可任意递归的样式树。
- 样式内的样式嵌套（例如 `**a *b* c**`）、不同样式相互嵌套、公式内再解析格式标记均不支持：内层非数学标记按字面文本保留；只有公式分隔符被公式解析器消费。样式闭合以对应的最早同类未转义闭合符为准。
- 公式中反斜杠用于 LaTeX 命令，不作为外层转义消除；公式内遇 `$` 以外字符不应用 Markdown 解析。

Tokenizer 只处理字符串，不更改 IR。预编译器必须将 Tokenizer 应用于所有下列文本字段：`text` block 的 markdown、option label 与 content 中的 text、answer.short_values、answer.detail 的 text block、analysis 的 text block、所有递归 question，以及 table headers/rows 每个 cell 的 text。`math_block.latex` 直接作为展示公式载荷，不进行 inline Markdown 分词。图片 caption/alt 除非未来规格扩展，不在本切片作为题面文本渲染。

## 5. RenderPayload 契约

### 5.1 数据模型

`internal/render` 持有独立 DTO，至少包含：

```text
RenderPayload {
  payload_version: "1"
  exam: RenderExam
  layout: RenderLayout
  assets: map<asset_id, ResolvedAsset>
  warnings: []RenderWarning
}

RenderInline { tokens: []inline.Token }
ResolvedAsset {
  asset_id: string
  path: string       // workspace 根目录相对路径，POSIX 分隔符
  media_type: string
  sha256: string
  width_mm?: number  // 基于 IR suggested_width_mm 与本模板安全上限预编译
}
RenderLayout {
  paper_size: "a4"|"a3"|"b4"
  columns: 1|2
  mode: "student"|"teacher"
  show_answer: bool
  show_analysis: bool
  show_binding_line: bool
  option_layout: "auto"|"1x4"|"2x2"|"4x1"
  header_text?: RenderInline
  footer_text?: RenderInline
  ...预计算的页面尺寸、网格和版面常量...
}
```

实际 JSON 使用稳定字段名、UTF-8 和确定顺序（Go struct 字段顺序；map 键按 `asset_id` 排序后构造或改用有序 slice）。不序列化绝对路径、宿主机目录、临时目录名或用户提供的未经校验 URI。Payload 中题干及其他块使用显式 block kind 和 Token 列表；表格 cell 的 `colspan`/`rowspan` 已由验证器确认是正整数，布局器补足缺省为 1。未知 IR block kind 保留 kind 与原始字段可支持范围内的信息，并通过 warning 显示占位提示，不可静默丢弃。

预编译阶段一次性完成：所有 inline 文本分词；图片 asset ID 解析、内容复制与哈希；布局常量归一化；选项统一网格决策；验证显示/留白语义。Typst 不再读取原始 IR JSON 和原始 Layout JSON，也不做 asset ID 查找。

### 5.2 警告和错误

非致命 inline 降级、未知 block 等写入 `RenderWarning`（带 code、路径上下文、可读描述）并返回 Artifact。缺失资产、路径不安全、无法读取/复制资产、无效 payload、Typst 非零退出或超时则渲染失败并返回 contextual error；错误不得吞掉 stderr。安全性失败不可降级成可访问的任意路径。

## 6. 资产解析与隔离（`internal/render`）

接口应允许纯本地实现和未来对象存储适配，不让渲染器知道 assets 根目录：

```go
type AssetResolver interface {
    Resolve(ctx context.Context, assetID string, asset ir.Asset) (io.ReadCloser, error)
}

type Renderer interface {
    Render(ctx context.Context, exam ir.ExamDocumentIR, layout ir.LayoutSpec) (*Artifact, error)
}

type Artifact struct {
    Bytes    []byte
    MIMEType string
    Warnings []RenderWarning
}
```

本地目录解析器首版从调用方指定根目录读取（CLI 默认/测试使用 `testdata/golden/assets`）；接口以 asset ID 和 IR 元数据定位，不允许将任意 `uri` 直接作为文件系统路径。映射规则须有明确配置/fixture 约定（例如 `asset_id` 对应受控文件清单），不可依赖隐式拼接任意 URI。

**路径穿越防护要求：** 在解析器入口验证 asset ID 完整匹配 `^ast_[A-Za-z0-9_-]+$`，拒绝空值、分隔符、`.`/`..` 路径段、绝对路径、NUL、反斜杠编码/分隔符和任何不符合 schema 的字符。文件打开后用 `filepath.Abs`、`filepath.Rel` 与 `filepath.Clean` 验证最终候选严格位于配置根目录内；拒绝根外路径及指向根外的符号链接。不得仅靠字符串前缀判断包含关系。复制前后都保持该约束。恶意或缺失路径返回错误，不返回原始路径给 Typst。

workspace 组装时，以 asset ID 为去重键；同一 `asset_id` 多次引用只解析/读取一次、只复制一份，多个 payload 引用共享同一相对路径。不同 ID 即使内容相同仍分别记录；内容哈希只用于完整性校验，不擅自合并语义不同资产。每个复制结果记录 SHA-256 与 MIME，拷贝应 context-aware、有限大小（实现配置的明确上限），并通过安全生成的文件名写入 workspace 内的 `assets/`。资产解析/复制与 Typst 的并发资源均须有上界。

## 7. Renderer 与 Typst 子进程

### 7.1 公共 Renderer

所有实现使用统一双输入接口：

```go
Render(ctx context.Context, exam ir.ExamDocumentIR, layout ir.LayoutSpec) (*Artifact, error)
```

返回的 Artifact 包含 PDF 二进制、`application/pdf` MIME 和 warnings。Typst、未来 Docx/LaTeX 实现均实现此接口，上层 CLI/服务不针对后端改变 API。渲染期间不修改调用方传入的 exam/layout。

### 7.2 TypstRenderer 并发、deadline 与错误

- `TypstRenderer` 接受 Typst 可执行文件路径、最大并发、超时时长、临时根配置、模板路径/嵌入资源及注入的 command factory 等测试依赖。最大并发默认 `runtime.NumCPU()`；显式配置必须为正整数。
- 使用有缓冲 Channel 作为信号量；进入渲染先以 `select` 等待获得 token 或 `ctx.Done()`。等待阶段取消必须立即返回，不得启动子进程。完成或失败以 `defer` 释放 token。
- 调用方已有 deadline 时遵从该 deadline；无 deadline 时为本次编译添加默认 30s timeout（配置可覆盖）。使用 `exec.CommandContext` 启动，不经 shell。取消/超时必须终止并 Wait 回收进程；错误明确区分 context cancel/deadline 与编译失败。
- 捕获 stderr（有界缓存，防无限增长），非零退出时在 contextual error 中包含可读 stderr；不得将敏感宿主机路径泄露到用户错误。
- 每次 render 创建独立临时目录，使用 `--root <workspace>`。将 `payload.json`、模板、MiTeX 包缓存、`assets/` 与输出 PDF 都置于该 root 之内；Typst 所需路径只允许 workspace 相对路径。调用 `defer os.RemoveAll(workspace)`，成功、失败、取消均清理；返回 PDF bytes 后不可让 Artifact 依赖临时路径。
- 如果复制模板或包缓存资源，确保每次 workspace 只读输入，避免并发渲染共享可变文件。进程的 stdin/stdout/stderr 和产物读取应有大小限制与 contextual error。

### 7.3 可注入进程边界

为可靠测试并避免真实 Typst 依赖，封装 command factory/runner，测试可注入假程序或慢命令。注入点必须仍走同一信号量、context、stderr、Wait、workspace 清理逻辑；生产实现必须使用 `exec.CommandContext`。测试程序由测试代码自身生成在独立临时目录，不从不可信 fixtures 执行脚本。

## 8. MiTeX 固定版本与离线策略（强制）

1. 切片 1 编码阶段**第一个前置验证动作**：获取并固定一个明确的 `@preview/mitex` 版本，确认其包内容（含 `.wasm`）能在本机 Typst **0.15.1** 下完成最小 inline 与 display 公式编译。记录版本、来源、许可证、包完整性校验值及实测命令/结果；若版本不兼容，先解决兼容版本/适配，不进入其他实现步骤。
2. 将固定版本的完整 MiTeX 包及其 `.wasm` 文件 vendoring 至仓库 `third_party/typst-packages/` 的 Typst 包缓存约定目录（保持 `@preview/mitex/<version>` 可按 Typst 包规范解析）。校验值固定在代码/文档的可审计位置。遵守并附带上游许可要求。
3. Renderer 每次构建 workspace 时，将只读 vendored package cache 放入 workspace 中预期的 cache 目录，并通过 Typst `--package-cache-path <workspace 内路径>` 显式指定；命令不依赖 `$HOME/.cache/typst`、系统包缓存或在线服务。模板固定 import 已 vendored 的准确包版本。
4. `./scripts/verify.sh` 的未来实现必须在断网条件仍可运行 MiTeX 编译。禁止 verify 或普通渲染时隐式在线 fetch；缺包、哈希不符、版本不匹配须立即给出明确失败信息，不允许静默回退为错误公式文本。
5. 当前已确认 Typst 0.15.1 可执行文件存在、系统 Typst cache 目录无可见包内容；本规格冻结时**没有**尝试下载、兼容性编译或断网渲染。这是编码阶段的明确阻塞前置验证，不是已经通过的兼容性结论。

## 9. Typst 模板与排版规格（`templates/exam.typ`）

### 9.1 输入与模板职责

模板从 workspace 内 `payload.json` 读取 RenderPayload。模板不解析 `$...$`、Markdown 或 blank 字符串、不读原始 IR/Layout JSON、不推导 asset ID 路径、不通过 regex 处理文本。它按 Token 的 Type 做声明式映射：text 输出文本，bold/italic/underline 应用相应样式，math_inline 调用 MiTeX `mi`，blank 绘制有序答题横线/空格并保留序号语义。`math_block.latex` 调用 MiTeX `mitex` 展示公式。公式输入的 Typst/MiTeX 转换边界由兼容性前置验证固定。

### 9.2 页面、卷面要素与模式真值表

- `paper_size` 直接映射 A4/A3/B4 页面；统一页面边距与栏间距写入 RenderLayout 常量，不允许以 CLI flags 代替 Layout Spec。
- `columns` 设置单栏或双栏，影响正文题目流；密封/装订线始终在页面左边缘保留版心外的预留空间，不参与题目栏流。
- `show_binding_line=true` 在页面左侧显示连续装订/密封线及必要的考生信息线；false 时既不绘线也不额外预留装订专属宽度。A3/B4 与双栏均保持左侧位置稳定。
- `header_text`、`footer_text` 作为预分词后的 inline 内容放置在页眉/页脚，分页时按 Typst page header/footer 语义重复，不因内容变化而改变题目流顺序。
- `float_right` 图片采用确定宽度上限、比例约束与局部环绕：仅影响当前/相邻段落的可用行宽，不跨题/跨表格浮动；无法安全浮动的上下文退化为右对齐块图并添加 warning。资产实际尺寸/IR suggested_width_mm 先在 Go 限幅，Typst 仅使用 payload 已给宽度。
- 表格列数按该表最大有效网格列数计算（cell colspan 累加并考虑 rowspan 占格）；跨行/列单元格传给 Typst table cell 的 `rowspan`/`colspan`。不规则空格补为显式空 cell；重叠、不可能满足的网格返回预编译错误，不默默挤压或截断。表格 cell 文本使用 Token 渲染。
- `answer_space_mm` 以毫米为单位：student 模式按字段值保留答题空间；teacher 模式默认紧凑、不保留该空间。若 teacher 答题定位需要留白，必须由未来独立 Layout Spec 字段表达，不可默默复用该值。

| `mode` | `show_answer` | `show_analysis` | 答案 | 解析 | `answer_space_mm` |
|---|---:|---:|---|---|---|
| student | false | false | 隐藏 | 隐藏 | 按题目字段留白 |
| student | false | true | 隐藏 | 显示 | 按题目字段留白 |
| student | true | false | 显示 | 隐藏 | 不再为空答案区额外留白；若有答案内容则按内容排版 |
| student | true | true | 显示 | 显示 | 不再为空答案区额外留白；若有答案内容则按内容排版 |
| teacher | false | false | 隐藏 | 隐藏 | 紧凑，不留答题空间 |
| teacher | false | true | 隐藏 | 显示 | 紧凑，不留答题空间 |
| teacher | true | false | 显示 | 隐藏 | 紧凑，不留答题空间 |
| teacher | true | true | 显示 | 显示 | 紧凑，不留答题空间 |

`mode` 仅控制卷面定位/留白行为；`show_answer` 与 `show_analysis` 是互相独立、无隐式强制的显隐开关。上述所有组合都有效，任何开关组合不得被模板静默忽略。学生模式若开启答案显示，照样显示答案，但不因原答题区另留重复空间。

### 9.3 选项布局与 auto 算法

`option_layout` 明确表示列数×行数：`1x4` 为一行四个等宽格，`2x2` 为两列两行，`4x1` 为四列一行（每项独占整行；该字符串按现有 schema enum 保留，语义以“一项一行”为准，网格文档应避免把首数误解为行数）。非 `auto` 值严格遵循指定网格，不由模板重新调整；不足项保留空格，超出该网格项数时以同一网格继续追加完整行。

`auto` 不使用 Go 的字符宽度启发式，也不采用 greedy row packing。Go 预编译/Typst 单次测量所有 option 的**完整内容**（文本、MiTeX 公式和图片均按最终可排版内容测量），得到最大项宽 `Wmax` 与可用选项栏宽 `Wavail`，随后所有选项统一采用同一阶梯：

1. 若 `4 × Wmax + gutters ≤ Wavail`，选 `1x4`（4 等分对齐）；
2. 否则若 `2 × Wmax + gutter ≤ Wavail`，选 `2x2`（2 等分对齐）；
3. 否则选 `4x1`（每项独占一行）。

`measure()` 只对每项内容测一次，不通过试排—回滚循环改变布局。测量结果先由 Typst 返回/注入 payload（按实现选择单次预编译测量，但同一判定必须可测试、稳定）；measure 无法处理的资源或非确定内容按最大宽度无界处理，确定性回退 `4x1`。每个布局都按固定列宽网格对齐，禁止出现首行 3 项、次行 1 项等不对称 greedy packing。复杂内容自然换行/溢长按同一固定格宽处理；禁止无限增宽或不终止的 reflow。

切片实现须先验证 Typst 0.15.1 对包含 MiTeX/图片内容的 `measure()` API 能否稳定取得所需宽度。若 Typst 宏层无法直接序列化测量值到 RenderPayload，允许在同次渲染执行单趟确定性测量并立即选档，但不得用字符数替代视觉测量或做多次排版反馈循环；相关边界用公式、图片和超长选项 fixture 物理证伪。

### 9.4 `answer_space_mm` 与输出稳定性

答题留白使用确定的毫米长度，负值由 schema 拒绝，零值不生成空间；student 下按题目位置呈现，teacher 紧凑。日期/随机数/临时路径不进入文档文本，避免同一输入产生无关差异。PDF 字节不要求跨 Typst 版本完全相同，但页面结构、模式行为、资源和选项网格必须由测试可断言。

## 10. 最小 CLI 与门禁升级计划（设计，不在本 Spec 冻结轮实施）

新增 `cmd/examrender/main.go`：

```text
examrender -ir <exam-ir.json> -layout <layout-spec.json> -assets <asset-root> -out <output.pdf>
```

CLI 读取并运行 `ir.Validate` 对应的 Exam IR 与 Layout Spec 校验（文件校验或等价内存校验），构造本地 `AssetResolver` 与 `TypstRenderer`，执行 `Render(ctx, exam, layout)`，将 Artifact bytes 原子写出到 `-out`，错误打印到 stderr 并以非零退出。`-assets` 是资产解析根目录，不是布局策略；CLI 不增加覆盖 Layout Spec 的零散版式 flags。输入/输出不可相互覆盖，输出父目录按明确策略处理，不留下临时文件。

后续 `scripts/verify.sh` 将以 `go run ./cmd/examrender ...` 对 golden IR/layout/assets 分别运行代表性 student/teacher 渲染，验证 PDF 非空及 `%PDF-` 文件头，运行 `go vet`、race 测试、schema fixture 检查；临时输出目录须用 trap/cleanup 清理。MiTeX package cache 必须来自 vendored 文件，不访问网络。切片 1 实现完成时同步更新 `docs/subsystems/exam-ir.md`、`docs/subsystems/render-engine.md` 与其实际路径，但不混入本 proposal 文件的变更范围。

## 11. 文件变更清单与实现顺序

### 本 Spec 冻结阶段

- 只创建 `docs/specs/slice-1-render-engine.md`。
- 不修改 Go 代码、模板、schema、fixtures、`scripts/verify.sh`、`docs/subsystems/` 或其他文件。

### 后续编码阶段建议顺序

1. **前置验证（首项、阻塞门槛）：** 固定 MiTeX 版本/来源/许可/哈希，验证其含 `.wasm` 包与 Typst 0.15.1 的离线兼容；并验证公式 token 与 Typst `measure()` 组合。
2. 在 `internal/ir/inline/` 实现 tokenizer、诊断模型及表驱动测试。
3. 在 `internal/render/` 实现 payload 预编译、资产路径安全/去重、本地 resolver、接口、workspace 生命周期及并发/超时进程包装。
4. 扩展 `templates/exam.typ` 支持 RenderPayload、MiTeX、模式真值表、统一选项网格、纸张/分栏/页眉页脚/装订线、图片环绕和合并单元格。
5. 新增 `cmd/examrender/main.go`，串接验证器与 renderer。
6. 增加代表性 fixtures 与单元/集成/端到端/竞态测试；同步更新现状 subsystem 文档。
7. 升级 `scripts/verify.sh`，执行真实 Go 渲染链路并核验产物和离线包缓存。
8. 实际运行 `./scripts/verify.sh`；按仓库约定准确报告命令结果与未运行项。

### 预期涉及路径

`internal/ir/inline/`、`internal/render/`、`templates/exam.typ`、`cmd/examrender/main.go`、`third_party/typst-packages/`、`testdata/golden/`、`scripts/verify.sh`、`docs/subsystems/exam-ir.md`、`docs/subsystems/render-engine.md`。这些是未来实现路径，不是冻结阶段的修改清单。

## 12. 测试与验收标准

所有涉及子进程/临时目录的测试用 `t.Cleanup` 注册清理。执行 `go test -race ./...` 覆盖竞态；验证实现按仓库要求执行 `./scripts/verify.sh`。验收至少包括：

### 12.1 Inline 表驱动测试（`internal/ir/inline`）

- 每种 Token：纯文本、inline math、bold、italic、underline、带序号 blank；连续文本合并及相邻标记。
- 转义：NUL 字节 不作为示例；逐项测试 `\$`、`\*`、`\_`、`\\` 不开启对应标记，公式内 LaTeX 反斜杠保持。
- 未闭合 `$`、`**`、`*`、`__`：原文无损降级、产生稳定 diagnostic、后续有效文本继续解析，绝不 panic。
- 嵌套公式规则：`**已知 $a_n = 1$**` 与 `**x $y$ z**` 的精确 token 序列；不支持的样式嵌套按字面文本处理。
- blank 边界：N=1、较大合法数值、0、负数、非数字、整数溢出、缺少 `}}`；无效项原文保留并诊断。
- 中文、多字节 UTF-8、组合字符、emoji、标记邻接 Unicode 的 token 内容和 byte offsets；无效 UTF-8 不 panic、JSON 输出合法。
- fuzz/property 测试：任意字节输入不会 panic；诊断 offsets 落在输入 byte 范围；合法输出可 JSON marshal。

### 12.2 Payload 与资产测试

- golden IR 每个可渲染文本字段都进入预分词 payload；递归 children、answer、analysis、options、table cell 均覆盖。
- `math_block` 不被 inline parser 改写；缺失/未知 block 按约定报错或 warning。
- 恶意 asset ID（`../`、绝对路径、反斜杠、NUL、编码分隔符/非法字符）以及指向根外的符号链接均被拒绝；候选路径检查验证 `filepath.Rel` 而非前缀。
- 同一 asset ID 被多处引用时 resolver/读取/复制只发生一次，payload 引用共享路径；每次 Render workspace 独立；内容 SHA-256 正确。
- 复制限制、取消、缺失文件、目录外文件、读错误与无效 URI 均有确定错误和零遗留文件。

### 12.3 Renderer 并发、超时与清理测试（`-race`）

- 用注入慢命令并发触发多次 render，记录活动进程最大值；断言不超过配置 semaphore 上限，默认配置为 `runtime.NumCPU()`。
- 全部信号量占满时取消等待中的 context，断言及时返回且未启动命令。
- 命令执行中取消 context 和 deadline 超时都及时终止/Wait 子进程，并返回可辨识错误。
- 假命令非零退出并写 stderr，断言 error 包含可读诊断且被截断到上限。
- 成功、编译失败、取消、超时、资产复制失败后 workspace 全部移除；使用 `t.Cleanup` 清理测试自身目录/假程序。
- 多 render 并行时 workspace 不共享；不得向 repo/workdir 留下临时文件。

### 12.4 Typst 真实渲染与端到端

- 对 golden IR、student/teacher layout、黄金 assets 通过 CLI/Renderer 真实编译，输出字节非空且头部以 `%PDF-` 开始，MIME 为 `application/pdf`。
- 同一 IR 下 student 与 teacher PDF 不仅是有效 PDF，必须在页面文本/结构上体现答案/解析开关及答题留白的预期差异；对所有真值表开关组合做至少 renderer payload 层断言。
- `option_layout=auto` 使用测量 fixture 覆盖四项短文本、公式项、图片项、超宽项，断言结果只可能为统一 `1x4`、`2x2` 或 `4x1`，不存在 greedy 不对称行；显式布局按规则追加完整网格行。
- A4/A3/B4、单/双栏、header/footer、binding line on/off、float_right 及 colspan/rowspan 均有对应 golden/结构断言。
- 对 Typst 0.15.1 + vendored MiTeX package 执行在线与人为断网环境下的公式渲染；确保断网仍成功且无包下载尝试。若兼容性前置验证未通过，不得将切片标为完成。
- 渲染完成后工作目录无新垃圾文件，workspace 无论成功/失败均不存在；重复相同输入时非时间/路径元数据导致的行为稳定。

## 13. 冻结期已检查文件与未试事项

已完整阅读：`AGENTS.md`、`LESSONS.md`、`docs/code-standards.md`、`docs/subsystems/exam-ir.md`、`docs/subsystems/render-engine.md`、`schemas/exam-ir-v1.json`、`schemas/layout-spec-v1.json`、`templates/exam.typ`。同时检查过相关 IR/布局 Go DTO、验证器、`cmd/ircheck`、`scripts/verify.sh`、golden IR/layout/assets 路径与 `go.mod`。

冻结设计时已确认本机 `typst 0.15.1`、Go、`mutool` 可执行；Typst cache 路径未发现现存包内容。**Not tried：** 未获取 MiTeX 包、未验证 MiTeX+Typst 0.15.1 兼容性、未执行 Typst compile/PDF render、未执行 `./scripts/verify.sh`、未实现或运行 Go tests/race tests。MiTeX 兼容性是后续编码的第一项阻塞验证；本文件只定义规格，不宣称任何未执行验证通过。

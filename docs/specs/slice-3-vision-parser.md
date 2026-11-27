# MVP Slice 3 — 脏卷视觉解析、配图去水印与 Schema 自纠

> **状态：冻结的实现规格（proposal）**。本文件定义后续实现目标，不代表现状已经具备这些能力。实现合并时再更新 `docs/subsystems/*.md`；本切片规格阶段不修改现状文档。

## 1. 目标与范围

在切片 2 的 PDF ingest/parser/worker/store 边界上增加无 CGO 的 PDF 页面栅格化、归一化配图裁剪、纯 Go HSV 清洗、OpenAI-compatible 多模态解析、受限 Schema/语义自纠与双资产持久化契约。实现应把脏卷转换成可由现有 `ir.Validator` 验证、现有 assembler 持久化并由 renderer 消费的 Exam IR。

设计原则：

- 保持 `ParserAdapter.Parse(ctx, Source) (ParseResult, error)` 作为解析边界。Parser 返回候选 IR 和资产字节，不上传对象、不写数据库、不操作 River；pipeline worker 仍拥有验证后的上传与持久化副作用。
- 不引入 CGO 或带 CGO 的直接/传递依赖。PDF 操作统一使用有界 `os/exec` 子进程；像素处理和缩放使用 Go 标准库 `image`。
- `scripts/verify.sh` 必须始终完全离线，不请求外部 API、不消耗真实模型 token。云端交互通过环境变量注入，live 验收只能人工显式触发。
- 解析、栅格、清洗均设有输入大小、像素、并发、请求体/响应体、超时上限；失败时按错误分类进入 River 的既有取消或退避路径。

## 2. Explicit Non-Goals

本切片不做 HTTP API 服务、前端校对台、Word `.docx` 导出、本地 Python GPU Worker、LaMa/Real-ESRGAN、独立 OCR/版面解析供应商、向量化/embedding、Typst 排版颜值或多主题重构。未来 OCR/版面服务仅保留 adapter 扩展点，不在本切片接入。真实付费 VLM 请求不得进入默认或集成自动门禁。

## 3. 切片 2 兼容审计与演进要求

### 3.1 当前接口事实

- `internal/parser/parser.go` 的 `Source` 提供 job/key/hash/有界读取函数；`AssetBlob` 仅有单份 `Data []byte`；`ParseResult` 为 IR 加 `[]AssetBlob`。解析器错误已分为 permanent/transient。
- `internal/pipeline/worker.go` 先调用 parser，再 `ValidateExamDTO`，后将每个资产的唯一 blob 写入 `RawAssetKey`，最后构建 `store.VerifiedAsset` 并持久化。当前没有 raw/clean 双载荷路径。
- `storage.RawAssetKey`、`storage.CleanAssetKey` 已存在。`store.VerifiedAsset` 已有主 `StorageKey`、可选 `RawStorageKey`、单个 `SHA256`；assembler 对 `raw_cropped` 期望 raw 主键，对 `cleaned`/`vectorized` 期望 clean 主键并要求 raw companion。
- 资产表当前仅对主 `storage_key` 保存一个摘要。`S3AssetResolver` 使用 registry 主对象 key/digest，不以 IR 中 URI 选对象。必须为两个不同字节的对象补充独立摘要契约。
- `schemas/exam-ir-v1.json` 的 asset 已含 `source_bbox`、`status`、`aspect_ratio`、`suggested_width_mm`、`uri`、`raw_uri`；状态枚举为 `raw_cropped|cleaned|vectorized`。BBox schema 限定 page >=1、四坐标各在 [0,1]，但不限制坐标顺序，也无法约束 page 不超过当前源 PDF 页数。
- `ir.Validator.ValidateExamDTO` 执行 JSON Schema + 语义检查（包括 asset map key 与 `asset_id` 一致、image refs 解析）。切片 3 对 `source_bbox` 增加源文档相关校验，不改动 IR v1 schema。
- Pipeline 对永久 parser 错误记录失败并返回 `river.JobCancel`；transient 错误原样返回 River 退避重试。该切片复用既有分类机制。

### 3.2 最小双资产演进

扩展 parser 返回载荷，使一个资产能携带 raw crop 与可选 clean 输出；建议数据形状：

```go
type AssetBlob struct {
    AssetID   string
    MediaType string
    RawData   []byte
    CleanData []byte // 成功清洗时非空；raw_cropped 时为空
    RawSHA256 string // 可选声明；worker 必须重算并核对
    SHA256    string // 可选 clean 声明；worker 必须重算并核对
}
```

实际字段可使用命名嵌套 variant DTO，但语义必须等价：每个 IR 资产恰有一个载荷；raw 与 clean 的摘要独立校验；未声明 digest 不表示可信。Cloud parser 保持无持久化副作用，pipeline worker 对象上传时选择服务器推导的 canonical key：

- `raw_cropped`：只上传 `RawAssetKey`，主 `storage_key` 指向 raw 对象，`raw_storage_key` 和 `raw_sha256` 为空；该主对象必须能由 `S3AssetResolver` 正常读取并渲染。
- `cleaned`：上传 raw 和 clean 两个不同变体，主 `storage_key` 指向 `CleanAssetKey`，`raw_storage_key` 指向 `RawAssetKey`；`sha256` 校验 clean 对象，`raw_sha256` 校验 raw 对象。
- 不在本切片生成 `vectorized`。对既有状态不得改变 assembler 的既有校验约定。
- 清洗失败回退必须如实返回 `raw_cropped`，附 warning，`CleanData` 为空；不得把相同 raw 字节复制成 clean 伪装成功，也不得重复上传两份相同字节。

数据库迁移增加 `raw_sha256` 列，**允许 NULL**，以兼容迁移前已存在行；不为历史记录伪造 digest。新建 `cleaned` 行要求 raw key 与有效 raw digest 同时存在，主 digest 仍校验 clean 对象。修改 sqlc query/model、`VerifiedAsset`、assembler 的 key/status/identity/conflict 判断、store fake、resolver/asset lookup，使新写入的 raw 与 clean 对象均能独立核验。旧行 `raw_sha256 IS NULL` 时保持旧主对象读取行为；若尝试读取 raw companion 而摘要为空，按既定兼容规则拒绝未验证 raw 读取或仅允许原始主对象读取，不能静默用 clean digest 验 raw。

### 3.3 ID 与元数据所有权

VLM 输出的 asset key 是单次解析范围内的不可信临时标识（如 `fig_1`），不是 IR/persistence ID。每个候选资产由 Go 服务端生成 `ast_` + UUID（或等价密码学唯一 ID），并在验证之前同步替换：

1. `ExamDocumentIR.Assets` map key；
2. 对应 `Asset.asset_id`；
3. 所有深层 `image_ref.asset_id`（question stem/options/answer/analysis/children）。

重映射使用单一映射表，遇到未定义引用、重复临时 key、悬空引用或 ID 冲突作为永久 IR 输入错误。模型不得决定持久化对象 key、storage URI 或 digest。服务端在最终结果中写入 resolver-neutral URI/status；URI 不包含磁盘路径、对象存储 endpoint 或 object key。

## 4. 分阶段解析数据流与边界

### 4.1 主流程顺序（顺序不可调换）

1. `parser.ReadSource` 有界读取并验证 PDF 签名与注册 SHA-256。
2. `raster` 探测页数并按 DPI 栅格化高清本地页图。
3. 每页图生成独立的 VLM 传输副本：纯 Go 等比缩小，使长边不超过配置值（默认 2048 px），编码 JPEG，设定质量参数和最大编码字节。**高清原始 PNG 保留在本地供 bbox 精裁；不得使用压缩传输图生成最终配图。**
4. 并发发送逐页视觉抽取请求；每页结果包含阅读顺序内容、题/小题边界、选项、答案/解析线索、临时图像 key 与归一化 `source_bbox`。
5. 按页号稳定排序并合并候选 Exam IR；跨页/卷末信息先尝试确定性归并，再按 4.2 规则决定是否调用纯文本 LLM 归并。
6. 服务端重映射全局 `ast_...` ID。
7. 对候选运行**完整** `ir.Validator.ValidateExamDTO`，并运行 Slice 3 专有 bbox/资产约束：`source_bbox` 必填、坐标有限且 corner 几何顺序正确、bbox 页号不超过实际页数、所有模型资产均被有效引用且 bbox 可映射到对应源页。任何错误进入自纠环，不得先裁剪。
8. 最多默认 2 轮 schema/语义自纠；每轮后重新运行 IR + bbox 全验证。修复不得擅自篡改/重置服务端 ID、不得新增/删除/重新绑定资产，也不得制造没有合法 `source_bbox` 的资产。对模型可修字段只做受控修复；如果必须修改受保护的资产映射或 bbox 不可修，永久失败。
9. 自纠成功后进行一次只读全验证；通过后才在本地高清页 PNG 上执行 `CropPNG` 与 `HSVCleaner`。
10. 将真实 `aspect_ratio`、`suggested_width_mm`、`status`、warning 回填至资产元数据；status 仅按真实结果选 `cleaned` 或 `raw_cropped`。最终再次只读执行完整 Validator 与 bbox/资产关联验证；不再触发 LLM 修复。
11. 返回 IR 与 raw/clean `AssetBlob`。worker 执行上传和 persistence；parser 不保留临时目录或对外部存储产生 side effect。

在自纠后裁剪能避免非法 bbox 直接触发图像操作，也避免修复后已生成的图片载荷与 IR 脱节。自纠通过后如果真实裁剪/解码仍失败，该 bbox/源页对输入而言不可处理，作为永久 parse failure（错误路径含资产 ID/page，不含整份图片数据）。清洗算法失败单独按 raw_cropped 回退，不导致 parser 总体失败。

### 4.2 确定性归并与纯文本 LLM 归并边界

- **确定性合并必做：** 按源页及页内 reading order 排列内容；合并可判定的标题/题号连续关系；保持各页 provenance；同页/相邻页重复候选去重须使用可审计的规则；答案页显式题号与唯一 display number 可直接绑定；保持不确定字段为空/警告，不凭空推断答案。
- **仅当需要跨页语义推断时调用纯文本归并 LLM：** 例如页末题干不完整、下页以续文开始且无法由规则断定连接，或卷末答案/解析没有可确定的显式编号但上下文可被模型可靠匹配。请求只含有界页面抽取文本、结构化题目/答案候选及必要页号，不附加任何图片 Base64，也不重新发送高清图。
- 纯文本归并的输出仍是候选 IR patch/关系，不直接写 storage metadata；必须限制输入/输出字节和 token，并应用 allowlist patch，禁止它改变服务端 ID、添加没有合法 bbox 的资产、改写不相关题目。歧义未消解时保留 warning/未绑定信息，不能强行配对。
- Repair 阶段针对 Validator 与 bbox 专有校验的具体路径错误，不等价于无界“再解析整卷”。服务端受保护字段在 prompt 和应用层双重约束。

### 4.3 超时、取消与 River 预算

配置应具有以下独立上限：单页 HTTP timeout、并发页数、页速率、纯文本归并 timeout、每轮 repair timeout、raster process timeout、整次 parse 总预算 `total_parse_timeout`。请求须尊重父 context；不要以忽略 `ctx.Done()` 的后台请求续跑。总预算必须严格小于 River ingest job timeout，并为 worker 上传、状态更新、持久化及错误收尾保留明确余量。配置校验应拒绝不可能满足此不等式的预算组合。

调用页 VLM/归并/修复前检查总预算剩余量；若不足以在 job deadline 前完成该阶段并留出预留时间，则在发起外部调用前返回 context/deadline transient。River 超时不能短于正常长卷的配置总预算。River 重试可重新调用已完成的页 API，当前没有跨 attempt 的请求级去重/结果缓存契约；因此应记录阶段耗时与 provider usage，并明确重试仍可能再次计费，不得声称 exactly-once。部署配置需通过代表性长卷测量后确定预算，不允许由无限延长 River timeout 掩盖无界工作。

## 5. 核心包、接口与配置

### 5.1 `internal/vision/raster`

建议接口：

```go
type Page struct {
    Number int
    DPI    int
    Image  image.Image // 或受控 PNG 文件/reader；不得泄露临时路径
}
type Rasterizer interface {
    PageCount(ctx context.Context, pdf []byte) (int, error)
    Render(ctx context.Context, pdf []byte, pages PageRange) ([]Page, error)
}
type CropResult struct {
    PNG              []byte
    SHA256           string
    Width, Height    int
    AspectRatio      float64
    SuggestedWidthMM float64
}
func CropPNG(page image.Image, rect [4]float64, dpi int, cfg CropConfig) (CropResult, error)
```

实现要求：

- MuPDF renderer 通过 `os/exec` 的 `CommandContext`，并为每条外部命令设置 `context.WithTimeout`。输出到专属临时目录，`defer os.RemoveAll` 覆盖成功、失败、取消、超时路径；绝不把用户输入拼成 shell 命令。
- 使用全局 CPU semaphore 对 raster process/重型图像工作做有界并发；即使调用方并发创建 renderer，也不能绕过全局配置。`errgroup.SetLimit` 另行限制页请求，不代替全局子进程 semaphore。
- 页数通过 `pdfinfo` 输出中唯一锚定的 `Pages: <positive integer>` 行确定性解析；非零 exit、缺失/重复/非法行均失败。最大页数检查必须先于 `mutool draw`。执行 PDF 工具时固定环境/参数，限制 stdout/stderr 捕获量。禁止使用 `mutool info` 文本的猜测式页数解析。
- `mutool draw` 参数明确 `-F png`、DPI、页范围、输出 `%d` 模式；校验输出页数与请求页号一致，解码后检查实际像素尺寸/像素数/文件大小。
- 默认 DPI 取 250（允许配置范围 200–300 为常用区间，整体硬上限由配置约束）；默认最大页数、每页像素上限、总解码字节、超时与 semaphore 数必须可配置且设安全缺省值。超页/资源上限视为永久输入错误。
- BBox 明确定义为 `[x1,y1,x2,y2]` 的归一化 corner 坐标。拒绝 NaN、Inf、越界、`x1>=x2`、`y1>=y2`。把坐标乘以对应页宽高并采用向外取整以避免吞掉边缘内容；先加配置 padding，再钳制到图像 bounds，最终空矩形拒绝。padding 单位明确为输出像素或 mm（建议配置为 mm 并按 DPI 换算）。输出 PNG SHA-256 为 raw crop digest；`suggested_width_mm` 基于裁剪宽度/DPI 换算并受版心最大宽度约束；统一使用 `25.4 mm/in`。

### 5.2 `internal/vision/clean`

```go
type ImageCleaner interface {
    Clean(ctx context.Context, src io.Reader) (*CleanResult, error)
}
type CleanResult struct {
    PNG      []byte
    SHA256   string
    Warnings []string
}
type HSVConfig struct {
    // RedHue/BlueHue circular ranges, SaturationMin, MarkValueMin,
    // WatermarkSaturationMax, WatermarkValueMin,
    // DarkProtectValueMax, plus pixel/byte limits.
}
```

实现与语义：

- 纯 Go `image/color` 像素遍历，不引入 OpenCV/Gocv、go-fitz、C binding 或 CGO 依赖。将 RGB 归一化后计算 HSV；红色色相必须支持 0° 附近的环绕区间，蓝色范围可配置。
- 参数化红/蓝 Hue 范围、高饱和度阈值、批注亮度下限、浅灰/低饱和 watermark 的亮度上下界、深黑保护阈值、背景替代策略与最大像素数。配置在构造阶段验证范围；不接受 NaN/Inf 或相互矛盾阈值。
- 分类优先级：深色保护（印刷文字、细黑几何线、虚线）先于清理逻辑；之后清除红/蓝高饱和手写批注，再清除满足低饱和且高亮度条件的浅灰水印/扫描底纹；其余像素保留。默认替换纯白，可选局部背景亮度但必须有确定性边界与单元测试。
- 每个输入和输出受字节/像素上限限制；ctx 在解码后像素处理期间按行检查。成功输出标准 PNG 和 SHA-256。
- 清洗失败返回错误供调用层记录 warning 并回退为 raw crop、资产 `status=raw_cropped`；不得将原图重复填入 CleanData。可观测 warning 内容必须有界且不含图片内容/凭据。

### 5.3 `internal/parser/cloud`

- `CloudAPIParser` 实现既有 `parser.ParserAdapter`，构造时注入 validator、rasterizer、cleaner、HTTP client、配置、ID generator、日志/诊断 sink；关键依赖可 fake。HTTP client 不从环境代理/自定义 transport 暗中绕过显式配置，live endpoint/key/model 只从明确的 `EXAMFORGE_VLM_BASE_URL`、`EXAMFORGE_VLM_API_KEY`、`EXAMFORGE_VLM_MODEL` 环境变量加载。不得把 key 放在 CLI flag、源码常量、fixture、错误或日志。
- 采用标准 OpenAI-compatible Multimodal Chat Completions 请求/响应；base URL 不硬编码厂商域名。限定协议版本与模型字段、请求大小、响应大小、token 上限；遇到不兼容响应结构给出安全的 parser error，不记录完整 HTTP body。`Authorization` 值及请求图片 Base64 均不得写日志。
- 定义窄接口以隔离协议：`PageExtractor` 负责一页多模态抽取；`TextMerger` 负责纯文本跨页归并；repair 可复用 text client 但要有独立 system/task schema。未来 OCR/版面工具实现独立 adapter，不将其混入 OpenAI-compatible transport。
- 每页抽取结构应包含页号、阅读顺序 block/题目候选、选项、答案线索、临时图 key 与 bbox；要求模型仅返回可 parse JSON，不接受模型声称可信的 ID/URI/digest/status。输入图像使用第 4 节压缩副本，最终 bbox 仍映射到同页高清原图。
- 分页并发使用 `errgroup.WithContext` + `SetLimit`；所有调用共享 `golang.org/x/time/rate` token bucket，并有 HTTP request timeout。结果放入按 page number 索引的槽位后确定性合并，不因网络完成顺序改变输出。取消父 ctx 后等待 group 返回并停止后续请求，不遗留 goroutine。
- 诊断记录页数、各阶段耗时、provider 返回的 token usage、修复轮次、告警计数。日志不输出完整 IR、图片 Base64 或 secret；候选 IR 只可作为受限 repair 请求体传给 provider，不进入日志。

## 6. 自纠、响应安全与错误分类

### 6.1 自纠规则

- 对归并且 ID 重映射后的候选运行 `ir.Validator.ValidateExamDTO`，另运行 bbox 专有 validator。错误格式包括 JSON pointer/字段路径、稳定错误码、简洁原因和页号/资产临时 key（如适用）。
- 默认 `max_repair_rounds=2`，可配置但设置硬上限；round 数只计算修复请求，不含初始页抽取/必要 text merge。每轮有独立 timeout 且受总预算与 ctx 控制。
- 修复请求传有界候选 JSON 与截断的错误列表；禁止图片 Base64。修复响应解析为候选 patch/IR 后，重新应用服务端不可变字段/ID映射约束，再执行完整 IR+bbox 校验。禁止通过丢掉资产、虚构 asset bbox 或改动 ID 来“修好”校验。
- 成功后先只读复验，然后才裁剪清洗。图像 bytes/几何元数据回填完成后再最终只读复验。最终复验失败不再请求 LLM，视为实现/输入一致性 permanent defect。
- HTTP body 使用硬限制 reader；结构化错误只记录 status、有限响应片段的脱敏摘要；token usage 无论是否有修复都累加。

### 6.2 错误分类表

| 场景 | Parser 错误分类 | Worker / River 行为 |
|---|---|---|
| `%PDF-` 缺失、hash mismatch、损坏 PDF、页数超过上限、bbox 页号/几何非法且修复耗尽 | permanent（稳定 code） | 记录 failed，`river.JobCancel` |
| IR/schema/语义错误经 2 轮仍未修复；不可解析/超限响应；HTTP 400、401、403 | permanent | 记录 failed，`river.JobCancel` |
| HTTP 429、5xx、DNS/连接错误、HTTP timeout、provider transient unavailable | transient | 返回普通 error，River backoff 重试 |
| parent ctx canceled、worker shutdown、Job timeout | 非永久 context/deadline | 返回普通 error；不得将输入记成 failed |
| S3/对象存储 transient unavailable | transient | River backoff 重试 |
| 同 key 不同内容、asset 缺失/未引用、IR 与 blob 不一致、明确非法记录 | permanent | 记录 failed，`river.JobCancel` |
| HSV cleaner 失败但原始 crop 可用 | 不作为整卷失败 | warning；asset `raw_cropped`，只保存 raw |

所有错误在持久化前继续经现有 `storage.Redact` 规则过滤 URL/token/密钥并限长；新增 parser 错误 code 不携带 provider response body、完整 source URI 或 image payload。

## 7. CLI 与三层验收

- 在 `cmd/exampipe` 增加显式解析器选择，或新增 `cmd/exameval` 专用本地评估命令：`mock` 可无凭据离线执行；`cloud` 必须显式选择且检查完整 VLM 环境配置。CLI 支持传入本地脏 PDF，执行解析、worker 上传/组装与 Typst render，输出结构化摘要（页数、阶段耗时、usage、修复轮次、warning 数、PDF 路径）。API key 不作为命令行参数、不回显。
- **默认离线 `scripts/verify.sh`：** 0 网络、0 付费 token；测试使用 localhost `httptest.Server` fake、合法小型 PDF fixture 和本地 mutool。包含 `-race` 全链路 parser 测试、HSV 物理证伪、asset 双 digest/pipeline/store/render 覆盖，并保留既有切片 1/2 门禁。
- **`scripts/verify-integration.sh`：** 继续只用于显式 PostgreSQL + SeaweedFS 测试环境；允许增加 httptest fake VLM 的集成 ingest，不调用真实服务；所有服务资源按现有 integration cleanup 契约回收。
- **手动 Live：** 操作者显式设置 `EXAMFORGE_VLM_*` 后手动运行 CLI，使用真实 endpoint/provider。绝不由 `verify.sh`、integration gate、CI 默认任务或测试发现逻辑触发。

## 8. 未来文件变更清单（仅实现阶段）

### 新增

- `internal/vision/raster/`：MuPDF wrapper、页数解析、BBox crop、进程/资源限制测试。
- `internal/vision/clean/`：接口、HSV cleaner、阈值/像素物理证伪测试。
- `internal/parser/cloud/`：HTTP协议 client、分页抽取、文本 merge、ID remap、repair、诊断和 httptest。
- 小型合法多页 PDF fixture（优先程序生成或明确来源与许可）及合成图像测试 helper。
- DB migration：增加 nullable `raw_sha256`，并覆盖已有数据迁移行为。

### 修改

- `internal/parser/parser.go`、`internal/parser/mock.go`：双变体解析结果。
- `internal/pipeline/worker.go`、`internal/pipeline/assets.go`：双对象验证、受限并发 immutable put、status 与 digest 选择。
- `internal/storage/keys.go`（复用已有 raw/clean key）、storage resolver 与 asset lookup；保持 raw_cropped 主对象正常解析。
- `internal/store/assembler.go`、store DTO/SQL queries/sqlc generated code、store fakes/tests：双 digest 和 status/key 一致性。
- `cmd/exampipe` 或新增 `cmd/exameval`；`scripts/verify.sh` 增加离线 raster/clean/cloud fake tests。
- 后续实现按架构规则同步更新相关 `docs/subsystems/*.md`；本规格冻结阶段不动这些现状文档。

## 9. 分步实现顺序

1. 固定接口/限额/错误与 ID 规则；先设计并实现 schema-first nullable `raw_sha256` migration、store/sqlc/resolver 双对象读取与旧行兼容。
2. 扩展 parser asset payload、mock；worker 增加 raw_cropped 单对象与 cleaned 双对象上传/摘要校验；证明 resolver 可渲染两种 status。
3. 实现 raster 子进程、页数探测、输出护栏和 bbox crop；先用假命令证明注入 seam 生效，再加真实小 PDF mutool 物理测试。
4. 实现纯 Go HSV cleaner 与可配置阈值；合成图做像素级去除率/黑线保留率断言。
5. 实现 VLM 协议 client、压缩传输图、多页并发与 rate limiter、稳定归并、server asset remap、校验/自纠，然后裁剪清洗及元数据回填。
6. 完成 River error class、总预算与 Job Timeout 配置验证；实现 CLI 与诊断输出。
7. 添加 pipeline/store/e2e/integration fake 服务用例，运行 `./scripts/verify.sh`；integration 与 live 仅按各自显式方式验证。

## 10. 自动化测试与验收矩阵

### 10.1 Raster / subprocess

- 程序生成或内置极简合法多页 PDF，使用真实 `mutool` 验证各页 PNG 输出、页数、页范围、DPI 设定对应的像素尺寸、输出页号和高 DPI 图像存在。
- 超页数文件在执行 draw 前拒绝；pdfinfo Pages 行缺失/重复/非整数/非正数、命令非零退出均拒绝且错误稳定。
- 假命令记录 argv，断言固定参数/页范围正确且 factory 真被调用；长时间假命令配短 timeout，断言子进程退出、context deadline 返回、临时目录无残留。
- 并发任务超过 semaphore 时观测最大活动子进程不越界；成功、命令失败、取消、超时所有路径 `t.Cleanup`/defer 后均无临时目录残留。
- `CropPNG` 覆盖正常 bbox、padding、四边钳制、像素向外取整、aspect ratio、DPI→mm；拒绝非法序、零面积、NaN/Inf、超界、非法 page、超像素和空输出。

### 10.2 HSV 物理证伪

- 程序合成同一 PNG：白/扫描灰底、灰色斜水印、红色手写标记、蓝色批注、纯黑实线/细线/虚线和印刷字迹；保留各类别输入 mask 供精确像素统计。
- 清洗后分别断言红/蓝/浅灰目标像素去除率达到规格配置阈值（初始验收建议 >=95%，阈值须由 fixture 精确统计定义），黑色线/字保留率 >=99%，并断言输出仍有预期非白像素/连通线段，防止全白结果误过。
- 覆盖 hue 0° 红色环绕、阈值边界、色彩空间量化、透明输入策略、损坏图像、超像素、ctx cancel；失败回退 `raw_cropped` + warning 且仅产生 raw payload。

### 10.3 Cloud API、合并与自纠

- `httptest.Server` 检查 OpenAI-compatible method/path/headers/body；确认图片请求使用限长 JPEG 而不是原始高清 PNG/Base64，响应体限额与 error redaction 生效，API key 不出现在日志/错误。
- 逐页响应延迟乱序时，合并结果仍按页号稳定；测量最大并发不超过 `SetLimit`，共享 token bucket 限速有效；取消 ctx 后所有请求结束、无 goroutine 泄漏。
- 对确定性可绑定的跨页续文/答案使用规则路径且不额外调用归并模型；对歧义跨页/无显式答案编号调用纯文本 merge，并断言请求没有图像内容；歧义未解不强制错误绑定。
- 初轮 IR 缺必填字段及 bbox `x1>=x2` 或页号越界，检查第 2 次 repair 请求含准确 JSON 路径；成功修复后先验证再裁剪。修复期间尝试改 server ID 或新增无 bbox 资产必须拒绝；超过 2 轮仍非法返回永久 parser Error。
- HTTP 429/503、网络超时→transient；400/401/403→permanent；坏 JSON/响应过大/未知协议结构按错误表分类。
- 相同模型本地占位 key（如 `fig_1`）跨不同 parse remap 为不同全局 `ast_...`；所有嵌套 image refs、map key、asset_id 一致，悬空引用拒绝。
- 验证 metrics 只记录页数、分段耗时、usage、repair count、warnings；不打印 Base64/密钥/整份图片。

### 10.4 Dual assets / pipeline / integration

- raw-only `raw_cropped` 上传单对象、asset main key 指向 raw、resolver 可读取并渲染；cleaned 上传 clean+raw，各 digest 分别验证且 main key 为 clean。
- raw digest migration 对已有行保持 NULL 且 migration 成功；旧行主对象 resolver 继续工作；新 cleaned 行缺 raw digest/错摘要永久拒绝。
- immutable put 相同摘要幂等、不同摘要冲突；缺失/重复/多余 blob、IR status 与 payload 不匹配、无 bbox 资产、媒体类型不符、超大小均拒绝。
- fake parser/VLM 通过真实 worker、内存对象存储/store fake 完成验证及持久化；对数据库关键迁移/assembler/resolver 行为增加显式 integration 覆盖。
- `go test -race` 覆盖 parser 并发、cleaner、worker 双上传、store fake；默认 gate 中所有外部模型请求必须指向 httptest localhost，测试运行时不得依赖任何 VLM 环境变量或网络。

## 11. 当前环境探测记录（设计阶段）

WSL 环境中已实测：

```text
$ command -v mutool
/usr/bin/mutool
$ mutool -v
mutool version 1.27.0
$ command -v pdfinfo
/usr/bin/pdfinfo
$ pdfinfo -v
pdfinfo version 26.01.0
Copyright 2005-2026 The Poppler Developers - http://poppler.freedesktop.org
Copyright 1996-2011, 2022 Glyph & Cog, LLC.
```

`mutool pages` usage 显示 `mutool pages [options] file.pdf [pages]`，接受逗号分隔页号/范围；`mutool draw` usage 显示 raster 格式包含 PNG、`-F` 可固定格式、`-r` 设置 DPI、`-o` 支持 `%d` 页号输出并可指定页范围。`mutool info` usage 没有页数参数（`-M` 为列页面尺寸），所以规格选用锚定解析 `pdfinfo` 的 `Pages:` 行做预检。探测命令未实际渲染 PDF。

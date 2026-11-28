# MVP 切片 4B：左右对照校对台 Web Workbench 技术规格

> **状态：已冻结（设计规格）。** 本文件冻结 4B 前端的工程边界、运行时契约、状态模型、无损编辑规则、测试门禁和浏览器验收矩阵。
>
> 本文是提案/实现依据，不代表本文件列出的 `web/`、`scripts/verify-web.sh` 或 P0/P1 后端工作已经存在。实现本规格前必须遵守“实现前置条件”章节；本轮冻结只创建本 Spec，不创建 Web 代码、不修改 Go 后端、`api/openapi.yaml`、根验证脚本或 `docs/subsystems/` 现状文档。

## 1. 背景、目标与非目标

### 1.1 背景

切片 4A 已提供入卷、任务查询、试卷详情、源 PDF、题目条件更新、资产变体代理和 LayoutSpec 渲染 HTTP API。4B 在其上提供一个面向桌面宽屏的专业校对台：左侧保留原卷溯源，右侧编辑结构化题目，并将每一次保存和渲染都绑定到 4A 的强 ETag、revision 与错误 envelope。

前端不得把 IR 当作数据库模型，也不得复制 Typst 的分页算法。服务端保存的 IR 是校对工作台的内容来源；`LayoutSpec` 是独立的渲染策略输入。

### 1.2 目标

- `/`：PDF 上传、任务队列、试卷列表、进入校对台。
- `/exams/:examId`：左右对照校对台。
  - 左栏使用本地打包的 `pdf.js` 查看完整源 PDF。
  - 根据题目 `provenance.bboxes` 联动页码和归一化矩形高亮。
  - 右栏编辑当前 4A 支持的题目字段，保留脏状态、冲突和 CoW 提示。
  - 对表格和未知内容块提供只读预览，避免前端重建时丢字段。
  - 展示并切换 raw/clean 图片变体，明确资产 selector 的全局作用域。
  - 使用完整 `LayoutSpec` 调用服务端渲染，展示 PDF 和非阻断 warning。
- 在没有后端、数据库、对象存储或网络的情况下，完成 TypeScript 类型、API 客户端、状态模型、BBox/inline/缓存/取消/资源释放的自动化测试。
- 所有运行时资源（pdf.js worker、KaTeX CSS 与字体、CMap/standard fonts 如需要）本地打包，断网可运行。

### 1.3 非目标

- 用户登录、权限管理、多租户和公网安全边界。
- 跨卷题库搜索、自由组卷、创建空白试卷、拖拽重排。
- 富文本 WYSIWYG 编辑器、任意 HTML/Markdown 解释器、Typst 主题设计器。
- 移动端/手机小屏布局；目标是桌面宽屏专业工作台。
- PDF Range/分片加载；4A 当前只提供完整 `200` 对象。
- 4B MVP 内未知开放块字段的持久化保证和 composite child 的独立写入；二者列入 P1 后续后端演进契约。
- 本 Spec 阶段修改 4A Go 实现、OpenAPI、脚本、数据库迁移或现状文档。

## 2. 实现前置条件与契约分级

### 2.1 P0：4B 编码前必须完成的轻量契约补丁

P0 不扩展业务能力，不阻塞 4B 的 MVP 交付范围；它只保证 OpenAPI 类型生成不会绕过已知的契约漂移。P0 必须先由单独变更完成并获审查：

1. 对齐 `api/openapi.yaml` 与 4A handler 的已知传输语义，至少覆盖：
   - `PatchedItem` 成功响应实际含 `item`，OpenAPI 必须声明它。
   - `stem:null` 的实际允许/拒绝语义必须与 operation description、schema、handler 一致。
   - `provenance:{}`、`review_status` 清除语义、运行时 score 上限必须明确为同一契约。
   - 资产 GET 的 `If-None-Match`/304 行为、实际错误状态和 render 422 的 details 能力应有准确说明。
2. 扩展 `cmd/openapicheck` 的本地引用检查：
   - 遍历所有外部 `$ref`，包括 `../schemas/exam-ir-v1.json`、`../schemas/layout-spec-v1.json` 以及 `#/$defs/...` fragment。
   - 校验 JSON Pointer 节点真实存在，而不是只编译 external schema 根对象。
   - 禁止远程 `$ref` 和越界路径。
   - 负例（损坏 fragment、远程引用、越界 pointer）必须使门禁非零。

P0 不包含未知块保真、children 写入、当前卷资产作用域等产品能力；本轮不实施 P0，只在本 Spec 记录为实现顺序的第一步。

### 2.2 P1：4B 之后的独立后端演进契约

P1 不阻塞 4B MVP。它记录需要另一个后端切片单独设计、实现和验收的能力：

- **开放块原始 JSON 保留**：当前 Go `ContentBlock` 是 flat struct。合法的开放 `type` 可以带 Go DTO 未声明的字段，但 decode/持久化/再编码会丢失这些字段，严格读回还可能拒绝旧字段。P1 需要为开放 block 提供受控 raw JSON/扩展字段 seam，并覆盖 assembler、fingerprint、CoW、PATCH、读回和渲染的 GET → 修改兄弟字段 → GET 无损测试。
- **Composite child 独立写入**：当前 children 没有 `exam_items` occurrence、独立 ETag 或 PATCH 路径。P1 需要稳定 child identity、父子并发版本、编辑范围、递归错误路径、review 统计和权限/审计语义。

P1 未完成前，4B 对未知块和 children 的行为必须是只读预览、防误提交和明确提示；不能以浏览器端保留对象副本来宣称服务端持久化无损。

## 3. 4A 契约审阅与前端必须遵守的事实

本节以当前 `api/openapi.yaml` 与实现为准，列出前端不可猜测的事实。P0 完成后，若契约文字改变，必须同步更新本节和生成类型。

### 3.1 路由和响应

| 路由 | 前端契约 |
|---|---|
| `POST /api/v1/ingest` | `multipart/form-data`，恰好一个名为 `file` 且有文件名的 part；实现还检查前五字节 `%PDF-`。成功为 `202`，返回 `job_id/state=queued/created_at/updated_at`，并返回相对 `Location` 和 `X-Request-Id`。没有取消或幂等键。 |
| `GET /api/v1/jobs` | `{items,next_cursor}`；`limit` 1–100，默认 50；可选 `cursor`、`state`。排序 `created_at DESC, job_id DESC`。 |
| `GET /api/v1/jobs/{job_id}` | `Job` 含 `state`、attempt/time 字段及可空 `exam_id/error_code/error_summary/started_at/finished_at`。失败任务正常以 `200` 返回 `state=failed`，不是 HTTP error。 |
| `GET /api/v1/exams` | `{items,next_cursor}`，按 `created_at DESC, exam_id DESC`；摘要包含 `exam_id/title/subject/created_at/pending_review_count/section_count/item_count`。没有服务端搜索参数。 |
| `GET /api/v1/exams/{exam_id}` | `{ir,metadata,items,assets}`。外层 `metadata.exam_id` 是 URL 所需数据库 UUID；`ir.exam_id` 是 IR 字符串，不可替代。 |
| `GET /api/v1/exams/{exam_id}/source-pdf` | 完整 `200 application/pdf`，`Content-Disposition: inline`，带源摘要 ETag；Range 被忽略，不支持 `206`。全局 `X-Frame-Options: DENY`，不得依赖 iframe。 |
| `PATCH /api/v1/exams/{exam_id}/items/{item_id}` | 必须带当前 item 的强 `If-Match`。写目标只能是 `item_id`，不能是共享 `question_id`。成功响应含 `item_id/question_id/revision/etag/item`，并在响应头返回 ETag；成功后仍重取 exam 以更新 `is_shared` 和外围 metadata。 |
| `GET /api/v1/assets/{asset_id}?variant=active|clean|raw` | 返回图片字节，不返回资产 JSON；缺省是 active。active 是 `no-cache`，显式 raw/clean 是 immutable 长缓存；实现支持图片响应的 `If-None-Match`/304。只可加载注册并经摘要校验的 MIME。 |
| `PATCH /api/v1/assets/{asset_id}/active-variant` | body 是 `{variant:"raw"|"clean"}`；必须带资产外围 metadata 的强 ETag，成功返回新 `active_variant/available_variants/revision/etag`。selector 是全局 `asset_id` 状态。 |
| `POST /api/v1/exams/{exam_id}/render` | body 是裸完整 `LayoutSpec`，不是 `{layout:...}`；成功返回 PDF 和 `X-Render-Warning-Count`，`X-Render-Warnings` 可缺省。支持 request context 取消，但取消不撤销已提交的其他写请求。 |
| `GET /healthz` / `GET /readyz` | 分别是进程存活和 DB/S3 readiness 探针；未就绪为 503。它们不在十个业务 OpenAPI path 中，但开发代理必须支持。 |

### 3.2 强 ETag 的两种用途

必须在 API client 类型和测试中分开建模：

| ETag | 来源 | 用法 |
|---|---|---|
| item/asset state ETag | `exam.items[].etag`、`exam.assets[].etag`、PATCH 状态响应 | 原样放入对应写请求的 `If-Match`；格式包含 resource ID/revision。 |
| PDF/image content ETag | source PDF 或图片字节响应 | 仅用于缓存条件请求；图片 digest ETag 不能作为 active-variant PATCH 的令牌。 |

缺失/格式错误/资源不匹配的 `If-Match` 是 400；合法但过期是 409。409 可能只在响应头提供当前 ETag，不提供服务器最新文档。前端严禁拿新 ETag 自动重放旧草稿覆盖服务端。

### 3.3 PATCH 字段和清除语义

当前可写字段分为：

- entity：`stem`、`options`、`answer`、`analysis`、`metadata`、`provenance.review_status`；
- occurrence：`display_number`、`score`、`answer_space_mm`。

规则：

- 省略字段表示不修改；字段提交是替换，不是递归 merge。
- `answer`、`analysis`、`metadata` 可按当前契约显式 `null` 清除。
- `options:[]` 是显式清空；`stem:null`、`options:null`、`provenance:null`、`provenance:{}` 和 `review_status:null` 不能由客户端假定为清除操作，P0 对齐后以生成 schema 和实际 handler 为准。
- `metadata` 只提交一个 key 会替换整个 metadata，不得在 UI 中误称为局部 key 更新。
- 不可编辑 `kind`、题树关系、children 结构、顺序、bbox/confidence、asset registry identity、question type。
- 当前实现的 422 可能没有 `details`；未注册图片引用甚至可能映射为 404；前端必须保留表单级/通用错误兜底，不能依赖每次都有可标注路径。

### 3.4 IR、题树、BBox 与 inline

- Exam IR 根必须有 `schema_version/exam_id/metadata/assets/sections`；schema 版本为 `1.0.0`。
- `ContentBlock.type` 是开放字符串；核心类型为 `text`、`math_block`、`image_ref`、`table`。schema 允许开放块附加字段，但当前 4A DTO 不保证这些字段保留，见 P1。
- `sections[].questions[]` 是顶层题；详情 `items[]` 按 section 内 0 起始 `ordinal` 与顶层题对应。children 递归嵌入父题，没有独立 item。
- `provenance.bboxes[]` 的 `page` 从 1 开始；`rect=[x1,y1,x2,y2]` 是归一化 corner 坐标，不是 `[x,y,width,height]`。在与源页同朝向的视图中，矩形为 `(x1*W,y1*H,(x2-x1)*W,(y2-y1)*H)`；不能把它当 PDF 左下原点再翻转 y。
- bbox 可缺，也可能是全页兜底或跨页信息不完整；校对台以辅助定位展示，不承诺人工级精确框。
- inline 权威语义来自 `internal/ir/inline`：`$...$`、`**...**`、`*...*`、`__...__`、`{{blank:N}}`、限定反斜杠转义；错误标记保留可见文本并产生诊断。不要用通用 Markdown parser 扩大语义。

## 4. 工程结构、技术栈与离线资源

### 4.1 工具链和包管理

本机只读检查结果：

- Node：`/usr/bin/node`, `v22.22.1`。
- npm：`/usr/bin/npm`, `9.2.0`。
- pnpm：不在 PATH；corepack 存在但本轮不启用网络、不安装依赖。
- `openapi-typescript`：不在 PATH。
- 当前仓库没有 `web/`、`package.json`、lockfile 或 `node_modules`。

因此 4B 采用 npm，但不硬锁单一点 npm 版本：

- `web/package.json` 使用 `engines` 声明兼容范围，最低兼容线为 `npm >= 9.2.0`，Node 范围由实际 Vite/React 依赖验证后冻结。
- 依赖版本必须精确 pin，提交 `web/package-lock.json`；lockfile 是依赖复现的权威。
- 验证使用 `web/node_modules/.bin` 的本地工具，禁止全局 `openapi-typescript`，禁止验证过程中的 `npx` 隐式下载。

### 4.2 技术栈

- Vite + React + TypeScript，`strict: true`。
- Tailwind CSS，仅用于工作台布局和组件样式，不把内容/布局策略写进 Tailwind 配置。
- `pdfjs-dist`：源 PDF 渲染和页导航。
- `katex`：公式预览，`throwOnError:false`、`trust:false`。
- Vitest + Testing Library：纯离线单元/组件测试。
- API 层使用轻量 `fetch` 封装，不引入大型请求状态框架；请求缓存、编辑 draft 和资源生命周期由明确模块管理。

### 4.3 推荐目录

```text
web/
  package.json
  package-lock.json
  index.html
  vite.config.ts
  tsconfig.json
  tsconfig.node.json
  postcss.config.js
  tailwind.config.ts
  scripts/
    generate-api-types.mjs
    verify-generated-api.mjs
  src/
    main.tsx
    app/{App.tsx,routes.tsx,workbench-state.ts}
    api/{schema.gen.ts,client.ts,errors.ts,decoders.ts}
    domain/{exam.ts,item-patch.ts,layout.ts,assets.ts}
    features/
      catalog/{JobList.tsx,ExamList.tsx}
      ingest/{UploadPanel.tsx,JobStatus.tsx}
      workbench/{Workbench.tsx,PdfPane.tsx,InspectorPane.tsx,QuestionEditor.tsx,AssetVariantControl.tsx,RenderPreview.tsx}
    pdf/{document-loader.ts,page-renderer.ts,bbox-overlay.ts,memory-budget.ts}
    inline/{parser.ts,renderer.ts,diagnostics.ts}
    render/{layout-presets.ts,render-client.ts}
    components/
    test/{fixtures,api,inline,pdf,workbench}
```

`src/api/schema.gen.ts` 只能由生成命令产生，不能手工编辑；业务模块不得散布 `fetch`。

### 4.4 Vite 代理和运行时配置

开发时在 `web/vite.config.ts` 配置：

- `/api` → `VITE_API_PROXY_TARGET`，默认 `http://127.0.0.1:8080`；
- `/healthz`、`/readyz` → 同一 target；
- `VITE_API_BASE_URL` 仅作为部署时可选的公开 base URL，不包含服务端凭据；默认同源。

同源代理优先，避免无必要 CORS。4A 没有 wildcard credentials 契约，前端不自行开启 credentials。

### 4.5 本地 pdf.js 和 KaTeX 资源

禁止 CDN、远程 worker、运行时字体下载和外链图片。实现阶段优先使用同一锁定版本的 Vite URL import：

```ts
import workerSrc from "pdfjs-dist/build/pdf.worker.mjs?url"
GlobalWorkerOptions.workerSrc = workerSrc
```

实际入口以锁定版本的 `pdfjs-dist` 包为准；构建产物必须包含 worker。若中文/特殊 PDF 需要 CMap 或 standard fonts，它们也必须从同一依赖包作为本地产物输出，不能使用远程 URL。

KaTeX 使用包内 CSS：

```ts
import "katex/dist/katex.min.css"
```

字体由 Vite 处理为本地构建资源。产物扫描拒绝 `http://`、`https://`、`//` 外链资源和 CDN 域名。

## 5. OpenAPI 类型生成与强类型 API 客户端

### 5.1 Schema-first 生成

唯一输入为根文件 `../api/openapi.yaml`（相对于 `web/`），生成 `web/src/api/schema.gen.ts`。不复制 `schemas/*.json`，不手写一套近似 IR/Layout 类型。

推荐命令形态：

```text
openapi-typescript ../api/openapi.yaml -o src/api/schema.gen.ts
```

具体 flags 和锁定版本必须写进 `web/package.json` script，并在 lockfile 中固定。`typegen:check` 在临时目录生成后与提交文件字节比较；生成器缺失、网络访问、源文件漂移都必须非零。

候选 `openapi-typescript` 版本必须在无网络隔离环境物理验证：

- 8 个外部引用位置及其 root/fragment 都能解析；
- `$defs` 内部引用以 JSON Schema 文件资源为基准，不错误回到 OpenAPI 根；
- `const`、nullable union、`oneOf`、recursive `children`、开放 block type、二进制响应和 multipart request 都能生成；
- 生成两次字节一致。

若锁定版本不能直接解析 external refs，不得用手工 TypeScript 类型绕过。只能在另行批准后锁定支持该能力的版本，或使用锁定版本、确定性、离线的本地 bundler 后再生成；原始 `api/openapi.yaml` 仍是唯一权威。

### 5.2 API client 责任

轻量 client 统一处理：

- base URL、路径编码、FormData、JSON、Blob/ArrayBuffer；
- `AbortSignal`、超时和请求序列；
- `ETag`、`If-Match`、`If-None-Match`、`Location`、`X-Request-Id`；
- `ErrorResponse` 解析；
- `X-Render-Warning-Count` 和可选 `X-Render-Warnings`；
- Content-Type 和 `%PDF-`/图片字节基本检查。

二进制响应不得尝试 JSON parse；错误先按 Content-Type 尝试 envelope，失败时保留状态码、响应 request id（若有）和安全 fallback。

### 5.3 错误映射

| 状态 | UI 行为 |
|---|---|
| 400 | 显示请求/字段结构错误；不丢草稿。 |
| 409 | 显示“服务端已更新”；保留当前草稿和原 ETag；提供“重新加载最新数据”按钮；严禁静默覆盖或强行重试。 |
| 413 | 上传明确提示超过 50 MiB/请求体限制；JSON/PDF 也显示对应资源过大。 |
| 422 | 使用 `details[].path` 能定位时标红字段；无 details 或只有 `layout` 时显示表单级错误。 |
| 404 | 区分资源不存在与 variant 不可用；不因资产缺失自动清空题目草稿。 |
| 429 | 显示受限提示，不自动无界重试。 |
| 503 | 显示服务未就绪/渲染不可用；保留用户输入。 |

`error_summary` 仅作为脱敏文本展示，React 文本转义，不写 HTML、不发送外部日志服务。

## 6. 页面、布局和状态流

### 6.1 `/` 上传与任务/试卷工作台

#### 上传

- 拖拽和文件选择都进入同一校验路径。
- 接受 `.pdf` 与 `application/pdf` 作为前端提示条件，但服务端 `%PDF-` magic 是最终裁决。
- 文件大小按净文件保守限制在 50 MiB 以下，并在界面说明 multipart overhead 可能使服务端边界更严格；不声称恰好 50 MiB 一定成功。
- 上传进行中可 abort 本次 HTTP，但不得显示“后台 job 已取消”；若 202 已提交后网络中断，必须允许刷新 jobs 确认状态，不自动重 POST。
- 收到 202 后按返回字段构造 job，并插入列表顶部；随后用 GET job 获取权威状态。

#### Jobs

- filter：`queued/parsing/validating/persisted/failed`；切换 filter 时清除 cursor，重新加载第一页。
- cursor 只由 `next_cursor` 驱动；`null` 表示末页；不把 cursor 当页码。
- 存在活动任务时 2–3 秒受控轮询；页面 hidden、组件卸载、请求 abort、全部任务到达 `persisted/failed` 都必须清理 timer。
- River retry 可能使活动状态回到 parsing；UI 不假设状态单调。
- `persisted` 提供进入校对台；`failed` 显示稳定错误 code、脱敏摘要和该次 GET 的 request id。

#### Exams

- 使用 `GET /api/v1/exams` 游标加载更多。
- 展示 title、subject、created time、item count、`pending_review_count` 角标。
- 不做服务端不存在的全文搜索；若实现快速筛选，只能标注为“已加载结果筛选”。

### 6.2 `/exams/:examId` 双栏工作台

布局约束：

- 顶部 toolbar 固定在工作台内；
- 左右两栏桌面宽屏布局，建议初始 50/50 或 45/55，并支持受控拖拽或固定比例；
- 每栏独立滚动，页面不因长题目整体横向滚动；
- 设置可见的最小桌面宽度，移动端不作为 MVP 支持目标；
- 离开、切题、切卷或刷新前检查脏状态。

状态至少拆分为：

- `examDetail` 与 item/asset metadata maps；
- per-item `draft`、`dirtyFields`、`saveState`、`conflictState`；
- selected root item、selected child path（只读）；
- source PDF loading/document/page/render state；
- asset image URL state；
- render request sequence、AbortController、PDF object URL；
- polling/visibility teardown handles。

## 7. 左栏：源 PDF 与 BBox 溯源

### 7.1 PDF.js 加载

1. `fetch(/api/v1/exams/{exam_id}/source-pdf, {signal})` 获取完整字节。
2. 要求 HTTP 200、PDF MIME 和 `%PDF-` magic；不把 URL 交给 pdf.js，避免其尝试 Range。
3. 使用 `getDocument({data: pdfBytes})`；只渲染当前页和少量邻近页。
4. exam 切换、请求取消、组件卸载时销毁 loading task、document、page render task 和 canvas 引用。
5. 限制并发页面渲染、DPR 和高分辨率 canvas 缓存；禁止 `toDataURL()` 复制整页。

### 7.2 页码、缩放和高亮

- page 是 1-based；页码跳转超界时钳制并显示状态。
- 缩放使用 pdf.js viewport；canvas 和 overlay 必须使用同一 viewport transform。
- 题目选中后定位到首个 bbox 页；当前页绘制该题所有 bbox。
- 缺少 bbox、非法顺序、超界或全页兜底时显示温和“仅提供页级定位/暂无精确框”提示，不抛异常。
- BBox 转换集中在 `bbox-overlay.ts`，覆盖：页号、归一化 corner、越界钳制、页面旋转、canvas/CSS 缩放和多框。
- 4A 当前对题目 bbox 的精度不是强保证：可能缺失、全页或跨页不完整；4B 将其定位为辅助溯源，不提供回写 bbox。

## 8. 右栏：题目校对、无损防线与即时预览

### 8.1 导航和进度

- filter：全部、仅 `needs_review`、`provenance.warnings` 非空。
- 复核进度使用当前卷顶层题的实际 review 状态；后端 pending count 按不同根 question 实体统计，不能简单按 children 或 occurrence 相加。
- 题目树递归展示 composite children；child 当前没有独立保存入口。

### 8.2 字段模型

支持展示/编辑：

- `text.markdown`、`math_block.latex`、选项、`answer.short_values`、`answer.detail`、解析；
- `metadata.knowledge_points`、`metadata.difficulty`；
- `provenance.review_status`；
- occurrence 覆盖 `display_number`、`score`、`answer_space_mm`；
- `single` 和 `composite` 的父题结构。

不提供改变 kind、ordering、children 结构、bbox/confidence、资产身份的控件。

### 8.3 表格、未知块与 P1 防误提交

- 表格至少提供结构化只读预览；不要把 flat/nested headers 重新序列化成可能改变结构的编辑器默认值。
- 未知 `type` 或存在未知 type-specific fields 的 block 显示醒目的只读占位和原已知字段摘要；不可进入可编辑表单。
- 若当前题目包含未知块/未知字段，保存按钮必须提示“当前 4A 不保证扩展字段无损保存”，并阻止会替换包含该结构的整个字段的提交；不能静默丢弃。
- P1 完成并通过 GET → 编辑兄弟字段 → PATCH → GET 的原始字段保真测试后，才可放宽该阻断。

### 8.4 Composite 父级编辑安全

MVP 不提供 child 独立 PATCH，但允许按当前 4A 能力编辑 composite 父级支持的字段。父级编辑必须遵守：

- draft 从当前完整题树克隆，保留 `children` 原子子树；
- 提交父题干、父级卷内题号/分值、`review_status` 等字段时，PATCH 只表达已编辑的允许字段，不能把 children 清空、重建丢失或改变；
- 保存前测试确认父级编辑后的本地/响应题目仍拥有逐字节等价的 children（在当前服务端支持形状内）；
- child 的编辑控件在 P1 前为只读，不能伪造 child `item_id` 或使用 display number 作为写目标。

### 8.5 Dirty、保存和 409

- 每个字段独立 dirty；PATCH 只发送显式改变的字段，不发送整个 IR。
- 保存前对 item metadata 的当前强 ETag 做原样 `If-Match`。
- 成功后将响应新 revision/etag/question_id 作为临时更新，并立即重新 GET exam，恢复完整 IR、`is_shared`、assets 和其他并发变化。
- `is_shared=true` 时显示：“该题为共享题库实体，保存将仅作用于当前试卷（写时复制）”。
- 409 保留草稿和 dirty 状态，显示“服务端已更新”；“重新加载最新数据”是显式用户动作，不能用新 ETag 自动覆盖。
- abort/网络断开后的写状态未知；不要直接标记“未保存”，应重新获取 exam 后再决定。
- 切换题目或离开页面时，应用内导航确认；`beforeunload` 只用于浏览器离开兜底。

### 8.6 Inline 与 KaTeX

- 移植/实现独立 TypeScript tokenizer，使用 `internal/ir/inline/inline_test.go` 同批 golden vectors。
- 未闭合 math、非法 blank、未知 markup 保留 literal 并显示诊断，不让预览白屏。
- `katex.renderToString(latex, {throwOnError:false, trust:false})`；异常降级为转义文本和非阻断提示。
- 非公式纯文本只用 React text node；禁止将用户输入拼入 `dangerouslySetInnerHTML`。KaTeX 生成 HTML 仅来自 KaTeX 自身。

## 9. 配图与 raw/clean 变体

### 9.1 展示 URL

浏览器永远使用 `asset_id` 通过 API 代理获取图片，不使用 IR 的 `uri/raw_uri`，不暴露 storage key。

因为 `GET /exams/{id}` 和 active-variant PATCH 都返回精确的 `active_variant`（`raw` 或 `clean`），常规图片展示优先构造显式变体 URL：

```text
/api/v1/assets/{asset_id}?variant=raw
/api/v1/assets/{asset_id}?variant=clean
```

即 `/api/v1/assets/{asset_id}?variant=${asset.active_variant}`。这命中 4A 显式变体的 immutable 缓存，避免 `variant=active` 的 `no-cache` 陈旧/验证负担。`variant=active` 仅作兼容兜底，不作为主展示路径。

切换成功后本地 `active_variant`、revision、ETag 原子更新，图片 URL 自然改变；常规 `<img>` 不使用 Blob URL，不需要为图片额外管理 Blob 生命周期。仅在需要受控读取/校验的特殊测试路径使用 Blob，并及时 revoke。

### 9.2 控件和全局副作用

- `available_variants` 只有 raw 时，显示“仅原图可用”，clean 按钮禁用。
- 双变体时切换按钮带当前资产状态 ETag `If-Match`，不是图片 digest ETag。
- 切换前明确提示：active selector 是全局 `asset_id` 状态，可能影响其他试卷的同一图片。
- 成功后刷新当前 exam metadata，并让所有当前页面引用该 asset 的显式 URL根据 active variant 更新；失败/409 刷新元数据后再决定按钮状态。
- raw-only、legacy raw 无 digest、缺 clean 均按不可用 variant 展示，不把 `ir.assets[].status` 当作 available variants 的唯一来源。

## 10. Toolbar、LayoutSpec 和渲染预览

### 10.1 LayoutSpec

所有预设都生成完整合法对象，不能只提交开关 patch。必填字段：

- `schema_version: "1.0.0"`
- `name`
- `paper_size: a4|a3|b4`
- `columns: 1|2`
- `mode: student|teacher`
- `show_answer`、`show_analysis`、`show_binding_line`
- `option_layout: auto|1x4|2x2|4x1`

可选 `header_text`、`footer_text`。学生 A4、教师 A4、A3 双栏只是前端起点对象，不是服务端 preset 名称或持久化资源。浏览器不复制 Typst 测量/分页逻辑。

### 10.2 Render 生命周期

- 预览和下载均 POST 裸 LayoutSpec；预览使用弹窗内嵌 PDF 视图或 pdf.js，下载使用 Blob。
- 新请求启动前 abort 旧 render；用 monotonically increasing request sequence 丢弃迟到响应。
- 弹窗关闭、重新渲染、exam 切换时 `URL.revokeObjectURL`，并销毁 pdf.js document/loading task。
- 成功检查 HTTP 200、PDF MIME 和 `%PDF-` magic。
- 读取 `X-Render-Warning-Count`；大于零显示非阻断 banner。
- `X-Render-Warnings` 可完全缺省；若存在，按有限 ASCII JSON `{code,path}` 解析并显示摘要，不展示原始 header 文本。
- 预览只代表已保存服务端状态。未保存草稿不通过 4A render 假装预览；应先保存成功，再渲染。

## 11. 安全、缓存和资源生命周期

- 默认同源；不使用外部 CDN、远程 worker、远程字体、任意图片 URL 或 iframe。
- IR、PDF、图片字节不写 localStorage；localStorage 仅可保存可恢复的 per-viewer UI 偏好，并对读写 try/catch。
- React 文本转义；KaTeX `trust:false`；不把 server error、PDF、IR、storage key、S3 endpoint 或完整 request body 写入日志/HTML。
- source PDF 仅缓存当前/邻近页；限制 canvas DPR、页面并发和缓存数量；`destroy()` 在所有卸载/取消/切换路径执行。
- 轮询 timer、visibility listener、AbortController、事件监听、pdf.js tasks 和 render Blob URL 都必须有明确 teardown。
- URL cache 规则：显式 raw/clean immutable；`active` 仅兜底 no-cache；切换使用显式 URL，不向服务端传未定义 revision query 参数。

## 12. 文件变更清单

### 12.1 本轮唯一创建

- `docs/specs/slice-4b-web-workbench.md`（本文件）。

### 12.2 后续 4B 实现阶段

- `web/package.json`、`web/package-lock.json`、Vite/TypeScript/Tailwind 配置和 `web/index.html`。
- `web/src/api/schema.gen.ts`、API client/errors/decoders、领域状态、页面、组件、pdf/inline/render 模块和测试。
- `scripts/verify-web.sh`；根 `scripts/verify.sh` 只有在 `web/node_modules` 和 lockfile 都存在时才可选调用 Web gate，否则保持纯 Go 快速门禁通过。

### 12.3 P0/P1 后续后端变更

- P0：`api/openapi.yaml` 已知漂移和 `cmd/openapicheck` external fragment 校验；由独立变更完成后才开始 4B 编码。
- P1：未知 block raw JSON 保留和 composite child 独立写契约；不阻塞 4B MVP，不在本 Spec 阶段实现。
- 任何实现阶段受影响的 `docs/subsystems/*.md` 必须按 `AGENTS.md` 更新；本 Spec 阶段不修改现状文档。

## 13. 分步实现顺序

1. 完成并审查 P0：对齐 OpenAPI/handler 的 response、null、error 语义；扩展 fragment 校验。P0 不包含未知 block/child 写能力。
2. 创建 npm Web 子工程，精确 pin 依赖并提交 lockfile；离线验证 Node/npm engines。
3. 物理验证 `openapi-typescript` external root/fragment、递归/nullable/binary/FormData 类型和零漂移；失败时不得手写类型绕过。
4. 实现 fetch client、error envelope、request id、ETag、If-Match、AbortController、warning headers。
5. 实现上传、jobs/exams 游标分页、可见性轮询和脏状态路由保护。
6. 实现完整 PDF 加载、页渲染、内存预算、BBox overlay 和 source-page 联动。
7. 实现已知 IR 展示、inline/KaTeX、安全文本、table/unknown block 只读、composite child 只读及父级 children 保留。
8. 实现字段级 dirty PATCH、CoW 提示、成功重取 exam、409 冲突恢复和 422 fallback。
9. 实现显式 raw/clean 图片 URL、资产 ETag 切换和全局影响提示。
10. 实现 LayoutSpec 预设、render 取消、warning banner、PDF 预览/下载和 URL/document cleanup。
11. 新增 Web 离线 gate 和物理构建产物扫描，再执行真实 examserver 浏览器联调。
12. 实现合并时更新受影响 current-state subsystem 文档，不把本 Spec 当作现状证明。

## 14. 离线门禁和物理证伪测试矩阵

### 14.1 `scripts/verify-web.sh`

未来脚本必须纯离线、零 DB/S3/VLM/外网依赖，按顺序执行：

1. 检查 Node/npm 兼容范围、`web/package-lock.json`、`web/node_modules`；缺少 Web 工具时独立脚本失败并清楚报告，不让根 Go gate隐式联网。
2. 使用本地锁定 `openapi-typescript` 执行 `typegen:check`，生成到临时目录并比较 `src/api/schema.gen.ts`。
3. `tsc --noEmit`，`strict: true`。
4. `vitest run`。
5. `vite build`。
6. 检查 dist 中存在本地 pdf.js worker、KaTeX CSS/字体（必要时 CMap/standard fonts），且无 CDN/外链 URL。

根 `scripts/verify.sh` 保持没有 Node、`node_modules` 时的纯 Go 语义；未来仅在 lockfile 和 `node_modules` 同时存在时可选运行 Web gate，不能因前端尚未准备好而破坏 Stage 0 Go 快速门禁。

### 14.2 单元/组件测试

- BBox：归一化 corner → viewport 像素、越界钳制、页码、旋转/viewport、多个框、缺框。
- inline：完整 Go 对照 vectors；未闭合公式降级不抛错；blank、style、转义；`<script>`/HTML 字符作为文本转义。
- API client：multipart、binary、PDF magic、错误 envelope、request id、无 details、409 header ETag、422 path、413、503。
- 编辑：只发送 dirty fields；携带 item If-Match；成功原子更新并重 GET；共享题 CoW 提示；409 不自动覆盖；unknown block/table 阻断；composite 父级 PATCH 不清空 children；children 不伪造 item_id。
- 资产：active metadata ETag 与 image digest ETag 分离；raw/clean URL；raw-only clean 禁用；切换后显式 URL随 active variant 变化；304/immutable 策略。
- render：新请求 abort 旧请求；warning count/ASCII summary；PDF Blob URL revoke；pdf.js document destroy；迟到响应丢弃。
- 生命周期：hidden/unmount 停止轮询；切 exam/关闭弹窗释放 timer、listener、AbortController、document、canvas 和 URL。

### 14.3 物理构建和视觉证伪

至少用一个包含中文、公式和已知源页矩形的真实 PDF：

- 断网启动构建产物，确认 pdf.js worker 和字体可用；
- 检查高分屏 canvas 与 overlay 对齐；
- 检查没有代码形式的数组/未知块内容进入预览；
- 检查表格、公式、CJK 和边界 bbox；
- 对最终 PDF 以 Go Typst 渲染为权威，浏览器预览不能替代最终分页/网格测量。

## 15. 真实浏览器联调验收清单

真实联调必须通过显式 integration 环境提供 PostgreSQL、River schema、SeaweedFS/S3 兼容存储和 `examserver`。默认 `scripts/verify.sh` 不能代替这条链路；不读取仓库 `.env`，只使用操作者显式提供的 integration 环境契约。

1. `POST /api/v1/ingest` 上传 PDF，看到 202、Location 和 job；轮询直到 `persisted` 或验证 `failed` 展示。
2. 进入 exam detail，核对外层 metadata、IR、顶层 item 的 `item_id/revision/etag/is_shared` 和 asset metadata。
3. 下载完整 source PDF，验证 200、页码跳转、缩放、DPR 和题目选择联动；缺 bbox 题显示降级提示。
4. 编辑独占题和共享题，验证 If-Match、成功重取、CoW、question_id/revision/etag；制造 409，确认草稿不被静默覆盖。
5. 编辑 composite 父题题干/题号/分值/review_status，确认 children 未被清空或改变；P1 前 child 控件保持只读。
6. 对 raw-only 资产确认 clean 禁用；对 raw+clean 资产确认显式 URL、If-Match、revision/etag 更新、跨卷影响提示和两变体仍可读。
7. 选择学生/教师/A3 LayoutSpec，预览和下载；快速切换验证旧 render abort、warning banner、URL/document 清理。
8. 断网/刷新/服务未就绪时验证安全错误、request id、503 提示和脏草稿保留。
9. 运行 `scripts/verify-integration.sh` 及浏览器手工清理；不把 fake、golden 只有 raw 的样例或离线 Go gate 冒充双变体/真实闭环通过。

## 16. 本轮检查记录与 Not tried

### 16.1 已检查

- `AGENTS.md`、`LESSONS.md`、`docs/code-standards.md`、`docs/subsystems/README.md`。
- `docs/specs/slice-4a-http-api.md`、`api/openapi.yaml`、`schemas/exam-ir-v1.json`、`schemas/layout-spec-v1.json`、`scripts/verify.sh`。
- `internal/api/api.go`、`routes.go`、`domain.go`、`patch.go` 及 API 测试。
- `internal/ir/`、`internal/ir/inline/`、`internal/store/`、`internal/storage/resolver.go`、`internal/parser/`、`internal/render/`、`templates/exam.typ`、`cmd/examserver/` 和 `cmd/openapicheck/` 的相关实现与测试。
- `testdata/golden/`、`testdata/render/`、`internal/pipeline/dualasset_integration_test.go`、`scripts/verify-integration.sh`。

### 16.2 工具链检查输出

```text
/usr/bin/node --version  -> v22.22.1
/usr/bin/npm --version   -> 9.2.0
pnpm                    -> not found
corepack                -> /usr/bin/corepack
openapi-typescript      -> not on PATH
```

未读取任何 `.env` 文件。

### 16.3 Not tried

本轮只读审阅和 Spec 冻结，以下没有运行，也不能声称通过：

- `./scripts/verify.sh`、`go test`、`go run ./cmd/openapicheck`；
- `openapi-typescript` 生成、`tsc`、Vitest、Vite build；
- 依赖安装、lockfile 生成、网络隔离下的 typegen 物理验证；
- `examserver` 启动、PostgreSQL/S3/River、`scripts/verify-integration.sh`；
- 浏览器手工/自动化验收、pdf.js worker 加载、KaTeX 字体断网验证；
- PDF rasterize/视觉检查、BBox 与旋转页物理对齐验证。

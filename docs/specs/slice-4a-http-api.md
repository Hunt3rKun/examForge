# MVP 切片 4A：HTTP API、OpenAPI 3.1 与常驻 Worker 技术规格

> **状态：已冻结，作为切片 4A 实现依据。** 本文件描述目标设计，不代表相关 API、迁移或服务已实现。切片实现合并时再同步更新 `docs/subsystems/*.md` 现状契约。

## 1. 目标与非目标

### 目标

1. 提供供切片 4B 使用的 Go HTTP API，并以 OpenAPI 3.1 作为唯一对外契约。
2. 复用现有 `Submitter`、River、Store/Assembler、ObjectStore、IR Validator、`TypstRenderer` 与 `S3AssetResolver`，避免在 handler 重写业务流程。
3. 为共享题目校对、资产 raw/clean 选择、HTTP PDF 代理和即时 PDF 预览定义事务、一致性、资源上限与错误映射。
4. 提供同时可运行 HTTP Server 与可选 River Worker 的 `cmd/examserver`，实现 readiness 与有界优雅停机。
5. 保持默认 `scripts/verify.sh` 完全离线；真实 PostgreSQL、River 与 S3 兼容服务仅由显式 integration 门禁使用。

### 非目标

- 不实现切片 4B 的 React/Vite/pdf.js/KaTeX 页面。
- 不提供用户登录、多租户或通用认证体系；该服务是本地/受信网络内部服务，不可直接暴露为公网 API。
- 不做 Word 导出、Typst 主题重做、跨卷蓝图组卷或批量压测框架。
- 不伪造对象存储不支持的 HTTP Range 响应；4A 不承诺 PDF `206 Partial Content`。
- 不自动从 HTTP 请求中选择解析器、提供 VLM 凭据或运行时修改解析配置。

## 2. 现有代码与持久化结构审计

### 2.1 摄取与任务

`internal/pipeline/submit.go` 中的 `Submitter.Submit(ctx, reader, size)` 已执行有界读取、大小与实际字节数一致性检查、`%PDF-` 检查、SHA-256 计算；随后先上传到按 job UUID 决定的 source key，再在**同一 pgx transaction** 内插入 `ingest_jobs`、River job 并写回 River job ID。当前事务失败时 source 对象可能遗留；现有代码没有自动补偿删除。

`ingest_jobs`（`migrations/0003_ingest_jobs.sql`、`0004_exams.sql`）包含：job UUID、source key/digest、可空 `exam_id`、River job ID、`queued|parsing|validating|persisted|failed` 状态、attempt count、受限错误字段和时间戳。River 负责重试时序；领域状态不另造 `retrying`。worker 的 `Work` 复用已有状态机，绝不在 parser 或 S3 调用期间持有 DB transaction。

`storage.MaxSourceBytes` 当前为 64 MiB。4A HTTP 的默认限制为 **50 MiB PDF 文件**，服务配置不得高于既有 64 MiB 系统上限；创建 Submitter 时也要设成同一上限。multipart 总体限制应包含有界 overhead。不能只信任 `Content-Length`。

### 2.2 Exams、questions 与 occurrence

`migrations/0002_questions.sql` 的 `questions` 保存完整 `QuestionItem` JSON、实体投影字段及 `content_fingerprint`/`fingerprint_version`。指纹索引非唯一。`QuestionFingerprint` v1 包含题目语义（包括 `answer_space_mm`），忽略 `question_id`、`display_number`、`score` 和 provenance。Assembler 会在精确内容匹配时复用已有实体，不会覆盖已有实体内容。

`migrations/0004_exams.sql` 的 `exam_items` 是一次卷内题目出现记录，含 UUID `item_id`、section FK、question FK、ordinal、`display_number_override`、`score_override`。一个 question 可被多个 item/多份试卷共享。因此编辑定位必须使用 `item_id`；仅使用共享实体 `question_id` 会编辑错目标或破坏历史卷。

当前 IR v1 的 QuestionItem 含 `answer_space_mm`，但数据库将其存于 question `content_json` 且 fingerprint v1 纳入其含义。为落实本规格的“卷级 occurrence 字段”分治，迁移后 `answer_space_mm` 归属 `exam_items`，Assembler 仍在输出 IR 时把 occurrence 值投影回 QuestionItem。迁移需保持旧试卷渲染结果不变，详见 §6.1。

### 2.3 Assets 与 ObjectStore

`migrations/0001_assets.sql` 的 `assets` 有 `asset_id`、MIME、唯一 `storage_key`、nullable `raw_storage_key`、状态、几何/说明字段、main `sha256` 和时间戳；`0005_asset_raw_digest.sql` 新增 nullable `raw_sha256`。raw digest 有 key 时方合法，但 legacy raw key 可以没有 digest。

slice 3 存储规则：

- `raw_cropped`：main key/digest 指 raw，通常没有 companion，也没有 clean variant。
- `cleaned`：main key/digest 指 clean；raw companion key 和独立 `raw_sha256` 指 raw。
- 不同变体不可覆盖；ObjectStore key 由服务端确定。

`S3AssetResolver.Resolve` 只读注册的 main key/digest，`ResolveRaw` 单独验证 raw companion；renderer 接口 `AssetResolver.Resolve` 目前不支持变体，render-scope cache 当前按 asset ID 缓存。为支持 active 变体需扩展 resolver 与 cache key，不得只加 HTTP selector。

### 2.4 现有可复用接口

- `internal/pipeline.Submitter`：上传 + DB/River 同事务 enqueue。
- `internal/store.Repository`、`Assembler`：事务边界、IR 与关系表装配；`InTx` 为 read-committed，`Read` 为 repeatable-read read-only。
- `internal/ir.Validator`：`ValidateExamDTO`、`DecodeLayoutSpec` 和 `ValidateLayout`；Exam IR 语义校验包含 nested asset reference 检查，资产是否真实注册仍由 store/service 查验。
- `internal/storage.ObjectStore`：有界 `Get`、不可变 `Put`、删除及对象状态查询；`S3AssetResolver` digest 验证。
- `internal/render.Renderer` / `TypstRenderer`：接收显式 `ctx, ExamDocumentIR, LayoutSpec`，返回 PDF artifact 与 warnings；已有进程取消、超时、并发限额和临时工作目录清理。
- `internal/pipeline.NewRiverClient`、`RiverMigrate`、`RiverVersion` 与应用 migration runner；River schema 由 River 自己管理。

## 3. OpenAPI 3.1 契约

### 3.1 单一权威与 schema 对齐

新建 `api/openapi.yaml`，声明 `openapi: 3.1.0`。HTTP DTO 只定义 API 元数据和 envelope；Exam IR 和 Layout Spec 不得在 OpenAPI 中复制定义，而采用 external reference：

- `ExamDocumentIR`：`../schemas/exam-ir-v1.json`
- `LayoutSpec`：`../schemas/layout-spec-v1.json`

路径以 `api/openapi.yaml` 所在目录为基准。离线契约检查必须解析 OpenAPI 文档、解析两个外部引用并检查引用目标存在/可解析；不访问网络。Schema 变更遵循现有 schema-first 规则。OpenAPI validator/TS 生成工具的兼容性需用仓库锁定版本物理验证，不能用手写近似 Schema 替代。

Exam IR v1 保持既有 renderer-neutral 形状。API 的 item revision、item ID、active asset variant 不加入 Exam IR v1；通过外围 DTO 显式传递，避免把数据库/API 状态塞进 IR。

### 3.2 通用错误 envelope 与状态映射

所有 JSON 错误使用统一 envelope：

```json
{
  "code": "validation_failed",
  "message": "Request validation failed",
  "request_id": "...",
  "details": [
    {"path": "sections[0].questions[1].stem[0]", "code": "invalid_reference", "message": "asset reference is not registered"}
  ]
}
```

`details` 可省略；错误 code 稳定、机器可读，错误 message 面向调用方且不含内部信息。`request_id` 总是返回。状态映射：

| HTTP | 用途 | 示例稳定 code |
|---|---|---|
| 400 | JSON/multipart 结构错误、缺必填 part、UUID/参数格式错误 | `malformed_request` |
| 404 | job/exam/item/asset 不存在，或请求的 raw/clean variant 不可用 | `not_found`、`variant_not_found` |
| 409 | If-Match 过期、revision 不匹配、资源状态冲突 | `revision_conflict`、`state_conflict` |
| 413 | body、PDF 或 JSON 超过路由上限 | `request_too_large` |
| 422 | IR/LayoutSpec/字段语义或资产引用校验失败 | `validation_failed` |
| 429 | 达到明确配置的 HTTP 并发/速率上限 | `rate_limited` |
| 500 | 未预期内部故障；泛化 message | `internal_error` |
| 503 | DB/S3 不可用、未就绪、服务关闭或依赖暂不可用 | `service_unavailable` |

存储内部错误、SQL、对象 key、请求内容、堆栈、Typst stderr 和 provider 原始响应不得进入外部错误或访问日志。内部日志只记录安全的分类码与 request ID。

### 3.3 API routes 与数据形状

基础路径为 `/api/v1`。JSON 成功响应采用明确 DTO；数据库行不作为 API DTO。

#### 摄取与任务

- `POST /api/v1/ingest`
  - `multipart/form-data`，必须恰有一个 `file` part，内容为 PDF。其他字段不参与解析配置；拒绝重复/额外文件 part。文件名不得用于对象 key、日志或存储路径。
  - 整个 body `http.MaxBytesReader` 有界；默认文件上限 50 MiB。检查 PDF 起始 `%PDF-`，并复用 Submitter 的精确长度/魔数/SHA 校验。
  - 成功 `202 Accepted`，含 `job_id`、`state=queued`、`created_at`、`updated_at`；`Location` 指向对应 GET job。
  - upload 成功但 DB/River transaction 失败时，使用短时独立 cleanup context best-effort 删除唯一 job source key。原始 enqueue/DB 错误优先返回；cleanup 失败只记脱敏日志/计数，不能覆盖原错。对象不可变及 key 独立于其他 job，删除范围只限本次 job key。
- `GET /api/v1/jobs`
  - query：`limit` 有明确上限；`cursor` 为不透明游标；可选 `state` 状态筛选。
  - `created_at DESC, job_id DESC` 稳定排序；响应 `{items,next_cursor}`。列表不包括 source key、River payload 或原始错误。
- `GET /api/v1/jobs/{job_id}`
  - 返回 job ID/state/attempt_count/可空 `exam_id`/脱敏 `error_code` 与摘要/创建、更新、开始、结束时间。
  - error message 使用已有 `storage.Redact` 思路且再按 API DTO 白名单输出，不回传 URL、凭据、endpoint 或对象 key。

#### Exams 与原卷 PDF

- `GET /api/v1/exams`
  - 稳定游标分页，排序 `created_at DESC, exam_id DESC`。
  - 返回卷头 metadata、section/item/question 摘要、创建时间、待复核题数。待复核数的纳入规则基于 `provenance.review_status` 的明确定义枚举/状态映射，不能用含糊的空值猜测。
- `GET /api/v1/exams/{exam_id}`
  - 返回外围包结构，避免污染 IR v1：

```json
{
  "ir": {"schema_version":"1.0.0", "exam_id":"...", "metadata":{}, "assets":{}, "sections":[]},
  "metadata": {"exam_id":"...", "created_at":"...", "updated_at":"...", "source_sha256":"..."},
  "items": [
    {
      "section_id":"...", "ordinal":0, "item_id":"...", "question_id":"...",
      "revision":1, "etag":"\"item-...-r1\"", "is_shared":true
    }
  ],
  "assets": [
    {
      "asset_id":"ast_...", "active_variant":"clean",
      "available_variants":["raw","clean"], "revision":1, "etag":"\"asset-...-r1\""
    }
  ]
}
```

  - `ir` 是完整 `ExamDocumentIR` external-ref schema；外围 `items` 按 IR 的 section/question 展开顺序提供，一项对应一个顶层 `exam_items` occurrence，必须带 `section_id`、section 内 `ordinal`、`item_id`、`question_id`、`revision`、`etag`、`is_shared`。复合题 children 没有独立 `exam_items` occurrence，保持 parent item 身份。
  - `assets` 旁路元数据数组按 asset_id 排序。仅含变体名称、当前 selector、版本和可供请求条件更新使用的 ETag，不返回 storage key/digest。
  - ETag 为强 ETag，引用不可猜测的 resource ID/revision；revision 是权威并发令牌。客户端将对应 item 或 asset 的 ETag 原样放进 `If-Match`。
- `GET /api/v1/exams/{exam_id}/source-pdf`
  - 服务端从 exam registry 的 source key 读取并代理，不泄露 key 或对象存储 endpoint。
  - 返回 `Content-Type: application/pdf`、`Content-Disposition: inline`、`X-Content-Type-Options: nosniff`、由已登记 SHA-256 构建的 ETag。
  - 当前 ObjectStore 只有完整对象 `Get`，没有 ranged read。4A 使用有界流式回传，不返回伪造的 `206`；Range 请求按完整对象语义处理（忽略 Range 并返回 200）。若未来需 pdf.js 分段加载，另扩展 ObjectStore Range API 或受限 seekable spool，并独立定义一致性、大小和并发限制。

#### 题目复核与共享题目 CoW

- `PATCH /api/v1/exams/{exam_id}/items/{item_id}`，按 occurrence 定位；禁止以 question_id 作为写目标。
- 请求包含字段更新（明确区分 entity 与 occurrence 字段）且必须包含 `If-Match` 强 ETag：
  - entity：`stem`、`options`、`answer`、`analysis`、`metadata`、`provenance.review_status`。
  - occurrence：`display_number`、`score`、`answer_space_mm`。
  - 不允许通过该接口修改 question kind、parent/child 结构关系、exam/section/item 排序、asset registry 身份或其他 occurrence。
- 缺 `If-Match` 返回 400；过期/版本不匹配返回 409，并提供新版本 ETag（不静默 last-write-wins）。成功响应返回更新后 item 与新 ETag。
- 详情见 §6 的事务与 CoW 策略。成功后完整验证重建出的 Exam IR 与 asset registry 引用；语义问题返回 422 且 `details[].path` 指向具体 IR 字段。

#### 资产代理与变体切换

- `GET /api/v1/assets/{asset_id}?variant=active|clean|raw`。缺省 `variant=active`。
  - 根据注册记录选 key，不能信任 IR uri、请求 header 或客户端提供的 key。
  - `active` 使用 selector 当前变体并设置 `Cache-Control: no-cache`；显式 `raw`/`clean` 使用带 digest ETag 的强缓存。变体响应 MIME 只允许注册白名单 image/png、image/jpeg、image/gif、image/webp、image/svg+xml，且读取时验证内容与登记 digest。
  - 所有资产响应设置 `X-Content-Type-Options: nosniff` 和 `Content-Security-Policy: default-src 'none'; sandbox`。额外 CSP 对未来 SVG 内联展示提供脚本沙箱；不允许任意 MIME sniffing。
  - `raw_cropped` 对 `variant=clean` 返回 404 `variant_not_found`。无 raw digest 的 legacy raw companion 也不可服务或选择；digest/key 不成对、对象不存在或校验失败不得返回未验证内容。
- `PATCH /api/v1/assets/{asset_id}/active-variant`
  - body `{ "variant": "raw" | "clean" }`，必须提供当前 asset ETag `If-Match`。
  - 事务锁 asset row；仅当对应 variant 有可验证 key + SHA-256 才能激活。更新 active selector 和 revision，不改写/丢弃任何另一变体的 key 或 SHA。过期 ETag 返回 409；缺失 variant 404。
  - 响应包含 asset_id、active_variant、available_variants、revision、etag，不包含内部 key/digest。

#### 即时 LayoutSpec 渲染

- `POST /api/v1/exams/{exam_id}/render`
  - 请求 `application/json` body 为 external-ref `LayoutSpec`，先限制 body，再经 `ir.Validator.DecodeLayoutSpec`/`ValidateLayout` 做 schema 校验；错误返回 422 + 路径明细。
  - 在一致读快照内 assemble IR 与 active variant state，再调 `Renderer.Render(ctx, exam, layout)`。不写文件、不保存临时 artifact。
  - 成功返回 `application/pdf`、`X-Content-Type-Options: nosniff`、artifact ETag。
  - warnings 通过响应头：`X-Render-Warning-Count` 为实际 warning 数；`X-Render-Warnings` 是可省略的紧凑 JSON 摘要数组，仅含 `{ "code": ..., "path": ... }`。摘要整体设小的严格上限（建议 4 KiB）；超过上限时省略整个摘要，不截断成无效 JSON，count 仍正确。**Header JSON 的 code/path 必须是纯 ASCII**：遇到任何非 ASCII 字符的 warning 字段时，以安全稳定 ASCII code/path 替代或省略该摘要项；绝不将中文、原始 message、stderr 或内部路径写入 header。若不含 warning 则 count 为 `0`，摘要可省略。
  - 取消请求时 context 传至 renderer 与 Typst 子进程，及时终止工作；复用现有 workspace defer cleanup，并测试零临时目录残留。

### 3.4 CORS 与跨源响应头

默认不返回跨源允许头。只有配置了明确 origin allowlist 才允许匹配来源；不支持 wildcard，避免与 credentials 的组合风险。启用 CORS 时：

- 允许方法严格限于实际 API method；允许请求头仅包括 Content-Type、If-Match、If-None-Match、Range（即便 4A 不实现 PDF Range，预检可按配置拒绝 Range）和 X-Request-Id；不启用 credentials。
- 必须设置 `Access-Control-Expose-Headers: ETag, Location, X-Request-Id, X-Render-Warning-Count, X-Render-Warnings`，以供浏览器客户端读取条件更新令牌、上传状态链接与诊断信息。
- CORS 响应不得暴露 S3 endpoint、数据库信息或内部 error detail。

## 4. HTTP 服务、中间件与资源护栏

### 路由实现

使用 Go 标准库 `net/http` `ServeMux` 的 method/path pattern 路由（按项目实际 Go 版本确认支持）；避免新增重型框架。Handlers 只做传输层解码、限额、错误映射与响应编排；业务事务放在可注入 service/store 接口中，利于离线 fake 和 `httptest`。

### 中间件顺序与日志

中间件顺序：Request ID → Panic Recovery → CORS/preflight → route-specific body limit/deadline/semaphore → handler → access log。

- 接受经长度/字符校验的调用方 request ID 或生成随机 ID；统一写 `X-Request-Id` 和 JSON 错误 envelope。
- Recovery 不返回 panic 原文/堆栈给客户端；内部只记录 request ID 与安全 panic category。
- slog access log 只记录 method、route template、status、duration、request_id、字节计数与稳定错误码。绝不记录 Authorization/Cookie/敏感头、multipart 文件名、query 全文、完整请求 body、IR、PDF、对象 key、signed URL 或未脱敏 provider/SQL 错误。
- 不记录 `exam_id`/`asset_id` 除非经过隐私评审且确有诊断需要；默认结构化字段采用 endpoint template 而非实际 path。

### 请求上限、超时与并发

- Ingest 通过 `http.MaxBytesReader` 限制总 multipart body；文件上限默认 50 MiB，校验 single `file` part、无重复 part、`%PDF-` 魔数和精确读取。提交组件再次独立校验。
- JSON patch/layout 限制为配置默认 1 MiB；未知字段、多个 JSON value、错误 Content-Type 视为 400，Schema/语义不合法为 422。
- `ReadHeaderTimeout` 与 `IdleTimeout` 必须非零、显式配置。不要设置会短于上传/渲染时间的全局 `WriteTimeout`。为 upload 与 render 配置各自 deadline 和 semaphore；ctx deadline 之外由已有 S3/Typst 自己的超时进一步收口。过载按 429 或依赖不可用按 503 响应。
- 客户端取消传播至 `ObjectStore.Get` 和 `Renderer.Render`。所有 request/body/response stream 及时关闭；任何中间文件使用 `t.Cleanup` 同等生命周期原则。

### 运维探针

- `GET /healthz` 是进程存活探针，不要求 DB/S3。
- `GET /readyz` 对 PostgreSQL 与 S3 ObjectStore 做有界探测，使用短 deadline。任一依赖失败或服务正在关闭，返回 503 和泛化 JSON/status；具体失败原因只进脱敏日志。
- 启停状态以原子/同步安全的 readiness flag 表示；shutdown 第一动作先置 false。

## 5. `cmd/examserver` 组合与常驻生命周期

### 配置、parser 与迁移

- 所有配置从显式环境变量/受控配置来源读取，不自动 source `.env`。数据库、S3、HTTP bind/CORS、资源上限、ParserMode、worker 开关、超时等启动参数经过验证；秘密不得放 CLI flag、日志或错误文本。
- ParserMode 明确为 `mock|cloud`。cloud 模式在连接 DB/S3 前调用 `cloud.EnvConfig` 校验 `EXAMFORGE_VLM_*` 完整性；mock 模式不触发外部 API。HTTP 客户端不能从请求选择 parser 或带入 provider credential。
- 新增显式迁移运维命令（或使用仓库已有 migrate 运维入口）；examserver 启动时只检查 app migration ledger 与 embedded migration 完全一致（版本、checksum，无 pending），检查 River schema 版本在支持范围，不在多副本 server 启动时自动并发改表。应用迁移与 River 迁移分别归属各自 runner。
- `EXAMFORGE_RUN_WORKER=true|false` 控制是否在此进程运行 River worker。**Worker 可选，但 producer 不可缺**：接受 `/ingest` 必须能在同一 transaction 调 River `InsertTx`。Worker=false 表示只生产队列任务、由其他 worker 节点执行；producer/DB 不可用则启动 fail-fast 或将 ingest/readiness 标为不可用，绝不返回虚假 accepted。

### 优雅停机

监听 SIGINT/SIGTERM。shutdown 使用一个有总 deadline 的 context，按顺序：

1. 标记 `ready=false`，拒绝新的业务工作并让 `/readyz` 返回 503。
2. 调 `http.Server.Shutdown(shutdownCtx)` 停止接入并排空在途请求；deadline 到期时 `Close` 剩余连接并记录超时。
3. 调 `riverClient.Stop(shutdownCtx)`，等待活动 job 优雅完成；在 deadline 后按 River API 语义停止，并确保 worker 不再访问即将关闭的 pool。
4. 关闭 DB pool 和其他生命周期资源；所有 goroutine/信号监听/channel 都有退出路径。

各阶段错误分别保留并聚合，不可因前一阶段失败而跳过 DB 资源关闭；测试重复 shutdown 与 deadline 到期行为。

## 6. 数据迁移、数据一致性与事务策略

### 6.1 Questions：occurrence 字段、fingerprint v2、CoW

实现必须按 schema-first 顺序变更 migration、SQL query、sqlc 生成代码、Assembler/IR 映射和 API service。推荐新增 migration（以当时最后版本号顺延）：

1. `exam_items.answer_space_mm_override double precision NULL CHECK (>=0)`；`exam_items.revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0)`。
2. 对每个现有 item，将所引用 question 的 `content_json.answer_space_mm` 拷贝到该 item override；旧值不存在/为零时按现有 JSON/DTO 语义回填为 0 或 NULL 并在 assembler 中还原为等效 0。迁移前后输出 IR 与 PDF 布局语义必须相同。
3. 将实体 QuestionItem 的 answer-space 从 canonical entity 定义中移出；更新 assemble/serialize/edit，使 `answer_space_mm` 仅从 item override 进入 IR。对旧 question `content_json` 可移除该键或在兼容过渡中忽略它，但读路径不能继续让旧实体字段覆盖 item 数据。
4. `fingerprint_version` 已是行字段且索引按 `(content_fingerprint,fingerprint_version)`，因此使用**版本列隔离**：v1 旧行保持原 fingerprint/version，不重写历史行；新建/编辑实体按 v2 canonicalization 写入 `fingerprint_version=2`。v2 canonicalization 不含 `answer_space_mm`、display_number、score、provenance；明确保留 v1 的其余语义字段规则。查询候选必须同时匹配版本，禁止跨 v1/v2 fingerprint 比较或假定同值表示同内容。
5. 历史 v1 question 在无内容变更时可继续作为历史共享实体读取；校对编辑或显式 v2 upsert 时重新规范化并写 v2。避免全库无必要重算导致不同实体合并。若后续要升级存量实体，必须另有离线 backfill、审计报告与碰撞处理流程，不能混在在线 HTTP transaction。

CoW 编辑事务：

1. 以固定锁顺序锁定 `exam`、目标 `exam_item` 与 question row（具体锁顺序一致用于所有写路径），验证 item 属于路径中的 exam，验证 If-Match revision。
2. 共享判定统计该 question 在全部 `exam_items` 中的引用。对所有可能新增/删除 question 引用的路径（包括 ingest assembler）采用相同的 question row lock protocol；插入 item 前必须按排序 question ID 获取 `FOR UPDATE` 锁，然后插入引用，避免 `PATCH` 在 count 与改写期间漏掉并发 ingest。
3. 共享时基于当前实体字段 clone；只更新 clone 的实体字段并将当前 item.question_id 重指向 clone。独占引用时更新原实体。所有 occurrence 字段只写当前 item；revision 每次成功编辑加一。
4. 更新后的实体计算 fingerprint v2。指纹索引不唯一；按 `(fingerprint,version)` 查候选并解码比较 canonical entity JSON：完全相等时可将当前 item 指向已有实体；同 hash 不同 canonical 内容时保留不同 entity，不以 hash 相等认定语义相同。此选择发生在事务中，不引入新的 unique constraint，也不泄漏唯一约束错误为 500。
5. 在同一 transaction 完整装配 exam，执行 `ir.Validator.ValidateExamDTO`；对资产引用另外校验 registry row 存在且允许使用。任何失败回滚 clone/update/relink/occurrence revision。

请求字段的 null/清除语义必须通过 OpenAPI patch schema 明确：区分未提交字段与显式清空；对于 required 或结构化字段不接受 null 删除；review_status 状态枚举在 OpenAPI 与业务校验中保持一致。

### 6.2 Assets：raw/clean 无损状态机

新增 migration 增加独立变体标识和 selector（具体 SQL 名称由实现保持此语义）：

- raw：`raw_storage_key` + 独立 `raw_sha256`。
- clean：新增 `clean_storage_key` + `clean_sha256`。
- `active_variant` (`raw|clean`) 与用于 HTTP 条件更新的 `revision`。
- 不再让“哪个 key 恰好填在 main storage_key”隐含当前选择；保留现有 `storage_key`/`sha256` 兼容读回填期间及历史记录，只有在所有 reader/writer 已切换后才可另行弃用。

迁移回填：

| 旧行 | 新 raw | 新 clean | 初始 active | 可用性 |
|---|---|---|---|---|
| `raw_cropped` | old main key/digest | 无 | raw | 仅 raw |
| `cleaned` 且 raw digest 有效 | old companion/raw digest | old main/sha | clean | raw、clean |
| legacy `cleaned` raw key 无 digest | key 保留但未验证 | old main/sha | clean | clean；raw 不可服务/切换 |
| inconsistent key/digest pair | 不猜测、不合成 | 不猜测、不合成 | 已验证 main 对应 variant 或标为 unavailable | readiness/审计报告发现；不可服务未验证数据 |

Selector 状态转换：

- `raw_cropped`: `raw → raw`；请求切 clean 为 404。
- `cleaned` 两变体均 digest-verified：`clean ↔ raw`；切换只改 selector/revision，保留两个 key/digest。
- legacy 无 digest raw companion：`clean → clean`；raw 不可选，不可用 clean digest 验 raw。
- resolver 按 variant 读取对应 key/digest并验证；default renderer 选择请求开始时建立的 active snapshot。render cache key 至少包括 asset_id + variant + digest/revision；同一次 render 采用稳定 snapshot，不混用 selector 切换前后的字节。

### 6.3 Jobs/exams 列表与事务一致性

新增 query 使用稳定排序和游标，不以 offset 深分页；筛选/列表查询加适当组合索引需由查询计划与 migration 评估决定。Exam detail、items 旁路元数据、assets 旁路元数据由一个一致 read snapshot 获得。PATCH 与 asset selector 写入使用事务和行锁；外部对象读取/Typst 进程不在 DB transaction 内运行。

## 7. 文件变更清单（实现阶段）

本清单是冻结规格预期的未来实现文件，不代表本次规格冻结同时创建它们：

- 新建 `api/openapi.yaml`、`internal/api/`、`cmd/examserver/`。
- `migrations/0006+`（最终版本号按实现时最后 migration 顺延）、`internal/store/queries/*.sql`、生成的 `internal/store/sqlcgen/*`、`internal/store/` assembler/repository/service。
- `internal/storage/` active variant lookup/resolver/cache，`internal/render/` resolver/variant snapshot integration；必要的 `internal/pipeline/` submitter cleanup/asset reference lock protocol。
- `scripts/verify.sh` 添加 OpenAPI 离线检查及新增离线 Go 测试；integration build-tag 测试扩展端到端 HTTP 生命周期。
- 实现合并时更新相关 `docs/subsystems/*.md` current-state 文档；spec 阶段不修改现状文档。

## 8. 分步实现顺序

1. 以 OpenAPI 完成 DTO、端点、错误码、条件请求和响应头契约；用本地 parser 验证 3.1 与 external refs。
2. Schema-first 设计迁移：exam_items occurrence answer-space/revision、assets 双变体/selector/revision；迁移回填验证与指纹 v2 语义审查。
3. 更新 sqlc queries/models；实现 occurrence/asset 锁、统一引用锁协议、事务 CoW、fingerprint v2 和 variant-aware resolver。
4. 实现 exam/job/asset/render service 与离线接口 fake，先测试事务失败、碰撞、并发与遗留行。
5. 实现 `internal/api` 中间件、错误 envelope、各 route handlers、流式代理及限流/超时；OpenAPI 与 handler response 对齐。
6. 实现 `cmd/examserver` 配置 fail-fast、schema readiness check、worker/producer 模式、signal shutdown。
7. 添加离线 OpenAPI 结构检查、全路由 `httptest -race`、sqlc 零漂移、取消/清理与物理证伪测试，执行 `./scripts/verify.sh`。
8. 通过显式 `scripts/verify-integration.sh` 运行真实 PostgreSQL/River/S3 HTTP 闭环及迁移回填/并发测试；更新 subsystem 现状契约后再合并实现。

## 9. 双门禁验收矩阵

### 离线门禁（零 DB/S3/VLM）

- OpenAPI 3.1 parser 校验、所有本地 external `$ref` 可解析；若新增 migration，`sqlc generate`/`sqlc diff` 无漂移。
- `internal/api` 路由 `httptest` + `-race`：request ID、JSON error envelope/status 映射、panic 恢复、日志字段白名单、CORS preflight/Expose-Headers、上传 413、非 PDF 400、multipart 重复/缺 file 拒绝、DB enqueue 失败触发 source orphan best-effort 清理且主错误不被覆盖。
- Jobs/exams 列表稳定分页与筛选；detail 外围 `items` 顺序和 item_id/ETag/is_shared、assets active/available/revision/etag 与 IR 顺序对应；source proxy 类型与 nosniff。
- PATCH：共享题 CoW 仅重指向当前 item、独占题原地更新、同一卷重复 question 按 item_id 精确编辑、question entity/occurrence 字段分治、answer-space 迁移语义、过期 If-Match 冲突、跨 PATCH 与 ingest 的引用锁竞争、fingerprint v2/命中候选/同 hash 不同内容、非法 IR 或不存在 asset 引用 422 与完整 rollback。
- Assets：raw_cropped clean 404、双变体来回切换不丢 key/digest、legacy raw 无 digest 不可切、ETag/revision 更新、MIME allowlist、digest mismatch 拒绝、`nosniff` + CSP、`active` no-cache 与显式变体缓存策略。
- Render：LayoutSpec 错误路径 422、PDF magic/content type、warning count 与纯 ASCII 有界摘要、取消传递至 renderer、临时 workspace 无残留。
- `healthz`/`readyz` 状态、探测 deadline、安全错误响应；examserver SIGTERM 的 readiness 先降、HTTP/worker/db 次序、deadline、goroutine 无泄漏。
- `./scripts/verify.sh` 必须零 DB、S3、VLM 与外网依赖；本规格仅规定未来实现应扩门禁。

### 显式集成门禁（PostgreSQL + River + S3 兼容对象存储）

以 integration tag 启动真实依赖与进程内 examserver/HTTP 客户端，Mock parser（或本地 httptest parser）运行完整闭环：

`POST /api/v1/ingest` → 轮询 `GET /api/v1/jobs/{id}` 到 `persisted` → `GET /api/v1/exams/{id}` 检查 IR + item/asset metadata → `GET /source-pdf` 验证 `%PDF-` → 对指定 item 带 If-Match PATCH，验证 shared entity CoW、另一份试卷不变、ETag 更新 → 指定资产 active clean→raw→clean，确认 GET active 字节变化且两套 digest/key 都仍可读 → `POST /render` 验证 `%PDF-` 与更新后的内容/active asset 生效。

另外验证 schema migration 对旧 raw_cropped、cleaned、无 raw digest legacy row 的回填；两个事务并发 ingest/PATCH 时引用锁策略实际阻止误判 shared count；启动发现 schema checksum/version 不匹配时 fail-fast；worker=true/false 的生产/消费语义符合规定；停机等待活动 worker。所有测试数据、source objects 与本次新建的对象在 `t.Cleanup` 清理；共享的预存对象不得因单次测试被误删。

## 10. 项目约束与设计风险记录

- 不增加 CGO/transitive CGO 依赖；文档操作继续通过已有 `os/exec` 边界。
- 默认 verify 永不要求服务配置；integration 缺配置必须失败关闭且不能回退到默认 endpoint。
- ETag 是传输形式，DB revision 是权威；时间戳精度不足以替代 revision。
- `answer_space_mm` 的 ownership 与 fingerprint 版本有耦合；按 v1/v2 列隔离，不能重定义 v1 的含义或跨版本去重。
- 题目引用计数只有在 ingestion attach path 与 PATCH 遵守同一锁协议时才可靠；若出现新的 exam item 创建路径，必须加入一致性审计和竞争测试。
- SVG 虽有白名单，也必须同时使用 `nosniff`、CSP sandbox 与字节校验；客户端不得把对象存储地址作为 SVG 来源。
- 本规格不支持 Range；任何实现若无法提供合法 200 有界流，则应返回明确错误，不得错误标注 `206`。

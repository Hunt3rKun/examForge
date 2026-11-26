# MVP Slice 2：持久层、对象存储与离线异步流水线

- **状态：冻结的实现规格（Spec only）**
- **范围：** 本文定义 Slice 2 的目标架构与验收契约；不代表相关代码已实现。
- **架构基线：** Slice 1 Exam IR v1 和 Typst renderer；PostgreSQL、标准 S3 协议、River。

## 1. 目标与非目标

把通过验证的 `ExamDocumentIR` 持久化为可跨卷复用的实体，并通过离线可测试的异步流水线从源 PDF 生成可重建 IR 和 PDF。IR 仍是交换与渲染 DTO，不是数据库模型；数据库按全局资产、独立题目、试卷与编排关系拆分。

实现必须遵守：无 CGO（包括传递依赖）；应用迁移与 River 自有迁移分开；对象存储使用 S3 协议并强制 path-style；Renderer 继续使用 `Render(ctx, ExamDocumentIR, LayoutSpec)`，不改动 TypstRenderer 核心；所有外部 I/O 有 context、大小/并发限制和可清理资源。

### Explicit Non-Goals

- 不做 HTTP API、前端页面或通用多租户/工作流平台。
- 不接真实 VLM/OCR API，不做 `mutool` 切图、HSV 去水印或真实云端解析。
- 不做 Word/DOCX 导出。
- 不启用 `pgvector`，不生成或检索 embedding。
- 不改变 Exam IR v1 Schema 和 Slice 1 Renderer 核心契约。
- 默认离线门禁不连接 PostgreSQL、SeaweedFS 或任何远程服务。

## 2. 当前接口与 IR 边界

`ExamDocumentIR` 的 Schema 定义于 `schemas/exam-ir-v1.json`；其 root 包含 `schema_version`、`exam_id`、`metadata`、全局 `assets` map 和有序 `sections`。Question 可递归包含 `children`；`metadata` 与 `provenance` 保留为 IR 扩展数据。完整校验通过 `internal/ir.Validator` 的 `ValidateExamIR`/`DecodeExamIR` 路径，不可只做 JSON 反序列化或只做 JSON Schema 检查。

Renderer 的资产桥接接口已存在于 `internal/render/render.go`：

```go
 type AssetResolver interface {
     Resolve(ctx context.Context, assetID string, asset ir.Asset) (io.ReadCloser, error)
 }
```

`TypstRenderer` 会通过该接口读取资产，并自行在隔离工作区暂存、验证图片格式与大小；IR 的 `asset.uri` 不能被当作文件系统路径或可信 S3 key。Slice 2 的 S3 resolver 直接实现上述接口，不修改 renderer。

## 3. ER 与关系设计

```text
ingest_jobs 1 ─── 0..1 exams
exams       1 ─── N   exam_sections 1 ─── N exam_items N ─── 1 questions
assets      1 ─── N   questions 中的 image_ref（由 content_json.asset_id 引用）
```

题目可跨卷复用；`exam_items` 保存卷内 occurrence、顺序和覆盖字段。复合题的 `children` 完整嵌在 `questions.content_json`，因此一个 composite question 无需依赖其父卷即可完整读取。任何子题若作为独立题目被抽取，也可以另有 questions 实体并被其他 exam_item 引用。资产 ID 全局唯一，引用校验由 Persist/Assembler 做；IR image_ref 仍使用 `asset_id`。

所有表使用 PostgreSQL `uuid` 作为数据库实体 ID（assets 除外，使用 IR 兼容 `ast_...` 字符串主键）；时间戳为 `timestamptz` 并按 UTC 处理。JSONB 和文本输入仍受应用层大小限制。数据库约束不得取代 IR 语义校验。

## 4. 表结构、字段映射与索引

以下是必须满足的字段语义；SQL 名称及具体 check constraint 在实现迁移中按此规格落地。

### 4.1 `assets`

| 列 | 语义 / 约束 |
|---|---|
| `asset_id text PRIMARY KEY` | 与 IR 同一全局 `ast_[A-Za-z0-9_-]+` ID；应用验证格式。 |
| `media_type text NOT NULL` | 与 IR asset media type 一致；写入时验证支持类型。 |
| `storage_key text NOT NULL UNIQUE` | 当前有效图像对象 key。 |
| `raw_storage_key text` | 原始裁剪对象 key；可空。 |
| `status text NOT NULL` | check 限制 `raw_cropped`、`cleaned`、`vectorized`。 |
| `aspect_ratio double precision` | 正值或空。 |
| `suggested_width_mm double precision` | 正值或空。 |
| `caption text`, `alt text` | 与 IR 可选展示元数据对应。 |
| `source_bbox jsonb` | 可空；IR BBox 对象 `{page, rect:[x,y,w,h]}`。 |
| `sha256 char(64) NOT NULL` | 对实际对象字节计算的十六进制 SHA-256；校验格式。 |
| `created_at`, `updated_at timestamptz` | UTC 创建与更新时刻。 |

索引：唯一 `storage_key`；`sha256` 建**非唯一 B-tree** 查询索引，不以相同内容推断相同资产身份。asset_id 已由主键索引。对象 key 的写入使用确定性 ID/key 规则。若同 `asset_id` 已存在：先比对新旧 SHA-256；相同则幂等放行（元数据必须兼容，或明确执行允许的元数据更新）；不同则返回冲突错误并失败，**禁止静默覆盖数据库记录或已有对象内容**。覆盖 S3 对象之前不得破坏性替换，需先完成内容校验；推荐 immutable content 或临时 key 后原子切换 metadata。

### 4.2 `questions`

| 列 | 语义 / 约束 |
|---|---|
| `question_id uuid PRIMARY KEY` | 数据库生成的稳定实体 UUID，也是 Assemble 写入 IR `question_id` 的值；不沿用输入可选的外部 `question_id` 作为主键。 |
| `subject text NOT NULL`, `grade_level text` | 规范化检索投影；subject 对应 IR exam metadata.subject，grade_level 对应 metadata.grade，且服务端从所属来源上下文提供。共享题目若存在多个来源分类差异，取值策略必须确定（首次写入不覆盖；必要时未来模型扩展）。 |
| `kind text NOT NULL` | `single`/`composite`。 |
| `question_type text` | 对应 QuestionItem.question_type。 |
| `knowledge_points text[] NOT NULL DEFAULT '{}'` | 对应 `metadata.knowledge_points`；按稳定顺序保留，查询使用 GIN。 |
| `difficulty double precision` | 对应 `metadata.difficulty`，范围 `[0,1]`。 |
| `content_fingerprint char(64) NOT NULL` | 服务端对规范化题目语义内容计算的确定性 SHA-256。排除题号/`display_number`、分值、卷头上下文及实体 ID；不得依赖上游可能为空或不可靠的 `metadata.dedup_fingerprint`。算法与规范化版本固定并有测试。 |
| `fingerprint_version smallint NOT NULL` | 指纹 canonicalization 算法版本。版本变更必须有迁移/回填策略，不能静默改变哈希语义。 |
| `content_json jsonb NOT NULL` | 题目 IR 语义内容，见下方映射。 |
| `created_at`, `updated_at timestamptz` | UTC 时刻。 |

`content_json` 是一个可独立重建的完整 `QuestionItem` 内容对象，保留下列 IR 映射：

| IR 字段 | 存储位置与组装规则 |
|---|---|
| `kind` | `questions.kind` 为查询投影，`content_json.kind` 也保留完整结构；写入一致性由唯一 Persist 路径保证。 |
| `question_id` | 输入的可选值不作为实体主键。输出 Assemble 时强制填充该行的 `questions.question_id` UUID 字符串，方便校对台按实体定位。若需审计输入原始值，可在受控 `content_json` provenance/source metadata 中保存，但绝不覆盖输出实体 UUID。 |
| `display_number` | `content_json.display_number` 保留源内容；每卷显示题号可由 `exam_items.display_number_override` 覆盖。Assemble 有 override 时输出 override，否则输出原始值。该字段按题号定义从 fingerprint 排除。 |
| `question_type` | `questions.question_type` 投影 + `content_json.question_type`。 |
| `score` | `content_json.score`；每卷分值可由 `exam_items.score_override` 覆盖；按分值定义从 fingerprint 排除。 |
| `stem`, `options`, `answer`, `analysis` | 作为 IR 结构完整保存在 `content_json` 对应字段。Option label/content 与 Answer.short_values/detail 均保留。 |
| `children` | 完整递归保存在 `content_json.children`；其子题字段也按本映射保留。 |
| `metadata` | 完整保存在 `content_json.metadata`（包含上游 `dedup_fingerprint` 原值）。其中 `knowledge_points`、`difficulty` 是可检索投影，持久化时同步，组装时校验投影一致并输出完整 metadata。 |
| `provenance` | 完整保存在 `content_json.provenance`，不因指纹生成或数据库投影而丢弃。 |
| `answer_space_mm` | `content_json.answer_space_mm` 原样保留。 |

开放类型 `ContentBlock` 可能在未来 Schema 中出现；存储不能将其解码后仅重写已知字段。`content_json` JSONB 保留完整的 block 属性，以避免忽略 IR 可扩展性。题目实体本身不得包含布局策略或二进制资产内容。

索引：`content_fingerprint` + `fingerprint_version` **非唯一 B-tree 索引**（内容相同不等同于实体身份；相同 fingerprint 可有多个实体，需由去重策略显式选择）；`knowledge_points` GIN；必要的 subject/grade/type 组合查询索引按实际查询加入。fingerprint 用于候选查重，不能单独静默合并人工校订状态不同的题目。

**指纹规范：** 服务端生成 canonical question value，按稳定规范化 JSON 编码后计算 SHA-256。输入只使用题目语义（包括 stem/options/answer/analysis/children、kind、question_type 等内容）；明确排除 IR/DB ID、`display_number`、score、provenance、卷头/section 信息和 `metadata.dedup_fingerprint`。`knowledge_points`/difficulty 是否影响语义必须固定为实现常量；本规格定义二者是分类/难度元数据，不进入 fingerprint。测试验证 canonicalizer 的确定性与字段纳入/排除边界。

### 4.3 `exams`

| 列 | 语义 / 约束 |
|---|---|
| `exam_id uuid PRIMARY KEY` | 数据库试卷实体 ID。 |
| `ir_exam_id text NOT NULL` | 原 IR `exam_id`，作为来源/交换标识；不把它误作 question/entity 主键。唯一性策略按来源与应用需求定义，禁止与 job 幂等混淆。 |
| `ingest_job_id uuid NOT NULL UNIQUE REFERENCES ingest_jobs(job_id)` | 单次 ingest job 至多生成一个 exam；重试复用它。 |
| `source_file_sha256 char(64)` | 源 PDF SHA-256。**非唯一 B-tree 索引**，允许不同 job 重复解析同一源 PDF，不把内容相同视作同一任务。 |
| `source_pdf_key text NOT NULL` | 源对象 key，不含凭据。 |
| `schema_version text NOT NULL` | 记录来源 IR Schema 版本。 |
| `metadata_json jsonb NOT NULL` | 完整 ExamMetadata：title、subtitle、subject、grade、total_score、duration_minutes、source_sha256。 |
| `created_at`, `updated_at timestamptz` | UTC 时刻。 |

索引：`UNIQUE(ingest_job_id)`；`source_file_sha256` **非唯一 B-tree**；`ir_exam_id` 索引按查询需求采用非唯一或限定来源范围唯一约束，不得以它取代 job 幂等约束。

### 4.4 `exam_sections` 与 `exam_items`

明确使用关联表，**不使用 `structure_json` 作为编排唯一真相**。

- `exam_sections(section_id uuid PK, exam_id FK, ir_section_id text, ordinal integer, title text, instruction text nullable)`。`UNIQUE(exam_id, ordinal)`、`UNIQUE(exam_id, ir_section_id)`；ordinal 从 0 或 1 起由实现统一并在所有读写中固定（推荐 0 起）。
- `exam_items(item_id uuid PK, section_id FK, question_id FK, ordinal integer, display_number_override text nullable, score_override double precision nullable)`。`UNIQUE(section_id, ordinal)`。该切片只将顶层 IR question 作为 section item；题目 `children` 随 question content_json 递归存取。未来需要单独编排子题时，应新增明确模型，不隐式将当前 JSON children 拆开。
- FK 删除 exam 时级联清除 sections/items；不级联删除共享 questions/assets。资产与题目引用完整性在 Persist/Assemble 检查。重复编排同一问题允许（不同 item occurrence），ordinal 必须唯一。

### 4.5 `ingest_jobs`

`ingest_jobs(job_id uuid PRIMARY KEY, source_pdf_key text NOT NULL, source_file_sha256 char(64) NOT NULL, exam_id uuid NULL, river_job_id text NULL UNIQUE, state text NOT NULL, attempt_count integer NOT NULL DEFAULT 0, error_code text NULL, error_message text NULL, created_at/updated_at/started_at/finished_at timestamptz)`。

- `state` CHECK：`queued | parsing | validating | persisted | failed`；不另造与 River retry schedule 重叠的 retrying 状态。
- `exam_id` 指向生成结果；避免形成不必要双向约束/提交循环，可由 exams.ingest_job_id 查询，或在最终持久化事务中一致填充且通过 FK 保证。
- `river_job_id` 在 enqueue 事务内写入；River job 表不由应用 SQL 迁移建造。
- `attempt_count` 与 River attempt 的同步语义定义为最近 worker 已开始的尝试数；不把错误消息存放凭据、完整 URL 或源文件内容。错误消息限制长度且进行秘密脱敏。
- 索引：`(state, created_at)`、source hash 非唯一查询索引可按需要加入。

## 5. DTO 双向组装：`ExamAssembler`

接口语义（具体 Go 类型可随 store 接口落地，但事务边界不得改变）：

```go
Persist(ctx context.Context, tx pgx.Tx, exam ir.ExamDocumentIR, source SourceRecord) (ExamRecord, error)
Assemble(ctx context.Context, examID uuid.UUID) (ir.ExamDocumentIR, error)
```

1. Persist 先对输入 DTO 执行 `ir.Validator.ValidateExamIR`（或等价完整 schema + semantic 校验）；未经校验不得写数据库。
2. 服务端为每个题目计算稳定 canonical SHA-256 fingerprint，并生成/查找 question entity。相同 fingerprint 可以对应多个实体；复用由明确策略决定，不能用上游 `metadata.dedup_fingerprint` 作为信任依据或唯一键。
3. 在一个 SQL 事务中写 exam、sections、items、questions 关系与 assets 元数据。资产对象必须先已成功上传并经哈希核对。SQL 事务失败则所有关系回滚。
4. Assemble 按 section ordinal 和 item ordinal 排序；还原 ExamMetadata、assets map、sections、题目完整 content_json 与 overrides。递归 children 次序按 JSON 数组原顺序保留。
5. 所有还原的 QuestionItem `question_id` 填为 `questions.question_id` UUID 的规范字符串；原 IR 可选 ID 不作为实体 ID。`display_number`/score override 规则如上。编排次序/卷内覆盖永远由 exam_items 独立持有。
6. 对 JSONB 缺字段/非法类型、缺失资产引用、损坏的顺序/外键或超出 IR 边界的数据返回上下文错误，不返回半成品。重建后的 DTO 必须再次通过完整 `ir.Validator`。
7. 往返比较采用规范化 IR 语义：对象键顺序不敏感；sections/questions/children/options/blocks 数组顺序敏感；metadata/provenance/资产属性不丢失。实体化后的 `question_id` 是规定的唯一差异，比较器忽略原 source question_id 并断言 assemble 值等于数据库实体 UUID。

## 6. 迁移与 sqlc

- 应用 SQL 文件放 `migrations/`，Go 使用 `embed` 编译进二进制；采用轻量有序执行器、schema migration ledger、PostgreSQL advisory lock、版本前后校验。每个迁移尽可能在事务内应用；遇到不支持事务的语句必须单独标注并有恢复说明。
- River 自带数据库对象**必须通过 River 对应版本的 `rivermigrate` API/CLI 管理**。禁止在应用 migration 手写/复制 River 建表 SQL。部署与测试明确先后执行应用 migration 与 River migration，两者版本分别可观测。
- `sqlc.yaml` 配置 PostgreSQL、`pgx/v5`，生成到 `internal/store/sqlcgen/`；查询 SQL 位于 `internal/store/queries/`。生成文件提交到仓库，代码审查关注源码 query 而非手改生成文件。
- 离线门禁在工具可用时运行 `sqlc generate`，随后检查生成目录无 diff；初次接入需明确工具版本。未配置或生成漂移必须失败而非静默跳过。
- 依赖选型门禁检查 Go 版本兼容、直接/传递依赖 CGO；不得引入 CGO driver。River schema 和应用 schema 不得互相维护对方表。

## 7. S3 对象存储与 Renderer Resolver

### 7.1 接口

```go
 type ObjectStore interface {
     Put(ctx context.Context, key string, body io.Reader, size int64, contentType string) error
     Get(ctx context.Context, key string, maxBytes int64) (io.ReadCloser, error)
     Stat(ctx context.Context, key string) (ObjectInfo, error)
     Delete(ctx context.Context, key string) error
 }
```

`ObjectInfo` 至少包含 size、content type、可选服务端 ETag。`Get` 必须在读取达到 `maxBytes+1` 时拒绝超限；调用者关闭 reader；不得无上限读取。Put 要求非负已知 size、上限、类型验证与 context 取消支持。`Exists` 可由 Stat 的 not-found 分类实现，避免两套语义。错误分类区分 not-found、永久 key/权限错误、瞬时服务错误。

`S3Store` 使用 `github.com/aws/aws-sdk-go-v2/service/s3`，基于 endpoint 配置 `BaseEndpoint`，并**强制** `UsePathStyle = true`。Region、bucket、credentials 作为运行时配置注入；不将 Secret 放入 Go 常量、命令行、日志、测试 fixture 或 Spec。不得将任意用户提供 URI 交给 SDK 当 key。

### 7.2 Key 与路径安全

- 上传原文件：`uploads/{job_id}/source.pdf`
- 原裁剪资产：`assets/raw/{asset_id}.{ext}`
- 清理后资产：`assets/clean/{asset_id}.{ext}`

Key 校验拒绝绝对路径、空 segment、`.`/`..` segment、反斜杠、NUL、控制字符、重复分隔符与非规范化编码。ID 先按 IR ID 格式校验，再生成 key；扩展名和媒体类型由 allowlist 映射而来，不接收路径片段。存储对象不允许覆盖不同内容：同 key 已存在时先验证 SHA-256；一致则幂等成功，不一致则报错并保留原对象。对失败流程产生的孤儿对象按 job/asset 前缀可识别和清理。

### 7.3 S3AssetResolver

`S3AssetResolver.Resolve(ctx, assetID, asset)` 实现 renderer 接口，通过服务端资产记录找到有效 `storage_key`，拒绝 URI/key 不一致时的 URI 回退；从 ObjectStore 返回有界 reader。对同一 render 中重复 asset ID 的重复解析，应由 resolver 的有界缓存/renderer 的现有预处理策略保证只下载一次；不得缓存无限资产或跨请求持有未关闭 body。Renderer 现有每资产 32 MiB、总资产 64 MiB 等限制继续有效。对象存储错误需映射为 renderer 可分类的资产错误，不泄漏 credentials、endpoint secret 或带凭据 URL。

## 8. ParserAdapter 与 MockParserAdapter

接口接收 context、源文件有界流/可重复读取的 Source 元信息，返回候选 `ExamDocumentIR` 与待存资产描述/字节流。建议语义签名：

```go
Parse(ctx context.Context, source Source) (ParseResult, error)
```

Parser 不写数据库、不上传对象、不提交 River job。Worker 对 Parser 输出进行完整 IR 校验后，才上传新资产和进入持久化。

`MockParserAdapter` 从 `testdata/golden/ir/sample.json` 与 `testdata/golden/assets/` 加载代表性结果；实现可注入测试 Hook：一次返回非法 IR、一次性瞬时超时、指定重复 fingerprint、可控延迟/取消。测试必须证明错误被正确分类，且不读真实网络/API。golden asset 读取使用项目路径，不把下载 fixture 当可执行代码。

## 9. River 入队、Worker 与状态机

### 9.1 事务性入队

提交服务在**单一 PostgreSQL transaction** 内：

1. INSERT `ingest_jobs` 状态 `queued`；
2. 调用 River `InsertTx(ctx, tx, args, opts)`；
3. 将 River 返回的 job ID 回写 `ingest_jobs.river_job_id`；
4. commit。

任一步失败都 rollback，确保不存在只写业务行或只写队列表的孤儿记录。River 客户端、数据库 transaction 类型必须匹配 `riverpgxv5`，版本/API 以选定依赖的实际签名为准并通过集成测试验证。

### 9.2 Worker 链路

1. 按 job ID 读取任务并通过条件更新 `queued -> parsing`；记录 attempt/start 时间。
2. 从 ObjectStore 有界读取 `source_pdf_key`；不在数据库事务内保持长时间 S3 请求或 parsing。
3. 调 ParserAdapter 得到候选 IR 与资产；推进 `parsing -> validating`。
4. 对 IR 运行完整 `ir.Validator`（Schema + semantic）并验证资产 ID、媒体类型、长度和 SHA-256。
5. 用确定性 key 上传资产；已存在对象需按哈希规则验证，禁止静默覆盖。
6. 在一个短 SQL 事务中 Persist assets/questions/exam/sections/items，并将 job 状态变为 `persisted`、关联 exam；commit 后 Worker 成功返回。

外部 S3 不具备与 PostgreSQL 的共同事务。对象上传先于 SQL commit；SQL 失败可能遗留对象，必须通过确定性 key安全重试和按 job/asset ID 前缀清理来处理，不得声称 SQL rollback 会撤销对象写入。

### 9.3 状态与错误处理

```text
queued -> parsing -> validating -> persisted
   \\         \\           \\-> failed (permanent validation/input failure)
    \\--------- retry by River on transient errors; domain state remains/re-enters active state per documented attempt update
```

允许转移只有 `queued -> parsing`、`parsing -> validating`、`validating -> persisted`、任意未终态 -> `failed`；重复 Worker 开始、终态重入和其他非法边必须被条件更新拒绝或作为幂等完成处理。River 的 retry scheduler 是重试权威；domain `attempt_count` 和最后错误记录反映尝试，暂时失败不冒充 `failed`。

- **永久错误**：JSON Schema/IR semantic validation 失败、不可支持输入、资产内容与声明不符、同资产 ID 不同 SHA。将 `ingest_jobs` 标 `failed` 并记录脱敏 bounded error，调用 River `river.JobCancel` 取消，不将其作为普通可重试错误返回。
- **可重试错误**：对象存储瞬时故障、连接超时、临时数据库不可用等；返回可重试错误交 River 退避。不得把取消/永久输入错误误分类为瞬时错误。
- 若更新业务状态与 River cancel 结果无法原子提交，必须通过可重试的终态同步路径恢复；测试覆盖取消成功、状态更新失败和 worker 重放，不允许永久错误无限重试。

### 9.4 幂等

- `job_id` 是一次上传/解析执行的主要幂等身份；`exams.ingest_job_id UNIQUE` 保证同任务重放复用同一 exam。
- **不同 job 可解析同一源 PDF**：`exams.source_file_sha256` 仅非唯一索引，不合并 job/exam。相同源 SHA 只可作为提示/查询，不构成唯一性或冲突。
- asset_id 冲突严格按 SHA 规则；key 冲突也校验字节 SHA，相同幂等放行，不同失败；不得对已有不同字节对象做静默覆盖。
- question fingerprint 服务端计算；它用于查询候选相同内容，不意味着自动 merge。由持久化策略显式选择复用或新实体；数据库 fingerprint 索引非唯一。
- 编排在同一个 SQL tx 内按目标 exam 替换/upsert sections/items；唯一顺序和 FK 约束保证并发下无重复脏关系。`question_id` 输出取实体 UUID；题号、分值覆盖保存在 occurrence。
- 源文件名不作去重身份；对象命名不含用户控制路径片段。

## 10. 最小离线 CLI 与文件变更清单

Slice 2 最小 CLI/集成入口可采用 `cmd/exampipe`：上传本地 PDF 到 S3、事务性入队、mock worker 消费、落库、Assembler 重建 IR、调用 Slice 1 `TypstRenderer` + `S3AssetResolver` 生成 PDF，并断言输出以 `%PDF-` 开头。真实外部资源入口只由 integration 测试启用；CLI 不接受秘密作为 flag。

预计实现文件（此处为未来变更，不在本 Spec 冻结操作中创建）：

- `migrations/`：应用 schema migrations；`internal/store/migrate/`：Go embed runner。
- `sqlc.yaml`、`internal/store/queries/`、`internal/store/sqlcgen/`、`internal/store/`：生成查询及 repository/Assembler。
- `internal/storage/`：ObjectStore、S3Store、key validator、S3AssetResolver。
- `internal/parser/`：ParserAdapter、MockParserAdapter。
- `internal/pipeline/`：ingest 状态、事务提交服务、River args/worker、错误分类。
- `cmd/exampipe/`：离线演示或操作入口。
- `scripts/verify.sh`：保持离线并增加 sqlc 漂移门禁；`scripts/verify-integration.sh`：仅显式集成测试连接。
- 相关单测和 `//go:build integration` 集成测试目录。
- 实现合并时再同步更新 `docs/subsystems/exam-ir.md`、`render-engine.md`、`pipeline-queue.md`、`vision-parser.md`；Spec 阶段不改现状文档。

## 11. 验证分层与物理证伪测试

### 11.1 快速离线门禁 `./scripts/verify.sh`

保留现有离线验证和 Slice 1 render smoke，加入：`go vet`、`go test -race ./...`（既有脚本使用 `-count=1`）、sqlc 生成无漂移检查、Schema 检查。门禁必须不依赖 PostgreSQL/S3/凭据；sqlc 配置与生成文件缺失时应明确失败。现有 Typst 相关检查继续使用 vendored fonts/packages 与网络隔离。

### 11.2 显式集成门禁 `scripts/verify-integration.sh`

执行 `go test -tags=integration -race ./...`，连接配置仅从环境变量读取，缺失/格式错误即失败，不回退到脚本内默认远程地址。需要 PostgreSQL + River migrations + app migrations，以及 SeaweedFS S3 endpoint、bucket、region 和测试 credentials。该脚本只由用户显式运行，不纳入默认 verify。

环境变量契约使用如下名称（值由操作者安全注入，不提交到仓库）：

- `EXAMFORGE_TEST_DATABASE_URL`
- `EXAMFORGE_TEST_S3_ENDPOINT`
- `EXAMFORGE_TEST_S3_BUCKET`
- `EXAMFORGE_TEST_S3_REGION`
- `EXAMFORGE_TEST_S3_ACCESS_KEY_ID`
- `EXAMFORGE_TEST_S3_SECRET_ACCESS_KEY`
- `EXAMFORGE_TEST_S3_FORCE_PATH_STYLE=true`（实现必须拒绝 false）

**凭据处理：** 数据库 URL 与 S3 secret 属于敏感值；不得把值写入 Spec、代码、命令行参数、测试日志或 Git。环境变量名与读取契约在本文定义；真实值由用户在其受控 shell/secret manager 中自行配置。本轮只创建规格文档，不修改 shell profile、环境配置文件，也不尝试网络连接。

每个集成测试使用独立 PostgreSQL schema 或等效隔离命名空间和唯一 S3 测试前缀；`t.Cleanup` 删除对象、测试行/schema、Worker 与临时文件。任何失败/取消路径均须执行清理；禁止共享固定数据污染复跑。

### 11.3 必须验收用例

**单元测试（零外部服务）：**

1. Assembler Persist/Assemble golden IR 语义往返一致；输出通过 `ir.Validate`；覆盖表格、Math/Text/Image、options、answer/analysis、nested composite、provenance、metadata 和资产引用。
2. `question_id` 被输出为实体 UUID；原始 source ID 不被误当数据库主键；display number/score override 对应 occurrence。
3. Canonical fingerprint 稳定 SHA-256、明确版本；题号/分数/卷头上下文/metadata.dedup_fingerprint 改变不改变 hash，语义块改变会改变 hash；相同 fingerprint 可有多个实体。
4. Assets 同 ID + 同 SHA 幂等；同 ID + 不同 SHA 失败且不覆盖；同 key + 不同 SHA 失败；元数据投影冲突按明确规则拒绝或更新。
5. ObjectStore key traversal/绝对路径/NUL/反斜杠拒绝；S3AssetResolver 不使用恶意 IR URI，重复资产下载去重，最大字节上限、cancel 和 not-found 均正确。
6. 状态机非法转移被拒绝；并发重复终态更新不生成错误脏数据。
7. Mock 的 Schema 非法输出走永久失败与 `JobCancel`，不能被标为普通重试；瞬时故障可重试并成功；失败消息不含 secret。
8. app migration runner 版本顺序/错误/回滚测试；River schema 不出现在应用 migrations。

**集成测试（`-tags=integration -race`，真实 PostgreSQL + SeaweedFS）：**

1. 真实 PostgreSQL 先执行 `rivermigrate` 和应用 migrations；path-style S3 上传源 PDF，执行同事务 job + River 入队，真实 River Worker 使用 MockParserAdapter 消费，落库并 Assemble，通过 `ir.Validate`，S3AssetResolver + TypstRenderer 生成合法 `%PDF-`。
2. 事务中段注入失败并 rollback；查询确认 `ingest_jobs` 与 River 队列均没有孤儿记录。
3. 同一 job 重放两次不增加 exams、不产生脏 assets/重复关系；重复 source SHA 的不同 job 允许各自 exam，验证 `source_file_sha256` 非唯一。
4. 同 asset_id 相同字节重放成功，不同字节内容拒绝且原对象 SHA 不变。
5. 不同卷共享 question entity；section/item 顺序、题号和分值覆盖 Assemble 后准确；question UUID 稳定。
6. 瞬时 S3/数据库错误后 River 重试成功；永久 Schema 失败被 cancel 且终态为 failed。
7. 所有测试以独立 schema/prefix 运行并由 `t.Cleanup` 删除数据库、对象、worker 等资源；重复运行与并发运行无残留。

## 12. 分步实现顺序

1. 依赖/无 CGO 审核，敲定 PostgreSQL、pgx/v5、River/riverpgxv5、AWS SDK v2 与 sqlc 兼容版本。
2. 先添加 schema-first 的 app migrations 与字段/约束/索引；River schema 仅运行 rivermigrate。
3. 配置 sqlc 与查询代码，建立受约束的 store 接口。
4. 实现服务端 fingerprint、资产哈希冲突契约与 ExamAssembler；先完成离线往返/证伪单测。
5. 实现 ObjectStore、S3Store/key validation、S3AssetResolver 和 fake。
6. 实现 ParserAdapter/MockParserAdapter。
7. 实现事务性 River enqueue、worker 状态机、永久/瞬时错误分类、重试幂等。
8. 增加 CLI/demo 与独立集成门禁，运行完整真实资源集成测试。
9. 实现 PR 同步更新受影响 subsystem current-state 文档；不把未来设计倒灌进现状文档。

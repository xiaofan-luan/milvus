# 加法 / 破坏性 Schema 演进(Additive / Destructive Schema Evolution)

- Issue: #50989
- 关系:取代 [20260706-delegator-schema-ready-snapshot.md](20260706-delegator-schema-ready-snapshot.md) 的"过渡模型"。复用其 published-snapshot(RCU)、readiness 握手;替换其"单一 exact-match 写闸门 + 统一读 version-gate",并**移除整个 Part II 的 dropped-field tombstone**。
- 日期:2026-07-10
- 状态:设计提案(社区 PR 前需译英文)

---

## 0. 一页纸

把每个 schema 变更分成 **additive**(加字段/函数)和 **destructive**(drop / 改语义,后者=加新 id + 删旧 id),**永不原地 mutate 一个 live field id**。然后:

- **Additive**:系统对用户**透明**——读写在过渡期都成功,**无部分写、无无谓失败**。靠两件事:①**节点放宽**(超前 vchannel 接受旧版本写并补齐)②**读时补齐**。前提是 rootcoord 暴露的 `SchemaVersion` 只在 broadcast 完成后的 ack 回调里推进,天然 `≤ 所有 vchannel applied`,所以 proxy(含崩溃/重启/扩容的新 proxy)永不超前——**不需要额外的版本闸门**(§6,已验证)。
- **Destructive(drop)**:系统**只保证安全失败**(不崩、不脏、不拉黑),**用户先停用**是契约(§3)。靠 `max_field_id`(id 不复用)+ **读版本-拒绝**(陈旧读引用被删字段 → 干净硬拒绝,InputError、不拉黑,而非 assert;proxy cache 由 AlterCollection ack 回调主动失效、秒级收敛后自然恢复)。**读侧 proxy 重编译透明自愈 = 可选优化、非目标**(§7.1/§15/§17)。
- **去掉 tombstone**:dynamic collection 上"被删 top-level 名字 → 落 `$meta`"是有意语义(**已定接受、归 §3**),dynamic 的名字解析正确性明确甩给用户;struct 子字段严格解析、不受影响(§9)。

---

## 1. 现有模型为什么根上错

当前 PR(#50990)对所有变更用一把闸门(写:`请求版本 == vchannel 版本`;读:`版本 <= 已发布` + 只保 ANN 的 guard)。两类变更安全要求相反:

1. **加字段类 → 跨 vchannel 部分写。** N→N+1 逐 vchannel 传播(非原子)。A 到 N+1 拒 N 请求,B 还 N 接受 N 请求 → 跨 A、B 的一次写只落 B → proxy 判整体失败 → 重试**双写** B。
2. **删字段类 → 陈旧读崩+拉黑。** 陈旧读引用被删 field id → segcore `Schema.h operator[]` 的 `AssertInfo` 抛默认 `UnexpectedError(2001)` → 通用 `ErrSegcore`(非 inputError、非 retriable)→ `lb_policy` 命中 `else` **拉黑健康 shard leader**(已核实;只有 ANN 字段被现有 guard 拦成 InputError)。

## 2. 核心原则 & 五条不变量

> 每个变更表达成 **additive 加** 或 **mark-delete(drop)**;改类型/语义 = 加新 id(additive)+ 删旧 id(destructive)。**永不原地 mutate live id 的数据或语义。**

**I1(地基).** DDL 层**结构性不可达**"原地改一个 live field id 的语义"(类型、nullable、default、analyzer、element_type…)。这是读路径"字段全在就服务"正确性的前提——见 §8。**不是约定,是必须在 DDL 接口层堵死的不变量。**

**I2(写侧不超前).** proxy 永不超前于任何 vchannel。**天然成立、无需额外机制**:rootcoord 暴露的 `SchemaVersion` 只在 broadcast 完成后的 ack 回调里推进(§6 已验证 V1/V2),所以 `SchemaVersion ≤ 所有 vchannel applied` 恒成立。

**I3(id 单调).** `max_field_id` 单调;dropped id 永不复用;re-add 拿新 id → 旧列数据永不被重解释。

**I4(读不崩不拉黑).** 读引用当前 schema 没有的 field id → 干净拒绝(不是 assert),不拉黑。见 §7.1。

**I5(destructive 契约).** destructive 变更系统只保证**安全失败**;**drop 前先停用是用户责任**(§3)。

## 3. 责任边界

| 类别 | 系统保证 | 用户责任 |
|---|---|---|
| **Additive** | 完全续跑 + 安全:读写过渡期都成功,**无部分写、无无谓失败**,透明。 | 无 |
| **Destructive** | **只保证安全**:不崩、不脏、不拉黑;掉队引用干净失败(重编译后 "not found" / dynamic 落 `$meta`);id 不复用。 | **先停用**:drop / 破坏性改语义前,停止发引用该字段的请求。 |

道理:加字段的部分写是用户躲不掉的系统缺陷,系统必须兜;drop 用户明知在删、理应已停用,系统只硬化底层失败模式。

## 4. 版本模型:只比上一步

rootcoord 的 `SchemaVersion` 既是内部版本、也是 DescribeCollection 暴露给 proxy 的版本(proxy 拿它盖读写请求的版本戳)——**不需要区分内部/暴露两个版本**,因为它只在 broadcast 完成后的 ack 回调里推进,天然满足 I2(§6)。

同 collection 的 DDL 串行(resource lock)+ 每次变更让 proxy 收敛后下个 DDL 才发生 → 请求正常最多落后**一个版本**。节点只需保留:当前 schema + 上一个版本号 + 上次变更的 `change_class` + 那一步的字段 diff。**无 `additive_since` 累积。** `change_class` 在 DDL commit 时由 **rootcoord**(算 old/new schema diff)盖到版本上,带 **fail-safe 默认:识别不了的一律 destructive**。

## 5. 分类矩阵(逐情况)

术语:**旧/新** = 按变更前/后 schema 编译;**补齐** = 读时对缺字段的旧行填 null/default/`{}`。物化方式(inline 算 vs 延后 backfill)与兼容类别正交,不改变分类。

| 变更 | 类别 | 旧写(过渡) | 旧读(过渡) | 新写 | 新读 |
|---|---|---|---|---|---|
| 加 nullable/default 列 | additive | 接受;新列物化 null/default | 正常 | 写新列 | 服务;旧行补 null/default |
| 加 nullable struct/子字段 | additive | 接受;子字段物化 null | 正常 | 写入 | 服务;旧行补 null |
| enable dynamic | additive | 接受;`$meta` 物化 `{}` | 正常 | 写 `$meta` | 服务;旧行 `$meta`=`{}` |
| 加 function output(本地/远程 runner) | additive | 接受;output 物化随 runner 局部性(§10) | 正常 | 写入 | 服务;旧行 output 由 backfill 渐进补 |
| drop 列/struct/function output | destructive | 上次 destructive → 拒绝+重编译;仍可能与滞后 vchannel 部分写(§3) | 引用被删字段 → delegator 拒绝 → 重编译;不引用 → 正常 | 无此字段 | 不引用;旧行被删列不可见,compaction 回收 |
| disable dynamic | destructive | 拒绝+重编译 | 引用 `$meta` → 拒绝+重编译;不引用 → 正常 | 不能写 `$meta` | 不暴露 `$meta`;历史 `$meta` 不可见 |
| 改类型/语义、改 BM25/MinHash signature | 加新 id(additive)+ 删旧 id(destructive) | 提供旧 id → 拒;新 id → additive | 引用旧 id → 拒+重编译;新 id → additive | 写新 id | 读新 id;旧行新 id 值由迁移 job 补 |

> **表中 destructive 读的"+重编译"= 可选透明自愈**(§7.1 ②③,非目标);不启用时为**干净硬拒绝**(InputError、不拉黑、可重试),安全性不变,proxy cache 秒级收敛后按新 schema 自然生效。
> **"drop 后同名 re-add" 不是一类**:= drop(destructive)+ add(additive)。`max_field_id` 给 re-add 新 id;旧 id 引用被拒+重编译;新读原子解析到新 id。
> **同版本 property/语义变更(TTL、mmap)是第三类**:不加不删字段,刻意不 bump 版本,走 §12 的 `(version, barrierTs)` readiness,不进 §6 写闸门。**注意 TTL 改的是行可见性——是 I1 的一个受控例外**,需在 §12 显式处理(否则 §7 纯版本读服务看不到它)。

## 6. 写路径:节点放宽(消灭加字段类部分写)

**关键前提(已验证,I2):`rootcoord 暴露的 SchemaVersion 本身就 ≤ 所有 vchannel 的 applied version`,不需要额外的版本闸门。** 因为 `SchemaVersion` 只在 `ApplyUpdates`(`meta_table.go:1062`)推进,而它**只被** `alterCollectionV2AckCallback`(broadcast 的 ack 回调,`ddl_callbacks_alter_collection_properties.go:457`)调用;ack 回调在 broadcast 完成之后才触发(V1),而 broadcast 完成 ⟺ 所有 vchannel 已 append+apply(V2)。所以 **SchemaVersion 变成 N+1 时,所有 vchannel 早已是 N+1**。DescribeCollection 返回 SchemaVersion,任何 proxy(含崩溃/重启/扩容的新 proxy,它走 DescribeCollection 直取 SchemaVersion)拿到 N+1 时都不可能超前于任何 vchannel。→ **"新 proxy → 滞后 vchannel"这半天然不存在,无需 W-gate。**

**节点放宽**处理另一半——broadcast 传播途中,某些 vchannel 已 N+1、而所有 proxy 还在 N(SchemaVersion 尚未在 ack 回调里推进),写 `insert(N)` 打到超前 vchannel。每节点(streamingnode `checkIfCollectionSchemaVersionMatch`)用自己当前 schema + 上一步:
- `请求版本 == 当前` → 接受。
- `请求版本 == 上一个`:上次 **additive** → 接受(闸门返回当前版本 N+1);上次 **destructive** → 拒绝 + 重编译。
- 更旧(≥2)→ 拒绝 + 重编译。
- 写请求引用被 mark-delete 的字段 → 拒绝(§9)。

**补齐无需写新代码(已验证):** 接受一个缺新字段的 `insert(N)` 后,**下游既有机制自动补齐**——segcore 的 growing segment `Insert`(`SegmentGrowingImpl.cpp:666`)对缺失的 nullable/default 字段填 null/default(注释即为"insert used old schema, segment has latest"这个场景),querynode/streamingnode 只透传;函数 output 由现有 `materializeFunctionFields(N+1)` 在写时算(本地 runner)或留空由 async backfill 补(远程)。additive 加的字段恒为 nullable/default/function-output,segcore 只对"非 nullable 无 default"才 assert,不中招。**所以 streamingnode 侧只需放宽闸门 + 记录上一步分类,零 backfill 代码。**

**为什么加字段类无部分写**:传播窗口内所有 proxy 看到 N、写 `insert(N)`;超前 vchannel(N+1)靠放宽接受+补齐,滞后 vchannel(N)匹配接受 → 两边都接受。窗口结束(ack 回调推进 SchemaVersion 到 N+1,此时所有 vchannel 已 N+1)后,proxy 才看到 N+1,无 lag。补齐确定性,两 vchannel 的 WAL 合法不同但读/replay 收敛。

> **残留(需限定)**:"additive 消灭部分写"只在"最多落后一版"成立时为真——两次 additive 极快连发、请求落后 ≥2 版时,A 在 N+2 拒、B 在 N 接受,仍可能部分写(窄,受 DDL 速率约束)。本节成立依赖 apply-at-append + "ack 回调在 broadcast 完成后触发"——**已验证**(§17 V1/V2)。

## 7. 读路径

delegator 拿读(编译版本 v = 该 proxy 的 W),对照已发布版本 P:
- **v == P** → 热路径,直接服务,不解析 plan。
- **v > P** → shard 落后,not-ready 重试(既有 gate)。
- **v < P** → 冷路径,解析 plan **全部字段依赖**(filter/output/group/order/aggregate、Query/QueryStream、delete-by-expr、struct 子字段、非 ANN):
  - 依赖 id **全在** P → 服务。**正确性依赖 I1**:present 的 id 语义不变,所以按 P 服务 = 按 v 服务。
  - 有 id **不在** P → 干净硬拒绝(§7.1 ①,`FieldIDInvalid`、不拉黑);**[可选、非目标]** proxy 重编译透明自愈(§7.1 ②③)。

物理删列 compaction 全 lazy、与读无协调——能引用被删列的读都先被拒。单版本 worker 足够:destructive 读靠**拒绝**保证安全,不靠服务旧数据。

### 7.1 绝不拉黑:一层核心(已达成)+ 两层可选优化

根因是抛错分类错(缺字段走默认 `2001` → 拉黑)。**一层核心保证安全,两层可选给透明:**

- **① Segcore 地板(核心、无条件、已实现,§17 PR-0)。** 在 segcore **读执行入口**(plan 创建、解析前)显式校验:plan 引用的 id 缺 → `ThrowInfo(FieldIDInvalid=2020)`(`merr` 已归 `inputError`)→ `lb_policy` "是 InputError 就早返回、**不拉黑**"。**这一层单独就达成整个方案的安全目标(不崩、不拉黑)**,不依赖任何 plan 遍历。⚠️ 只在读入口显式校验后抛,**不要全局改 `operator[]` 默认 code**。
- **②③ 透明自愈(可选优化、非目标 — 见 §15/§16/§17)。** ② delegator `v<P` 冷路径缺字段 → 返回可重试的"版本不匹配、请重编译"错(区别于 110"shard 落后、等一等");③ proxy 识别后失效 cache → 重拉 schema → 重编译 plan → 重试至收敛(真删 → 干净 "not found" / 落 `$meta`;drop+re-add → 解析到新 id,透明成功)。**目的仅是把过渡窗口内的一次硬失败磨平成透明重试,不改变安全性。**

**分工**:① **单独、无条件保证安全(不拉黑/不崩)**;②③ 只加"透明度"。**不做 ②③**:destructive 陈旧读引用被删字段 = 走 ① 干净硬失败(InputError、不拉黑),用户重试 + proxy cache 秒级主动收敛后恢复——**已评估可接受、定为非目标(§15/§17)**。

> ⚠️ **②③ 全或全无**:③ 的"proxy 失效+重拉+重编译+重试"现状没有(`PreExecute` 只编译一次、`lb_policy.ExecuteWithRetry` 复用同一请求同一版本戳)。**只做 ② 不做 ③** → 一个 `v<P` 可重试拒绝让 proxy 换节点**拿同一 v 无限重试**(重试风暴,比只有 ① 更差)。所以要么一起做、要么都不做——**当前(2026-07-11)定:都不做,只保留 ①**(§17)。

## 8. 去 tombstone:靠什么扛正确性

**不引入 `dropped_fields` tombstone。** 正确性由三件独立的东西保证:**`max_field_id`(I3)** + **读版本-拒绝(I4/§7)** + **写侧 `SchemaVersion` 天然不超前(I2/§6)**。tombstone 原本只为"dynamic 上被删名字静默回落 `$meta`"——这件事按 §3 甩给用户。

名字解析回到既有、不依赖 tombstone 的规则:live 字段(含 struct 子字段)命中即返回;不命中且 dynamic → 落 `$meta`;不命中且非 dynamic → "field not found"。**保留** `verifyDynamicFieldData` 既有的 static-name 检查(拒绝 `$meta` key 撞 live 静态名)。

## 9. dynamic + `$meta`:已决定接受,归 §3

**决定:纯去 tombstone,不打补丁。** 去 tombstone 后,dynamic collection 上一个被删的 **top-level** 字段名,再被引用会**静默解析到 `$meta`**(和任何未知裸名字一样)——这可能返回该名字在 `$meta` 里的历史数据、或让 delete 打到 `$meta` 匹配的另一批行,**无报错**。这一族"静默解析对了、结果可能不对"的行为**明确接受、归 §3(用户 drop 前先停用)**;§7.1 的三层网只保"可用性(不拉黑)+ 非 dynamic 正确性",不覆盖它(没有 id 缺失,不触发任何一层)。

**范围已核实收窄**:
- **只在 dynamic collection 的 top-level 字段**。裸标识符走 `parser_visitor.go` 的 `translateIdentifier` → `GetFieldFromNameDefaultJSON` → 不是静态字段就静默落 `$meta`。
- **struct 子字段不受影响**:只能用显式语法 `docs[text]` / `$[text]`,解析走**严格的 `GetFieldFromName`**(`VisitStructField`/`VisitStructSubField`),缺就 loud "struct field not found",**从不落 `$meta`**。所以上一版的 SubField-tombstone 本就多余。
- **非 dynamic collection**:被删/未知名字直接 "field not found"。

**无 corruption 已验**:被删列(旧 id)数据 pre-GC 不被任何读访问(陈旧读被 §7 拒);id 不复用(I3);`$meta` 撞 live 静态名有独立检查(保留)。唯一残留是上面 dynamic + top-level 的静默名字解析,已决定归 §3。

> 若将来想收掉这个静默:让 top-level 裸标识符也走**严格解析**(对齐 struct 已有行为)、动态键一律显式 `$meta["x"]`——一个 parser 层改动,仍不需要 tombstone。当前**不做**。

## 10. 函数

- **本地 runner(BM25/MinHash)**:加=additive。稳态新写 inline 从输入算 output;**过渡窗口滞后 vchannel 上写的行、以及变更前旧行,同走 backfill**(它们从该 vchannel 视角是"apply 前的行")——所以 §6"读/replay 收敛"对 output 列靠 backfill 成立,不是 inline。drop/改 signature 走 加新 id + 删旧 id。
- **远程 runner(TextEmbedding)**:加**仍是 additive**——只是 output **不在 WAL append 路径同步调远程**(会把摄入绑死外部 endpoint,同 §11 crash-loop),一律延后 async backfill。function-add 的 readiness 需含**写路径 runner 就绪**(仅对本地 inline 相关;远程只需 backfill job 就绪)。

## 11. 失败隔离(独立于过渡模型)

function-runner 初始化失败**不能**杀多租户节点。现状 `delete_node.go` 调 `mlog.Fatal`(=`os.Exit(1)`,注释却写 "panic"),pipeline 无 `recover()` → 某 collection 不可达 endpoint crash-loop 整个 querynode。正确:**冻结/隔离出问题的 collection/vchannel**(停其 tsafe、继续服务旧 published snapshot、readiness 报 not-ready),其余照常。

## 12. readiness 握手

- **cache 过期**(cached proxy 看到 N+1)gate 在 **read-readiness 之后**:ack 回调里 `waitCollectionSchemaReady`(所有 delegator published)通过才 `ExpireCaches`。写侧的"所有 vchannel applied"由 ack 回调的触发时机天然保证(§6/I2)。fresh proxy 走 DescribeCollection 直取 `SchemaVersion`——写安全(所有 vchannel 已 N+1),读若 delegator 未就绪则命中既有 `v>P` not-ready 重试。
- **同版本语义变更(TTL)**:刻意不 bump 版本 → 握手必须携带并比较 **`(version, barrierTs)`**,否则旧快照瞬间满足、cache 在 barrier 应用前过期。且 §7 的读服务需要一个 barrierTs 维度或 delegator 执行时按最新 barrier 重算——**这是 I1 的受控例外,需专门处理**(TTL 只改可见性、不改字段身份)。

## 13. 物理回收(GC)

drop 后旧列由 compaction **lazy** 回收,**与读无协调**(§7 版本检查兜住掉队读)。无 read drain、无 GC watermark。代价:回收前继续占存储/索引;drop 罕见,可接受。

## 14. 相对 #50990 增删

**移除(整个 Part II tombstone)**:`dropped_fields` proto 字段 + `DroppedFieldInfo`(**milvus-proto #631 随之不需要**);`pkg/util/typeutil/dropped_fields.go`;`SchemaHelper` dropped-field guard;`verifyDynamicFieldData` 的 dropped-field 拦截(**保留** static-name 检查);rootcoord 写 tombstone + 清除逻辑;`model.Collection.DroppedFields` + 持久化 + `kv_catalog` 拷贝。

**保留/复用**:`max_field_id`;published-snapshot(RCU);readiness 握手(加 `barrierTs` + 写侧);读 version-gate → 推广成全字段安全拒绝。

**新增**:版本 `change_class` + fail-safe 分类器(rootcoord);写侧 additive 放宽 + 补齐(§6);**segcore 缺字段抛错重分类(①,已实现+远端编译通过)**;失败隔离;`barrierTs`。**可选优化(非目标,§15/§17)**:delegator 冷路径全字段解析(②)+ proxy 重编译-重试路径(③,当前不存在)。(~~W 版本闸门~~ 已确认不需要,§6/I2。)

## 15. 非目标 / 契约

- DDL 间不做 snapshot isolation。
- **drop 前先停用是用户责任**;destructive 只保证安全失败,不保证透明。
- **读侧 proxy 重编译-重试透明自愈 = 可选优化、非目标**(2026-07-11 定)。destructive 陈旧读引用被删字段默认**干净硬失败**(InputError、不拉黑、安全),而非自动重编译;proxy cache 由 AlterCollection ack 回调主动失效、秒级收敛后自然恢复。见 §7.1、§17。
- 不引入单请求跨 vchannel 原子性;additive 靠 W+放宽绕开,destructive 是拒绝而非部分应用(部分写属 §3)。
- **dynamic 上 drop top-level 名字后引用它 = 落 `$meta`**,不 loud 报错(**已定接受、归用户**;§9)。struct 子字段严格解析、非 dynamic 直接 "field not found",均不受影响。

## 16. 落地排序(多 PR)

0. **止血**:segcore 读入口缺字段 → 抛 `FieldIDInvalid(2020)`(§7.1 ①)。几行、无依赖、独立先合,直接消掉当前 PR 拉黑。**[已实现 + 远端 build-cpp 通过,2026-07-11]**
1. 版本 `change_class` + fail-safe 分类器(§4);写侧 additive 放宽 + 补齐(§6)——消灭加字段类部分写;**移除 Part II tombstone**(§14)。**[已实现 + 远端 build/UT 通过,2026-07-11]**
2. readiness `barrierTs` + TTL 处理(§12);失败隔离(§11)。
3. **(可选优化、非目标、非阻塞)** proxy 重编译-重试组件(§7.1 ③)+ delegator 冷路径全字段解析(§7 ②)——destructive 读透明自愈。仅在需要磨平过渡窗口硬失败时做;**安全性不依赖它**(§7.1 ①/§17)。**② 与 ③ 全或全无。**

## 17. 实现状态 / 决定 / 剩余 / 风险

**已实现 + 验证(2026-07-11)**
- **PR-0 ✓ segcore 止血**:`PlanProto.cpp` 在 `CreatePlan`/`CreateRetrievePlan` **解析前**先 `CollectAccessFieldIDs`(ANN、predicate 列、group-by、order-by、aggregate、output 全覆盖)逐个 `CheckPlanFieldPresent`,缺则 `ThrowInfo(FieldIDInvalid=2020)`(跳过 system 字段)。**关键修复(review 发现)**:早前把校验放在 `PlanNodeFromProto` **之后**,filter/predicate 字段会先在 `ParseExprs → Schema::operator[]` 抛默认 `UnexpectedError(2001)` → 校验对主场景(引用被删字段的过滤谓词)**失效**;移到解析前后覆盖全部引用点。G1 链路验过(2020 经 `plan_c.cpp:75` 原样传出 → InputError → 不拉黑)。新增 2 个回归测试(`SearchPlanRejectsDroppedPredicateField`/`RetrievePlanRejectsDroppedPredicateField`)锁死"谓词引用被删字段 → 2020 而非 2001"。**远端 build-cpp + PlanProto 8/8 通过。**
- **PR-1 ✓ 写侧 additive 放宽 + 分类器**:`CollectionInfo` 加 `PrevSchemaVersion`/`LastChangeAdditive`;`isAdditiveSchemaChange` 保守分类;`checkIfCollectionSchemaVersionMatch` 放宽接受上一版本 additive;调用方无需改(`materializeFunctionFields` 已用返回的当前版本)。**函数比较用 `proto.Equal`(review 发现)**:早前只比 `OutputFieldIds`,会漏"原地改函数 input/params(如 embedding model 换端点)但保持 id+output 不变"这类 destructive 变更;`proto.Equal` 覆盖 input/output/params/type。**补齐零新代码**——segcore growing `Insert` 既有 backfill 处理缺失 nullable/default。**远端 build + 单测(含新增 in-place 改函数 case)通过。**
- **移除 tombstone ✓**(P1):删 `dropped_fields.go`、`model.DroppedFields`、SchemaHelper/`verifyDynamicFieldData` guard、rootcoord 写点、3 个 go.mod replace。grep 零残留,**远端 build(proxy/rootcoord/metastore)+ 单测通过**。
- **W-gate 消除 ✓**:见下 I2——`SchemaVersion` 只在 broadcast 后的 ack 回调推进,天然 `≤ 所有 vchannel applied`,新 proxy 也不超前,不需要额外闸门。
- **B2 ✓ 已满足(零代码)**:AlterCollectionSchema 只有 Add/Drop 动作,**无原地改字段类型/nullable 的 API**;改类型/语义只能 drop-old+add-new(expand/contract,`max_field_id` 给新 id)。I1"field id 在→语义不变"由 API 结构保证。(TTL 改 collection 级可见性、非字段身份,走 §12。)

**产品决定(已定)**
- **P1 = 纯去 tombstone**(2026-07-10)。dynamic top-level 被删名字静默落 `$meta` 接受、归 §3(§9);**milvus-proto #631 不需要**;struct 已是严格解析、不受影响。

**方案前提已验证(V1/V2/I2)**
- **V1 ✓**:`Broadcast` 只在所有 vchannel append 成功后返回(`BlockUntilDone`)。
- **V2 ✓**:schema append 时同步 apply(`handleAlterCollection` 在 `appendOp` 前)。
- **I2 ✓**:`SchemaVersion` 唯一推进点 `meta_table.go:1062` ← ack 回调(broadcast 后)→ `SchemaVersion ≤ 所有 vchannel applied` 恒成立,W-gate 不需要。

**剩余 PR(大、CGO 重,follow-up)**

> **共性(为何都是独立 follow-up,不在本轮合入)**:三者都是**失败模式行为变更**,正确性取决于故障路径行为,按本仓 `CLAUDE.md` 验证门 **G2 必须端到端 trace/故障注入验证**(非单测能覆盖);且各自带**架构分叉**(下列)需定夺。单测能过不等于行为正确——本轮只合入了能被单测/远端 UT 完整验证的 PR-0/PR-1/去 tombstone。

- **PR-2(可选优化、非目标 — 2026-07-11 定不做)**:proxy 重编译-重试(`lb_policy.ExecuteWithRetry` 现复用同一请求同一版本,不重编译)+ delegator 冷路径全字段解析 → destructive 读透明自愈。**决定不做,取舍**:安全(不拉黑/不崩)已由 PR-0 **全达成**;PR-2 只把一个**秒级、自收敛、且属用户违约**的过渡窗口内硬失败磨平成透明重试——proxy cache 由 AlterCollection ack 回调(`ddl_callbacks_alter_collection_properties.go:519-523`,`waitCollectionSchemaReady` 后 `ExpireCaches`)主动失效、秒级收敛。用它换掉**读热路径最高的改动风险**(动 `lb_policy` 重试循环 + 从非幂等的 `PreExecute` 抽幂等重编译)不划算。**② 与 ③ 全或全无**:只做 ② → 重试风暴。若将来要透明,轻量做法:只对 `FieldIDInvalid` 做一次 cache 失效 + 重编译重试,不建完整收敛机制。
  - **已探明的落点(2026-07-11)**:② delegator 侧——`search()`/`Query()`/`QueryStream()` 在 `validateReadSchemaVersion`(`delegator.go:458`,现只挡 `v>P` 返 `ErrCollectionSchemaVersionNotReady`=110)之后,对 `v<P` 解析 `req.SerializedExprPlan`(`planpb.PlanNode`,`segment_pruner.go` 已有 `proto.Unmarshal` 先例)提字段依赖,逐个 `snap.hasField()`(`schema_ready_state.go` 已有 O(1) 查),缺则返**新的、区别于 110 的**可重试错(`ErrCollectionSchemaMismatch`,需新增)。③ proxy 侧——plan 只在 `PreExecute` 编译一次、`lb_policy` 复用同一请求;需**外层有界重编译循环**(失效 cache→重拉 schema→重编译→重试)。
  - **架构分叉(需定夺)**:③ 循环放哪?`processTask`(`task_scheduler.go:558`)是**通用**入口(改动波及所有 task,风险最大);`searchTask.PreExecute` 很长且**非幂等**(`translateOutputFields`/`initSearchAggregation` 会追加/改状态),整体重跑不安全 → 需把"重编译 plan"抽成幂等方法。**② 与 ③ 全或全无**:只上 ② 不上 ③ → proxy 对新错换节点重试同一陈旧 plan = 重试风暴(比只有 PR-0 更差)。
- **PR-3a**:失败隔离(`delete_node.go:123` `mlog.Fatal`——经 `zap.OnFatal(WriteThenPanic)` 实为 **panic**,但 flowgraph/pipeline **无 `recover()`**,panic 上抛**杀整个进程**,重启后 WAL replay 再撞同一失败 → crash-loop → 按 collection/vchannel 冻结、不杀节点)。**架构分叉**:需给 flowgraph 加**每-vchannel 失败隔离**(recover + 冻结:停该 vchannel tsafe、继续服务旧 ready snapshot、readiness 报 not-ready),pipeline 现无 error 传播路径,非一行改动。
- **PR-3b**:TTL 的 `(version, barrierTs)` readiness(§12)——跨 proto(LeaderView)+ querycoord `CheckSchemaReady` + delegator 上报的 lockstep。

**风险 / 收紧**
- 分类器完备性(误判 destructive→additive = 静默损坏;已 fail-safe 默认 destructive + 单测覆盖 6 类 destructive)。
- additive 消灭部分写仅在"最多落后一版"下绝对成立(≥2 版仍窄窗部分写)。
- (PR-2 已定不做)故无"② 冷路径遍历完整性"这项风险;拉黑防线由 PR-0 ① **单独**兜住,与任何 plan 遍历无关。
- 本地 runner output inline 算的写路径 CPU 开销;不行就并入远程的延后 backfill。
- rename 语义未定,列为后续。

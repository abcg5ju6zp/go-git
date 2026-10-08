# Push 每-Ref 确认状态与崩溃可恢复 — 产品需求文档

## Overview
- **Summary**: 让一次 `Push` 为命令列表中的每个 ref 独立保存「待确认 / 已接受 / 已拒绝」结果；本地远程跟踪引用（`refs/remotes/<remote>/...`）只在收到远端对该 ref 的成功报告后才更新；结果通过持久化事务日志在部分失败、协议错误、重试与进程重启后不丢失、不回滚、不盲目重发。
- **Purpose**: 当前实现（[remote.go sendPack](file:///Users/ding/Documents/swe/0910072/project-06/remote.go#L156-L272) + [push.go SendPack](file:///Users/ding/Documents/swe/0910072/project-06/plumbing/transport/push.go#L17-L122)）把 `report-status` 折叠成「第一个错误」：推送无错时无条件更新**全部**命令对应的跟踪引用，出错时一个都不更新。多 ref 推送中远端先接受部分 ref 再拒绝/断线/取消时，客户端会把未确认的 ref 当作已推送；重试按陈旧状态重算还可能覆盖远端较晚的提交。
- **Target Users**: 一次推送多个分支和标签的发布脚本/CI，以及经过 SSH、HTTP(S)、file、git 协议使用 `Remote.Push` / `Repository.Push` 的所有调用方。

## Goals
- 一次 Push 内每个 ref 有独立、可查询、按命令顺序排列的结果：待确认（pending）、已接受（accepted）、已拒绝（rejected）。
- 只有远端明确报告 `ok` 的 ref 才更新对应的本地跟踪引用；`ng`、无报告、报告截断时一律不动该 ref 的跟踪状态。
- 已接受结果在部分失败、协议错误、同进程重试、进程重启后仍然可恢复：跟踪引用最终被精确更新一次，且不回滚、不覆盖已经更新到其他值的跟踪引用。
- 重试以重新获取的远端广告为准重新计算命令（线上更新本身由协议的 old→new CAS 保护），已满足的 ref 不再发送，缺失对象为空时不发送 packfile；库自身从不进行内部自动重发。
- 无变化推送、删除 ref、标签、Atomic、push-options、SSH/HTTP/file/git 传输以及现有错误类型和返回值保持兼容。

## Non-Goals
- 不实现客户端内部自动重试策略（指数退避、重试次数等）；重试仍是调用方再次发起一次新的 Push。
- 不改变 Git 线协议（不新增 capability，不改变 update-request / report-status 报文）。
- 不保证远端非原子更新的事务性（远端是否 atomic 仍由 `atomic` capability 决定）。
- 不为纯内存存储提供跨进程重启持久性（无持久介质；同进程内的重试仍受保护）。
- 不引入 packfile 字节假脱机（spool）：对象包是内容寻址、天然幂等的，恢复通过重新广告 + revlist 实现，而不是重放旧字节。
- 不改变 fetch/clone 的任何行为。

## Background & Context
- 推送链路：`Remote.PushContext` → 握手与 `GetRemoteRefs` → `sendPack` 生成 `[]*packp.Command` → revlist → `pushHashes`（内存管道编码 pack）→ `Session.Push` → `transport.SendPack`（[push.go](file:///Users/ding/Documents/swe/0910072/project-06/plumbing/transport/push.go)）→ 解码 `packp.ReportStatus` → `ReportStatus.Error()` 只返回第一个错误 → `Remote.updateRemoteReferenceStorage` 无条件更新全部命令映射的跟踪引用。
- `ReportStatus.Decode`（[report_status.go](file:///Users/ding/Documents/swe/0910072/project-06/plumbing/protocol/packp/report_status.go#L80-L112)）在缺少 flush / 中途 EOF 时返回错误，但此前已解码的 `unpack` 行与逐条 command 状态仍保留在结构体中——这是保留部分接受结果的关键事实。
- 远端错误类型：`packp.UnpackStatusErr`（值类型）、`packp.CommandStatusErr{ReferenceName, Status}`（值类型）；现有测试以 `ErrorContains`/`ErrorAs` 依赖它们。
- 存储可选能力有成熟先例：`storer.ShallowStorer` 等接口由 `storage/filesystem.Storage` 与 `storage/memory.Storage` 按需实现，自定义 storer 可不实现。文件存储可经 `Filesystem() billy.Filesystem` 在 `.git` 下做原子写（temp+rename，参考 [dotgit_rewrite_packed_refs.go](file:///Users/ding/Documents/swe/0910072/project-06/storage/filesystem/dotgit/dotgit_rewrite_packed_refs.go)）。
- 现有本地 ff/标签校验可能在客户端先行拒绝（如 `non-fast-forward update`）；这些路径在联系远端前返回，不产生事务日志。
- 多 ref 一接受一拒绝可由真实服务器自然产生（一个 ff 一个非 ff）；断线/截断可用自定义 `client.WithTransport` 包装与 file 传输的可替换服务端函数模拟。

## Functional Requirements

- **FR-1（逐 ref 结果模型）**: Push 完成后（无论成功、部分失败还是协议错误）可获得每个命令 ref 的 `{引用名, old, new, 状态(pending/accepted/rejected), 拒绝原因}`，顺序与发送命令一致；pending 表示已发送但未收到该 ref 的明确终态报告。
- **FR-2（仅确认后更新跟踪引用）**: 对 `ok` 的 create/update 命令，更新映射后的 `refs/remotes/<remote>/...`；对 `ok` 的 delete 命令，删除对应跟踪引用；pending/rejected 的 ref 不做任何本地跟踪变更。
- **FR-3（部分报告保留）**: report-status 解码中途失败（断线、context 取消、协议错误、缺 flush）时，已解码到的 `ok`/`ng` 行照常生效（accepted 更新跟踪、rejected 不更新），其余 ref 保持 pending；返回的错误仍是解码/连接错误。
- **FR-4（unpack 失败）**: `unpack` 行非 `ok` 时，任何跟踪引用都不得更新，所有 ref 标记为 rejected（原因为 unpack 状态），返回 `packp.UnpackStatusErr`。
- **FR-5（无 report-status 能力）**: 远端不具备 report-status 时，保持现行乐观语义：推送 I/O 成功即视为全部 accepted、更新跟踪引用、返回 nil。
- **FR-6（持久化事务日志）**: 支持可选 storer 接口的存储后端，在命令上链路前持久化一条以「remote 名 + URL」为键的事务记录（全部 ref=pending）；收到结果后持久化 accepted/rejected/pending 的最新状态；仍有 pending 时保留日志，全部终态后删除日志。文件后端写入必须原子（temp+rename）且崩溃后可重读；内存后端在同进程内有效。
- **FR-7（启动恢复）**: 一次新 Push 开始时读取该键的旧日志：accepted 条目立即以 CAS 方式应用到跟踪引用并从日志移除；pending 条目在本次 `GetRemoteRefs` 成功后与远端现状核对——远端已是 `new`（create/update）或已不存在（delete）则确认并 CAS 应用，否则保持未确认，交由本次正常命令计算处理。
- **FR-8（CAS 防覆盖）**: 跟踪引用的恢复/确认更新必须带期望值（=命令 old；create 期望 absent）。当前跟踪值已被更新到其他值（例如期间 fetch 到更新的远端提交）时跳过该 ref，绝不覆盖；删除同理（仅当跟踪值==old 时删除）。
- **FR-9（崩溃安全重放）**: 「持久化 accepted → CAS 应用跟踪更新 → 移除日志条目」的顺序保证任意一步崩溃后重放都是幂等的；其他 ref 的已接受结果不因某一 ref 失败而回滚。
- **FR-10（不重发 pack/不自动重发）**: 单次 Push 内不做内部自动重试；再次发起 Push 时按新广告重算 revlist，远端已具备全部对象时 `Packfile` 为 nil（线上无 pack 字节），已满足的 ref 不产生命令；非 force 推送由协议 old→new 校验保证不会覆盖远端较晚提交。
- **FR-11（兼容无变化推送）**: 命令数为 0 的路径继续直接返回 `NoErrAlreadyUpToDate`，不新建事务日志；启动恢复仍会先把旧日志中的 accepted 结果补齐（因此「推送其实已成功但响应丢失、重试显示 up-to-date」也能修好跟踪引用）。
- **FR-12（兼容删除/标签/混合批次）**: 纯删除推送（无 pack）、分支+删除同批、follow-tags、轻量/附注标签、force、force-with-lease、Atomic、push-options、Quiet/sideband 进度的行为与错误保持现状；Atomic 下逐 ref 信任远端报告行。
- **FR-13（传输无关）**: 状态采集在共享的 `transport.SendPack` 内完成，SSH/git（StreamSession）、HTTP(s)（smartPackSession）、file 传输自动同时生效。
- **FR-14（优雅降级）**: storer 不实现持久化接口、或日志文件读写失败（如只读 `.git`）时，Push 行为退化为当前行为（逐 ref 跟踪更新仍在进程内生效），日志故障经 trace 记录而不是中断推送。
- **FR-15（公开 API）**: 在不破坏既有 `Push(...) error` 签名与错误类型的前提下，提供可获取逐 ref 结果的新入口（Remote 与 Repository 各一组）；既有 `Push`/`PushContext` 签名与返回错误不变。

## Non-Functional Requirements
- **NFR-1（并发）**: 不引入任何全局/包级可变状态；内存后端自带互斥；文件后端以单键文件替换实现。
- **NFR-2（可靠性）**: 库代码不 panic；所有错误以 error 返回；JSON 日志带版本字段，损坏文件按「无日志」忽略。
- **NFR-3（性能）**: 正常推送仅增加一次小文件原子写（首条 pending）与一次终态写/删除；无额外网络往返；无额外对象遍历。
- **NFR-4（质量门禁）**: 新代码通过 `go build ./...`、`go vet`、`gofmt`、相关包 `go test -race`，并为每个行为变化配备「无修复会失败」的回归测试。

## Constraints
- **Technical**: Go 版本以 go.mod（1.26）为准；遵守 `.golangci.yaml`；仅使用现有依赖（标准库 `encoding/json`、`crypto/sha256`、billy FS）；不改动线协议报文格式。
- **Business**: 面向 `main`（v6）的内部 API 可扩展，但既有公开方法签名与错误值类型必须兼容。
- **Dependencies**: `plumbing/storer`（契约，低层无环依赖）、`plumbing/transport`（SendPack/请求结构）、`storage/{filesystem,memory}`、根包 `git`。

## Assumptions
- report-status 能力在现代服务端普遍存在；无该能力的服务器维持乐观假设（与现状一致）。
- 「进程重启」用同一 `.git` 目录重新打开 storage/仓库来模拟；不要求内存存储跨进程。
- 远端在连接中断前写入的 `ok` 行真实反映了 ref 已落盘（Git receive-pack 先更新 ref 再报告）。
- 同一 Remote 的并发 Push 不在支持范围内（与现状一致）。

## Acceptance Criteria

### AC-1: 部分接受/部分拒绝时只更新已接受 ref 的跟踪引用
- **Type**: `rule`
- **Given**: 本地一次推送两个分支，服务端对 A 返回 `ok`、对 B 返回 `ng <reason>`（如非快进）
- **When**: 调用 `Remote.Push`
- **Then**: 返回非 nil 错误且可 `errors.As` 到 `packp.CommandStatusErr`（引用名为 B、状态文案保留）；`refs/remotes/<remote>/A` 等于 A 的 new 且服务端 A 已更新；`refs/remotes/<remote>/B` 保持推送前的值且服务端 B 未变
- **Pass Condition**: 上述四项断言全部成立
- **Evidence**: 新增集成测试（file 传输 + 真实 receive-pack，一个 ff 一个非 ff）

### AC-2: 逐 ref 结果可查询且顺序一致
- **Type**: `rule`
- **Given**: 任意多 ref 推送（成功或失败）
- **When**: 调用带结果的新 Push 入口
- **Then**: 返回结果每个命令一项，顺序与 RefSpec 推导出的命令顺序一致，状态属于 pending/accepted/rejected，rejected 带原因
- **Pass Condition**: 成功批次全 accepted；混合批次 A=accepted、B=rejected；结果 API 与错误互不覆盖
- **Evidence**: remote 级测试 + SendPack 级测试

### AC-3: 报告截断/协议错误保留已接受结果
- **Type**: `rule`
- **Given**: 服务端写出 `unpack ok` 与 `ok refs/heads/A` 后在 flush 前断开（无 B 的状态行）
- **When**: Push 返回
- **Then**: 错误非 nil（解码/连接错误，文案含现有前缀）；A 的跟踪引用被更新为 new；B 保持 pending 且其跟踪引用不变；结果中 A=accepted、B=pending；调用返回后日志保留 B=pending。终态日志（含 A=accepted、B=pending）在任何跟踪引用更新之前已原子落盘——因此恰好在「收到报告、应用本地跟踪」之间崩溃时，重启可完整重放（以注入该状态日志的重启测试验证）
- **Pass Condition**: 全部断言成立；代码顺序为 持久化终态 → 应用跟踪 → 终态清理
- **Evidence**: 自定义包装传输（截断 report）的集成测试 + SendPack 脚本化读端单测 + AC-5 重启重放测试

### AC-4: unpack 失败拒绝全部跟踪更新
- **Type**: `rule`
- **Given**: 服务端返回 `unpack failed`（无 command 行）
- **When**: Push 返回
- **Then**: 错误可 `errors.As` 到 `packp.UnpackStatusErr`，文案含 `unpack error: failed`；所有 ref=rejected；无任何跟踪引用变化
- **Pass Condition**: 上述断言成立
- **Evidence**: 沿用并扩展 `TestSendPackWithReportStatusError` 断言结果状态 + remote 级断言

### AC-5: 进程重启后已接受结果不丢失且幂等重放
- **Type**: `rule`
- **Given**: AC-3 的截断推送后，关闭并以同一 `.git` 目录重新打开文件存储、重建 Remote（模拟重启），且服务端最终 A、B 均已更新
- **When**: 再次发起同样的 Push
- **Then**: 启动恢复先把日志中 A=accepted 经 CAS 应用到跟踪引用；B=pending 经本次广告核对（远端==new）后确认应用；日志被删除；推送整体返回 `NoErrAlreadyUpToDate`；两个跟踪引用均正确；重复执行恢复不报错、不改变结果
- **Pass Condition**: 重开后 A/B 跟踪引用正确、日志文件不存在、再次 Push 无错误变化
- **Evidence**: 文件存储重启集成测试（含同一恢复逻辑连续执行两次的幂等断言）

### AC-6: 不覆盖已更新到其他值的本地跟踪引用
- **Type**: `rule`
- **Given**: 日志记录 ref X 已接受（old=O, new=N），但重开后本地 `refs/remotes/<remote>/X` 已被 fetch 更新为另一个值 P
- **When**: 启动恢复执行
- **Then**: CAS 失败被识别为「已被更新状态取代」，跟踪引用保持 P，不报错，日志条目移除
- **Pass Condition**: 跟踪引用仍为 P
- **Evidence**: remote/恢复逻辑单测

### AC-7: 重试不重发 pack 且不自动重发未确认命令
- **Type**: `rule`
- **Given**: AC-3 截断推送（服务端实际已接受 A、B 并持有对象）
- **When**: 同进程或重启后再次 Push 同样 RefSpec
- **Then**: 第二次连接中客户端发送的 packfile 字节数为 0（请求无 Packfile）；不产生针对 A、B 的更新命令；返回 `NoErrAlreadyUpToDate`
- **Pass Condition**: 包装传输计数 pack 字节==0，且服务端引用正确
- **Evidence**: client.WithTransport 计数包装的集成测试

### AC-8: 兼容性矩阵不回退
- **Type**: `rule`
- **Given/When/Then**: ①无变化推送仍返回 `NoErrAlreadyUpToDate` 且无日志残留；②纯删除推送成功并删除本地跟踪；③分支+删除同批成功；④标签（轻量/附注/已存在标签拒绝/强更）行为与错误文案不变；⑤无 report-status 能力时成功并全部更新跟踪；⑥既有 push 测试全部通过（SSH/git 守护进程相关已知环境失败除外）
- **Pass Condition**: 现有 push 相关测试全部通过 + 新增针对②⑤的测试
- **Evidence**: `go test ./...`（过滤已知 git-daemon 限制）

### AC-9: 不支持/只读存储优雅降级
- **Type**: `rule`
- **Given**: 使用不实现持久化接口的 storer，或日志目录不可写
- **When**: Push 成功或部分失败
- **Then**: 推送结果、错误、跟踪引用行为与现行行为一致；无 panic；trace 中有记录
- **Pass Condition**: 推送正常返回；只读场景不新增错误
- **Evidence**: 内存存储（实现接口）与降级路径单测/现有 fixture 只读路径测试

### AC-10: 工程质量与可维护性
- **Type**: `rubric`
- **Dimension**: 分层与代码质量
- **Scale**: 1-5
- **Anchors**: 1=跨层污染、引入全局状态或 panic；3=功能正确但分层勉强、注释冗余；5=契约置于 storer、采集置于共享 SendPack、编排集中在 remote，命名地道、注释只解释 Git 语义、无抽象浪费
- **Pass Threshold**: >= 4
- **Evidence**: 代码评审 + `go vet`/`gofmt`/`golangci-lint`（可用时）+ 测试可读性

## Open Questions
- 无（实现中如发现 Git 语义细节分歧，以本地 `git` 参考行为与 [git/git 文档](https://git-scm.com/docs/pack-protocol) 为准并在代码注释中引用）。

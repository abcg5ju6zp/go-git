# Push 每-Ref 确认状态与崩溃可恢复 — 实施计划

## Task 1: 定义推送事务的 storer 契约
- **Status**: `completed`
- **Priority**: high
- **Depends On**: None
- **Description**:
  - 新增 `plumbing/storer/pushstate.go`：`RefUpdateState`（`pending`/`accepted`/`rejected` 字符串常量）、`PushRefUpdate{Name, Old, New, State, Reason}`（含 IsDelete 与 hex 自定义 JSON 编解码）、`PushTransaction{Version, RemoteName, RemoteURL, Started, Updates}`。
  - 可选接口 `PushStateStorer`：Load/Save/DeletePushTransaction；`PushTransactionKey` 以 sha256 生成确定性键。
- **Acceptance Criteria Addressed**: AC-5, AC-9
- **Test Requirements**:
  - `rule` TR-1.1: 类型零值可用、常量稳定、哈希 JSON 往返保留（40/64 位 hex）。证据：storage 两侧 roundtrip 测试。
- **Completion Evidence**:
  - [plumbing/storer/pushstate.go](file:///Users/ding/Documents/swe/0910072/project-06/plumbing/storer/pushstate.go)；`go build`/`go vet`/lint 通过；哈希编解码曾在集成测试中暴露 ObjectID 序列化为 `{}` 的问题并修复。

## Task 2: SendPack 产出逐 ref 报告并保留截断时的部分结果
- **Status**: `completed`
- **Priority**: high
- **Depends On**: Task 1
- **Description**:
  - `plumbing/transport/common.go`：新增 `RefStatus`、`PushReport{UnpackStatus, Refs}` 与 Accepted/Rejected/Pending 访问器；`PushRequest.Report *PushReport`。
  - `plumbing/transport/push.go`：发送前全部初始化为 pending；无 report-status → 全 accepted（乐观语义不变）；解码后按行映射；unpack 失败 → 全 rejected；解码错误时保留已解码行、其余 pending；返回错误类型与文案不变。
- **Acceptance Criteria Addressed**: AC-2, AC-3, AC-4, AC-8
- **Test Requirements**:
  - `rule` TR-2.1/2.2/2.3: 全 ok、混合 ok/ng、截断、unpack 失败、无能力五场景。证据：`TestSendPackReport*` 5 个新测试全部通过。
- **Completion Evidence**:
  - [common.go](file:///Users/ding/Documents/swe/0910072/project-06/plumbing/transport/common.go)、[push.go](file:///Users/ding/Documents/swe/0910072/project-06/plumbing/transport/push.go)、[send_pack_test.go](file:///Users/ding/Documents/swe/0910072/project-06/plumbing/transport/send_pack_test.go)；`go test ./plumbing/transport/` 通过（含 race）。

## Task 3: 内存与文件存储实现事务持久化
- **Status**: `completed`
- **Priority**: high
- **Depends On**: Task 1
- **Description**:
  - `storage/memory/pushstate.go`：互斥 map + 深拷贝；容忍零值 Storage。
  - `storage/filesystem/pushstate.go`：`.git/go-git/push/<sha256>.json`，temp+rename 原子写（含 ErrNotSupported 回退）；缺失/损坏/版本不符按无记录处理；编译期断言两 Storage 实现接口。
- **Acceptance Criteria Addressed**: AC-5, AC-9, AC-10
- **Test Requirements**:
  - `rule` TR-3.1: 重开存储后记录一致；delete 幂等；坏 JSON 忽略；键区分。
  - `rule` TR-3.2: 并发 race 安全；零值 Storage 不 panic。
- **Completion Evidence**:
  - [memory/pushstate.go](file:///Users/ding/Documents/swe/0910072/project-06/storage/memory/pushstate.go)、[filesystem/pushstate.go](file:///Users/ding/Documents/swe/0910072/project-06/storage/filesystem/pushstate.go)；对应 `pushstate_test.go` 全部通过（race）。

## Task 4: remote 编排 — 恢复、CAS 跟踪更新、终态持久化
- **Status**: `completed`
- **Priority**: high
- **Depends On**: Task 1, Task 2, Task 3
- **Description**:
  - Push 开始（网络前）应用旧日志 accepted（CAS）；广告后确认 pending（远端已达 new / 删除已生效）并立即持久化剩余集合，避免 no-op 早返回泄漏日志。
  - 命令上链路前持久化 pending（含与 carried 合并，同名新命令覆盖）；推送后无论错误与否仅对 accepted 做期望值 CAS 跟踪更新（create 期望 absent、delete 比对 old、CAS 冲突跳过不覆盖）；终态删除已决条目、保留 pending。
  - 删除旧的无条件 `updateRemoteReferenceStorage`；日志 I/O 失败 trace-only。
- **Acceptance Criteria Addressed**: AC-1, AC-3, AC-4, AC-5, AC-6, AC-7, AC-8, AC-9
- **Test Requirements**:
  - `rule` TR-4.1~4.5: 见集成测试 `remote_push_confirmation_test.go`（部分拒绝、截断+重试 0 pack、重启恢复幂等、CAS 不覆盖、全成功无残留）；既有全部 push 测试通过。
- **Completion Evidence**:
  - [remote.go](file:///Users/ding/Documents/swe/0910072/project-06/remote.go)、[remote_push_state.go](file:///Users/ding/Documents/swe/0910072/project-06/remote_push_state.go)；根包 `go test -race .` 通过（94s 全量）。

## Task 5: 公开逐 ref 结果 API（Remote/Repository）
- **Status**: `completed`
- **Priority**: medium
- **Depends On**: Task 4
- **Description**:
  - `PushRefState=storer.RefUpdateState` 别名与三常量；`PushRefStatus`、`PushResult`（Status/Accepted/Rejected/Pending/Error）。
  - Remote：`PushResult`/`PushContextResult`；Repository 同名方法；`Push`/`PushContext` 改为薄封装，签名与错误不变。
- **Acceptance Criteria Addressed**: AC-2, AC-8
- **Test Requirements**:
  - `rule` TR-5.1: 成功全 accepted、失败批次带原始错误、旧 Push 行为不变。证据：`TestPushResultAllAcceptedAndNoJournal` 及全量既有测试。
- **Completion Evidence**:
  - [repository.go](file:///Users/ding/Documents/swe/0910072/project-06/repository.go#L1465-L1503)、[remote_push_state.go](file:///Users/ding/Documents/swe/0910072/project-06/remote_push_state.go)；`go vet`/lint 0 issue。

## Task 6: 测试运输 — 可截断 report 的 file 服务端与 pack 计数传输
- **Status**: `completed`
- **Priority**: high
- **Depends On**: Task 2
- **Description**:
  - `file.Options.ReceivePack ServerFunc` 窄接缝（默认行为不变）。
  - 测试内：跨 Write 的 pkt-line 帧感知截断 writer（转发前 N 个 ok 行后吞掉后续与 flush）；hook 移动引用制造真实 ok+ng；`countingTransport` 统计 Push 次数与 pack 字节。
- **Acceptance Criteria Addressed**: AC-3, AC-5, AC-7
- **Test Requirements**:
  - `rule` TR-6.1: 截断稳定复现一 accepted 一 pending + decode 错误；第二次推送 packPushes==0、packBytes==0。
- **Completion Evidence**:
  - [file.go](file:///Users/ding/Documents/swe/0910072/project-06/plumbing/transport/file/file.go#L34-L71)、[remote_push_confirmation_test.go](file:///Users/ding/Documents/swe/0910072/project-06/remote_push_confirmation_test.go)；集成测试全部通过。

## Task 7: 全量验证与静态检查
- **Status**: `completed`
- **Priority**: high
- **Depends On**: Task 4, Task 5, Task 6
- **Description**:
  - gofmt/gofumpt/gci、go vet、golangci-lint(v2.14) 0 issue；根包与相关包 `go test -race` 通过；全仓 `go test ./...` 仅 `plumbing/transport/ssh` 因环境缺 SSH_KNOWN_HOSTS 失败（与本次改动无关，失败点在主机密钥回调）。
- **Acceptance Criteria Addressed**: AC-8, AC-9, AC-10
- **Test Requirements**:
  - `rubric` TR-7.1: 工程质量自评 4/5 — 契约/采集/编排分层清晰，无全局状态/panic；注释聚焦 Git 语义。
- **Completion Evidence**:
  - 命令输出：`golangci-lint run ...` → `0 issues`；`go test -race .` → ok 94.7s；`go test ./...` 仅 ssh 环境失败。

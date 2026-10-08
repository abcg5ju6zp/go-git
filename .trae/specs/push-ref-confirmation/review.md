# Push 每-Ref 确认状态与崩溃可恢复 — 独立评审

## 检查点结论（R2 后更新）

- [x] CP-R1: 部分接受/部分拒绝只更新 accepted 跟踪引用，错误仍是 packp.CommandStatusErr — **pass**（R1/R2）
- [x] CP-R2: 逐 ref 结果可查询、顺序与命令一致、rejected 带原因 — **pass**
- [x] CP-R3: 报告截断时已解码 ok 生效、其余 pending；终态在跟踪更新前落盘 — **pass**（R2 复核新顺序）
- [x] CP-R4: unpack 失败全部 rejected、无跟踪更新、UnpackStatusErr（含 remote 级脚本化测试）— **pass**
- [x] CP-R5: 重启 accepted 重放、pending 广告确认、再次执行幂等 — **pass**
- [x] CP-R6: 跟踪引用前进/删除冲突/create 冲突均不覆盖（三分支测试）— **pass**
- [x] CP-R7: 重试 packPushes==0、packBytes==0 — **pass**
- [x] CP-R8: 无变化/删除/标签/混合/无 report-status 等既有行为不回退 — **pass**（根包 race 全量）
- [x] CP-R9: 不实现接口的 storer 成功推送并更新跟踪；只读 fixture 推送既有测试覆盖；坏记录/零值存储/并发 — **pass**
- [x] CP-U1: 工程质量 — **4/5**

## Review History

### R1（独立代理，全新上下文）
- **结果**: pass（带 major）。
- Blocker: 0。
- Major-1: journal 不持久化 accepted，「收到 ok → CAS 前」崩溃窗口丢结果，且与 AC-3/FR-9 顺序不符。
- Major-2: 线上 ok 也走恢复级 CAS-skip，相对旧行为在陈旧跟踪场景回退。
- Major-3: 测试缺口：remote 级 unpack、降级 storer、update/delete CAS 分支、SHA256/zero 哈希往返、只读降级。
- Minor: 导出无消费者的 RefStatus.IsDelete；Save 就地改写调用方 Version；Started 重写；空 UnpackStatus 畸形响应旧行为差异等。

### R1 修复
- 新顺序：**先 `recordPushOutcomes`（accepted/rejected/pending 原子落盘）→ 再应用跟踪更新 → 成功后 `finalize` 仅留 pending**；tracking 更新失败时保留 outcomes 日志下次 CAS 重放（[remote.go sendPack](file:///Users/ding/Documents/swe/0910072/project-06/remote.go#L307-L331)、[remote_push_state.go](file:///Users/ding/Documents/swe/0910072/project-06/remote_push_state.go#L255-L407)）。
- 区分两条路径：线上 accepted 恢复旧的无条件 Set/Remove；恢复/广告确认才用 CAS 防覆盖。
- 补齐测试：`TestRecoveryTrackingCASBranches`（update/create/delete/幂等 7 分支）、`TestPushUnpackFailureUpdatesNoTracking`（remote 级）、`TestPushWorksWithoutPushStateStorer`（接口隐藏包装）、`TestPushRefUpdateJSONRoundTrip`（sha1/sha256/zero）。
- Minor：删除未用导出、Save 不改调用方、Started 全程保留、空 UnpackStatus + 无解码错误时维持旧的 UnpackStatusErr{""} 行为（而截断于 unpack 行之前保持 pending）。
- AC-3 文本按实现澄清（终态先落盘的崩溃窗口不变量）。
- 复验：`golangci-lint` 0 issue；根包 `go test -race .` ok 99.7s。

### R2（全新代理复审）
- **结果**: **pass**。CP-R1~R9 全部 pass，CP-U1 4/5；无 blocker、无新 major；3 个 minor。
- Minor-1：完整解码但 unpack 状态为空串的畸形响应，新代码返回 nil，旧实现返回 `UnpackStatusErr{Status:""}`。
- Minor-2：recover 遇非 CAS 存储错误会永久阻断后续 Push。
- Minor-3：无人认领的 carried pending 会残留日志（设计上保守安全）。

### R2 修复（均已完成并复验）
- Minor-1：`applyDecodedReport` 恢复「空 UnpackStatus 即 unpack 失败」语义（截断于 unpack 行之前仍保持 pending），新增 `TestSendPackReportEmptyUnpackStatusIsAnError`。
- Minor-2：recover 对非 CAS 存储错误改为 trace 记录、保留日志原样、不阻断本次推送（live 跟踪写仍会直接暴露存储故障）。
- Minor-3：finalize 补充注释说明残留 carried pending 是有意的保守策略；`PushResult.Error()` 注释改为准确描述其便利方法定位。
- 复验：`golangci-lint` 0 issue；根包/storer/storage `go test -race` 全绿（99.3s）；SSH 包仅环境性失败（缺 known_hosts，与改动无关）。
- **最终结论：pass，可合入。**

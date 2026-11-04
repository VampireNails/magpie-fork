# Magpie v3.3 项目交接：现状与证据

仓库内交接副本；代码基线为 `dd8b3b9c45fa84957c37d831d16eea70d3a761db`，此文档提交不修改产品代码。所有标为“本机材料”的绝对路径只在原工作机有意义，文件不随仓库提供；跨机器接手须另行获取，不应按仓库相对链接解析。

截至 2026-11-05 约 07:07（中国时间）。这是状态交接，不是最终验收或部署批准。后续必须重新核实会变化的仓库、进程和服务状态；保留现有工作树、配置和凭据，不因清理便利删除未知数据。

## 用户目标与边界

按 PRD v3.3 完成现有 Linux 服务端、Windows/WorkBuddy 和 Instabox/Hermes 的模型供给、自动登记、真实调用、管理、连接恢复、设备与诊断验收。旧版未发布，AC11 的新旧实例迁移与回退已按用户决定移出范围；**agent 接入前配置恢复仍在范围**。MCP/Skills 客户端适配不在首版。

PRD（本机材料：`D:/tmp/magpie/PRD-Magpie服务端与客户端-v3.3.md`）也不随本仓库提供；接手时需取得并核对其内容。

用户要求停止零散打补丁，先看整体改善；不要将额外隔离环境建设写成 PRD 要求或交付门槛。直接使用现有 Windows 和 Instabox，具体操作所需的配置备份/恢复是保护措施；不要求新 VM、账户、第三台机器。尊重此前跳过修改真实 WorkBuddy 配置的选择；不得读取、复制或向网络暴露真实凭据，不擅停现有服务。之前对 `D:/tmp/magpie-device-resident-cb6b4d` 删除的自动审批被拒，禁止绕过。真实模型请求会产生用量，不应机械重复已有用户操作；真实 agent 行为不可用直接网关请求替代。

## 代码与运行状态

- 11-05 约 07:06 实时远端核验：`main` = `fdfce4d986260527c399affaf7ff3de6d357cf27`；`integration/v33-handoff-20261104` = `2259b7d0f7f4034b9b9e070f8b884cd5d9346a74`；`integration/v33-c3-20261103` = `400d392a89c6818d16d87d36333aabfe1201f143`。远端状态随时可能变化；`origin` push URL 为 `disabled://no-push-authorized`。
- `D:/tmp/magpie-ac05-slow-refresh`：`fix/ac05-slow-refresh` @ `2259b7d0f7f4034b9b9e070f8b884cd5d9346a74`。该版本是已有服务端及双端实测的主要对象，**不是最终验收结论**。
- `D:/tmp/magpie-v33-unified-integration`：`integration/v33-unified-ac09-20261104` @ `dd8b3b9c45fa84957c37d831d16eea70d3a761db`，时点核查干净；包含 AC09 生命周期改动，未证明已部署或最终选定。`D:/tmp/magpie-ac09-state-loss` @ `dc06317dafc274a4410f8eb5061b62884133e8d9`，其生命周期文件树与 dd8 对应。
- `D:/tmp/magpie-ac09-fs-probe` 为 detached HEAD `282239eb6ae062efef5e21afd67a46eb3d46e266`，有未跟踪的 `internal/remotejoin/cross_process_storage_probe_test.go`；不要删除或覆盖该探针。AC09 state-loss 工作树时点核查干净。
- `D:/tmp/magpie-v33-c1-diagnostic-6bfee8e9` detached @ `6bfee8e9b4e28ed13c07c0e987d9c172fe7a29e2`，有未暂存的 `internal/cli/help_cli_test.go`；保留原样。除上述探针、C1 及下一条安全分支外，其余登记工作树在这次审计中干净，状态随后可能变化。
- `D:/tmp/magpie-ac10-independent-security`：`fix/ac10-independent-security` @ `5f29e36a860b8eec48463a5c77ed5bfdd0ab4546`；两个凭据处理文件有未提交且未验证修改，**不是 dd8 的组成部分**。保留原样；5f29 的混合 choice 正常内容过度遮盖已在本地网关用合成凭据复现。详见 安全核查（本机材料：`D:/tmp/magpie/outputs/2026-11-04/AC10-独立安全核查-2259b7d.md`）。
- 项目经理 07:06 报告：现有服务 `magpie-serve` active，Linux 产物 SHA256 `0c07602305b0df524ee4213ea83086fd1c5c7de65f40e5e54713f381eb1bb212`；Instabox suspended。该哈希与历史 2259 构建记录绑定，不证明其他候选已部署。Windows owner PID 20896 存在、监听本机 3425；WorkBuddy PID 9032 本次不在。不要停止 owner 或擅自唤醒/更改服务。

## 已有真实证据及限制

- **正常模型调用**：旧 `02de6a49d162230a9dbb03240b4243024629ca6f` 真实 WorkBuddy 与 Hermes 各有两模型切换、实际工具执行记录；WorkBuddy（本机材料：`D:/tmp/magpie-workbuddy-acceptance/evidence.md`）、Hermes（本机材料：`D:/tmp/magpie-review-isolated/hermes-acceptance-02de6a49.md`）。本轮 2259 用户确认 WorkBuddy Flash/Pro 返回指定答复，但执行者未独立观察答复或上游模型；计算答复提到 Bash，不足以证明实际工具运行。见 Windows 记录（本机材料：`D:/tmp/magpie/outputs/2026-11-04/Windows-WorkBuddy-2259b7d-待用户操作.md`）。
- **Hermes 新近实测**：2259 对应产物下 Hermes 真实调用 Flash/Pro 成功；临时 CLI 参数不证明持久接线切换。工具记录中 terminal 实际执行并返回结果，read_file 有调用但未确认成功；真实 stream-json 两次产生增量事件。现有配置未做持久接入/退出复验。见 Hermes 记录（本机材料：`D:/tmp/magpie/outputs/2026-11-04/Hermes现有双端-真实调用复核-2259b7d.md`）。指定 Codex 远端 ID 得到答复的另一次记录尚无上游路由证据，不能断言实际供应商或无 fallback。
- **登记与恢复**：2259 交接记录双端注册、Windows 被删后常驻重登记、另一设备不受影响，以及 Hermes 离线恢复和不自动重新接入；部分为历史叙述，非完整现场原始日志。AC09 的 `dc06317` 合成状态测试和真实跨进程锁测试覆盖混合故障窗口、锁临界释放与健康对照，但替身恢复不证明真实 agent 恢复耗时；设备重装、节点重建等仍缺真实完整证据。
- **模型变化与管理**：2259 交接叙述模型 2→1→2、订阅后两端 2→10、管理页/API 与 OAuth 回调；逐步原始输出并不完整，不能推断所有价格、能力、管理功能或模型变更通过。历史 OAuth 不因 SHA 变化机械重做。Windows 桌面“服务端”入口实际点击仍未完成；此前桌面工具点击和截图分别报几何不可用和采集超时，不能判定产品失败。
- **共享管理并发**：11-04 23:16，Windows 和现有 Instabox 在真实 Tailscale 路径对 Windows 临时 2259 管理服务各自登录，同一合成身份并发写同字段；两次 POST 200，双方读回 `theme=light`。握手约束下、假设时钟未跳变，两个客户端请求窗口重叠；不证明服务器内部同时处理、普遍写入规则、真实服务或不同管理身份。见 完整记录（本机材料：`D:/tmp/magpie/outputs/2026-11-04/AC08-跨设备共享管理并发-2259b7d.md`） 和 脱敏逐阶段输出（本机材料：`D:/tmp/magpie/outputs/2026-11-04/raw/ac08-cross-device-2259-r3.jsonl`）。此前 r1 同步超时、r2 驱动解析错误，不是产品失败。

## 清理与安全边界

跨设备 r3 记录称临时服务 PID 19648 已停止、端口 64526–64528 无监听，Instabox 按脚本标识未发现匹配进程；r1/r2 临时进程和端口也已清理。本次 Windows 只读复查 PID 19648、26756、13712、25856 不在，相关端口无监听；该观察不能替代当前 Instabox 复查。合成测试目录仍保留，勿擅自清理。Windows 原 owner 20896 保留；WorkBuddy 9032 已不在，后续状态需重新核实。

旧供应商 key 曾有副本短暂暴露，用户确认旧 key 已撤销；后续交接报告替换 key 的三协议测试成功，但当前凭据和实际服务状态不能凭历史推断。不要读取凭据文件、复制配置或输出含凭据链接。日志、诊断和各协议错误出口的完整安全性仍未证明；不能将几项遮盖测试提升为全覆盖。

## 后续方向

依据 整体改善评估（本机材料：`D:/tmp/magpie/outputs/2026-11-04/整体改善评估.md`）、连接生命周期草案（本机材料：`D:/tmp/magpie/outputs/2026-11-04/连接生命周期与设备身份-契约草案.md`） 和 安全诊断草案（本机材料：`D:/tmp/magpie/outputs/2026-11-04/安全诊断与流式响应-整体契约方案.md`），先整体评估连接状态/恢复时序和协议语义/敏感错误出口，再选择与上游兼容的方案；草案不是产品要求或实施授权。真实正常路径优先复用既有证据，按实际代码变化补测；重点仍包括 Windows 桌面管理入口、真实模型增删改及元数据、设备身份变化、运行中 agent 的恢复以及诊断安全。保存最终选定源码完整 SHA、产物、平台、结果与未测项；全量测试、race、真实双端和完整 AC01–10 不得在未完成时写成通过。推送、合并、部署遵循现有授权，未经验收不合入 main。

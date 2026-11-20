# T297 交付说明：DTLS 握手重传上限

## 背景与目标

移动终端经公网安全接入网关建立 DTLS 通道时，弱网与切网会使部分终端在握手途中静默消失。此前实现对只收不回的对端按 1s 起逐次翻倍、60s 封顶的重传间隔无限重传：握手调用不返回也不失败，在途接入的握手协程与底层套接字常驻，调用方只能靠外部超时兜底。本次改动为握手增加**重传次数上限**：对端持续静默时，有限次重传后以可分类错误结束握手，并保留显式关闭该上限的配置入口。

## 改动文件清单与需求逐条对应

| 文件 | 改动内容 | 对应需求 |
| --- | --- | --- |
| `internal/config/handshake.go` | 新增常量 `DefaultMaxRetransmitAttempts = 12`（含默认值取舍说明注释）；`HandshakeConfig` 新增字段 `MaxRetransmitAttempts int`，注释说明语义（0 或负值关闭上限、计数随对端进展复位、默认装配 12） | 新增配置项；说明注释与默认值装配一致 |
| `options.go` | `dtlsConfig` 新增 `MaxRetransmitAttempts` 字段；`applyOptions` 默认装配 `dtlsconfig.DefaultMaxRetransmitAttempts`；新增 `WithMaxRetransmitAttempts(attempts int)` 选项（0 显式关闭上限，负数返回 `errInvalidMaxRetransmitAttempts`）；`newHandshakeConfig` 将字段装配进 `HandshakeConfig` | 配置装配面；显式关闭上限的配置入口 |
| `internal/handshake/fsm.go` | 新增哨兵错误 `ErrRetransmitLimitExceeded`；新增计数助手 `countRetransmit`（上限非正时直接放行，保持既有无限重传） | 有限次重传后以可分类错误结束握手 |
| `internal/handshake/fsm12.go` | `fsm12` 新增 `retransmitAttempts` 字段；`wait` 的重传定时器分支在确认重传前计数，耗尽即返回 `StateErrored` 与上限错误；`handleWaitingFlight` 在收到非重传的对端报文（对端有进展）时复位计数 | DTLS 1.2 等待处理的上限语义；静默对端恢复后重获全额预算 |
| `internal/handshake/fsm13.go` | `fsm13` 新增 `retransmitAttempts` 字段；`retransmitTimeout` 改为返回 `(State, error)` 并在重传前计数（`wait` 与 `finish` 两处调用点同步更新）；`prepare`（对端响应后生成新 flight）复位计数 | DTLS 1.3 等待处理的上限语义 |
| `internal/handshake/fsm_retransmit_limit_test.go` | 新增回归测试（同目录同框架，testify + `flightTestConn`/`handshakeTestTimer` 既有设施） | 四类构造输入的回归覆盖 |
| `options_test.go` | 新增 `TestMaxRetransmitAttemptsOption`：默认装配 12、显式 0 关闭、自定义值、负数报错、`newHandshakeConfig` 装配一致性 | 配置装配的测试覆盖 |

未改动的关键点（需求要求"不受影响"的部分）：

- `handleRetransmitTimeout` 的退避曲线与 60s 封顶逻辑原样保留，上限只在重传前计数，不改变间隔节奏（新测试逐次断言间隔曲线 1→2→4→8→16→32→60→60…）。
- `ctx.Done()` 分支（`handleWaitCancellation`）原样保留：已配置超时或取消的调用在上限之前结束，返回原有的 `context.Canceled`/`context.DeadlineExceeded`，且间隔复位行为不变（既有测试 `TestHandshakeFSMWaitCancellationResetsRetransmitInterval` 与新测试 `TestHandshakeFSMRetransmitLimitWaitCancellation` 双重覆盖）。
- 错误传播路径未动：上限错误经 `conn.go` 的 `handshake failed: %w` 包装后返回调用方。

## 上限语义的设计取舍

设计输入为 `internal/handshake` 内 flight 状态机的公共等待循环（`runHandshakeFSM`）与两个协议版本各自的等待处理（`fsm12.wait`、`fsm13.wait`/`retransmitTimeout`）。取舍如下：

1. **计数点放在各版本的等待处理中，而非公共循环**。重传决策（`retransmit` 标志、cookie 重试、对端重传退避）分散在两个版本的 `wait`/`retransmitTimeout` 内，公共循环只看到状态迁移。在重传定时器分支、确认将要重传时计数，语义最准确：只对"对端静默导致的重传"计费，cookie 重试（对端刚回过 HelloVerifyRequest，并非静默）与 ACK 驱动的部分重传不计费。
2. **计数随对端进展复位**。上限约束的是"持续静默"这一段，而不是整个握手的总重传次数。DTLS 1.2 在 `handleWaitingFlight` 收到非重传报文处复位（与既有 `retransmitInterval` 复位点一致），DTLS 1.3 在 `prepare` 生成新 flight 处复位（同样与既有间隔复位点一致）。弱网短暂失联后恢复的终端因此重获全额预算，与"滚动重启窗口内短暂失联后重试接入的终端必须仍能在默认配置下完成握手"的要求一致。
3. **默认取值 12 次**。默认 1s 起始、逐次翻倍、60s 封顶的曲线下，12 次重传覆盖约 8 分钟（1+2+4+8+16+32+60×7≈483s 后终结）：前 6 次在 63 秒内完成退避爬升，覆盖典型弱网抖动；其后 6 次以 60s 间隔再覆盖约 7 分钟，容下单次切网与滚动重启窗口内的恢复；同时保证在途接入的协程与套接字在有限时间内必然释放，不再依赖外部超时兜底。
4. **逃生口形态：零值关闭**。`WithMaxRetransmitAttempts(0)` 显式恢复既有无限重传行为（内部 `MaxRetransmitAttempts <= 0` 一律视为关闭，`countRetransmit` 直接放行）；负数属于配置错误，选项装配时返回错误。零值即关闭的形态与"未配置走默认、显式 0 走旧行为"的装配模型自洽，且不需要额外的布尔开关。
5. **错误可分类**。上限耗尽返回独立哨兵错误 `ErrRetransmitLimitExceeded`（定义在 `internal/handshake`，受改动面约束未放入 `internal/errors`），与对端拒绝（alert 错误）和本机网络故障（net 错误）两类既有错误在类型与文本上均可区分，运维侧可按错误分类汇入告警。该错误不是 `context.Canceled`/`context.DeadlineExceeded`，不会与调用方自身的超时混淆。

## 新增回归测试（internal/handshake 同目录同框架）

`internal/handshake/fsm_retransmit_limit_test.go`，对 DTLS 1.2 与 1.3 两个等待处理各覆盖四类构造输入：

| 测试 | 构造输入 | 断言 |
| --- | --- | --- |
| `TestHandshakeFSMRetransmitLimitExceeded` | 默认上限（12）+ 立即触发的定时器，对端全程静默 | 前 12 次均返回 `StateSending` 且间隔曲线逐次翻倍至 60s 封顶；第 13 次返回 `StateErrored` 与 `ErrRetransmitLimitExceeded`，且不是 context 错误 |
| `TestHandshakeFSMRetransmitLimitDisabled` | 上限显式置 0，对端静默 | 20 次（超过默认上限）重传全部返回 `StateSending`，无终态错误，恢复既有无限重传 |
| `TestHandshakeFSMRetransmitLimitWaitCancellation` | 上限 2 + 永不触发的定时器 + 已取消/已超时的 ctx | 取消/超时先于上限生效，返回 `context.Canceled`/`context.DeadlineExceeded`，间隔复位为初始值，计数未动 |
| `TestHandshakeFSM12RetransmitLimitResetsOnPeerRecovery` | 上限 3，静默 2 次后注入 HelloVerifyRequest | 对端恢复后计数复位为 0，随后重获全额 3 次预算，第 4 次才出终态错误 |
| `TestHandshakeFSM13RetransmitLimitResetsOnPeerRecovery` | 上限 3，计数置 2 后调用 `prepare` | 生成新 flight 后计数复位为 0 |

`options_test.go` 新增 `TestMaxRetransmitAttemptsOption` 覆盖默认装配、显式关闭、自定义值、负数报错与 `newHandshakeConfig` 装配一致性。

## 实测读数（对只收不回对端的握手）

实测方法：本地 UDP 静默对端（只收不回），客户端 `dtls.Client` + `HandshakeContext`，默认 `FlightInterval`（1s）。对端每收到一个数据报记录一次时间戳，即为重传序列。

### 默认配置（上限 12 次）

（待实测完成后填入）

### 显式关闭上限（WithMaxRetransmitAttempts(0)）

（待实测完成后填入）

## 验收命令最终读数

（待实测完成后填入）

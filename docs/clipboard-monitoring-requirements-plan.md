# 剪贴板监听、去重与回声抑制优化需求及实施计划

## 文档信息

- **状态**：v0.11 实现完成，待发布与部署验收
- **编写日期**：2026-08-12
- **当前基线**：v0.10（提交 `56b85ea7`）
- **目标版本**：v0.11（统一处理层与原生 Wayland 后端一次性交付）
- **适用平台**：Linux Wayland、Linux X11、Windows、macOS

## 1. 背景

KDE Wayland 环境曾出现截图写入剪贴板后桌面冻结、鼠标仍可移动的问题。现场排查确认：

- Linux 监听器按一小时轮换时未终止旧的 `wl-paste -w`，曾累计 28 个监听进程。
- 截图时多个监听器同时向 KWin 请求图片，Plasma 连续记录 `DataControlOffer: timeout reading from pipe`。
- v0.10 已修复监听器生命周期，并将 Linux/Windows 系统通知与剪贴板读取解耦为非阻塞通知和单工作线程串行读取。
- v0.10 连续运行约 15 小时并完成 15 次监听器轮换后，仍只有一个 `wl-paste`，KWin 未再出现 `DataControlOffer` 超时，用户确认截图不卡顿。

现阶段仍有以下可优化点：

1. 部分截图会在同一秒产生两组图片发送记录，说明重复事件没有被完全合并。
2. Wayland 当前使用 `wl-paste --watch date +%s` 获取通知，再执行 `wl-paste --list-types` 和新的 `wl-paste` 读取内容；一次变化会多次访问 KWin。
3. 去重仅比较最近一次原始字节，窗口为 1 秒；无法覆盖时间稍长的重复事件，也无法识别像素相同但编码或元数据不同的图片。
4. 接收远端内容后的回声抑制主要依赖时间窗口，缺少与具体内容、平台序列号或 selection 身份绑定的判断。
5. Linux 命令后端仍需要外部进程、周期轮换和进程回收逻辑，维护面较大。

## 2. 需求目标

### 2.1 核心目标

1. 剪贴板内容读取、解码、去重和本地写入必须严格串行，任意时刻最多一个内容读取操作。
2. 系统消息循环、Wayland 事件循环和命令监听管道不得执行耗时的内容读取或网络发送。
3. 同一语义内容的密集重复通知只触发一次同步，不增加明显的用户可感知延迟。
4. 远端内容写入本地剪贴板后，不得作为新的本地内容再次发送形成回声。
5. Linux Wayland 最终使用单个原生 data-control 连接，单次 selection 只读取一种 MIME 内容。
6. 原生后端不可用或异常时必须有明确、可观测、可回滚的命令后端。
7. Windows 使用剪贴板序列号辅助去重，同时保持 Win32 窗口回调立即返回。
8. 保持现有网络协议、路由规则和配置文件向后兼容。

### 2.2 非目标

- 不实现剪贴板历史、搜索或持久化。
- 不同步文件列表、`PRIMARY` selection 或敏感内容类型。
- 不修改 MQTT、HTTP、V2 Relay 的协议格式。
- 不改变现有网络目标并行发送策略；“禁止并发”在本需求中约束剪贴板采集和本地写入路径。
- 不通过单纯增加防抖时间掩盖重复事件。
- 不要求在 v0.11 同时重写全部平台后端。

## 3. 设计约束

### 3.1 严格串行

- 系统通知可以非阻塞地进入容量为 1 的信号通道。
- 剪贴板采集只允许一个工作线程/goroutine。
- 新事件在读取期间到达时，只记录“有更新”和最新 generation，不启动第二次读取。
- 如需取消过期读取，必须先关闭并等待旧读取结束，再开始新读取。
- 单元测试必须证明 `maxConcurrentReads == 1`。

### 3.2 无 CGO 和静态构建

- Linux 原生实现不得引入 CGO 或运行时 `libwayland` 依赖。
- 必须继续支持 `CGO_ENABLED=0` 的现有发布矩阵。
- 不支持原生 data-control 的环境继续使用 X11/命令后端。

### 3.3 数据安全

- 默认日志不得输出剪贴板正文、图片数据或稳定的内容哈希。
- 调试日志如需关联同一内容，应使用进程启动时随机密钥生成的 HMAC 摘要前缀；进程重启后不可关联。
- 内容只保存在内存中，不新增磁盘缓存。
- 平台能够标识 sensitive selection 时必须跳过同步。

### 3.4 向后兼容

- 旧配置不增加任何字段也能正常运行。
- 新配置字段均有安全默认值。
- 命令后端保留至少一个稳定版本，便于现场回滚。

## 4. 方案比较

| 方案 | 优点 | 缺点 | 结论 |
|---|---|---|---|
| 增大防抖时间 | 改动小 | 增加所有复制操作延迟；无法识别窗口外或内容编码不同的重复事件 | 不采用 |
| 每个事件异步/并发读取 | 吞吐看似更高 | 会放大对 KWin/剪贴板所有者的请求，重现卡顿风险 | 禁止 |
| `wl-paste --watch` 直接把 stdin 交给辅助进程 | 比二次读取少一次访问 | MIME 信息不足；每次 selection 仍创建辅助进程；生命周期复杂 | 仅作为命令后端候选优化，不作为最终架构 |
| 直接使用 `golang.design/x/clipboard v0.8.0` 多格式 `Watch` | 纯 Go、原生 Wayland、事件驱动 | 公共 API 会按格式建立独立 watcher；同时监听文本和图片时存在多个内部读取路径 | 不直接使用多格式 `Watch` |
| 单连接原生 data-control 适配器 | 一次 offer、一次 MIME 选择、一次读取；无外部监听进程 | 需要新增协议适配和较完整的集成测试 | 最终方案 |

`golang.design/x/clipboard v0.8.0` 已包含纯 Go Wayland data-control 实现，可作为实现和测试参考。优先向上游增加“单 selection、多 MIME 选择、单连接”的接口；如果上游发布周期不能满足项目计划，则在 `internal/waylandclipboard` 中实现最小适配器，并保留原项目 MIT 许可证声明和 NOTICE 归属。

## 5. 目标架构

```text
平台事件源
  ├─ Wayland: 单个 data-control connection / selection offer
  ├─ Windows: WM_CLIPBOARDUPDATE + sequence number
  ├─ X11: xsel/xclip command fallback
  └─ macOS: NSPasteboard changeCount
          │
          ▼
非阻塞事件信号（容量 1，记录最新 generation）
          │
          ▼
唯一 clipboard worker
  1. 选择 MIME
  2. 有界、可取消地读取一次
  3. 计算内容指纹
  4. 判断重复/回声/过期事件
  5. 生成 ClipboardChange
          │
          ▼
现有 changeEvent / ForwardEngine
```

### 5.1 状态机

| 状态 | 说明 | 允许的转换 |
|---|---|---|
| `Idle` | 等待新事件 | `Reading`、`Stopping` |
| `Reading` | 正在读取一个 generation | `Processing`、`ReadingStale`、`Stopping` |
| `ReadingStale` | 读取期间出现更新；只记录最新 generation | 当前读取结束后进入 `Reading` 或 `Stopping` |
| `Processing` | 串行完成指纹、去重和回声判断 | `Idle`、`Reading`、`Stopping` |
| `Reconnecting` | 原生连接异常，按退避等待 | `Idle`、命令后端、`Stopping` |
| `Stopping` | 取消读取、关闭连接并等待 worker | 终止 |

状态由唯一 worker 持有；平台回调只更新事件信号和 generation，不直接修改处理状态。

## 6. 功能需求

### FR-01：统一处理器

- 新增平台无关的 `clipboardProcessor`。
- `clipboardProcessor` 负责串行读取调度、内容指纹、重复判断、回声判断和输出 `ClipboardChange`。
- 平台后端只负责报告 generation、可用 MIME 和提供内容读取函数。

### FR-02：事件合并

- 事件通道容量固定为 1，通知发送必须非阻塞。
- 空闲时收到事件立即安排读取；命令后端可以保留 200ms trailing-edge 防抖。
- 原生 Wayland/Windows generation 已能区分 selection 时，不默认增加 200ms 延迟。
- 读取期间收到多个事件时只保留最新 generation，当前读取结束后最多补读一次。

### FR-03：MIME 选择

一次 selection 只能选择并读取一种 MIME，优先级为：

1. `image/png`；
2. 其他可解码的 `image/*`；
3. UTF-8 `text/plain`；
4. 其他 `text/plain` 变体；
5. `UTF8_STRING`、`STRING`、`TEXT`。

未知格式、空 selection 和不支持的格式不产生同步消息。

### FR-04：内容指纹与重复判断

- 文本指纹：内容类型、长度和原始字节的 SHA-256；不得修剪空格、换行或 NUL。
- 图片首先计算原始字节指纹。
- 同一去重窗口内，图片原始指纹不同时才允许执行像素精确指纹：解码后对宽、高和规范 RGBA 像素计算 SHA-256。
- 像素完全相同才视为语义重复；禁止使用可能把相似图片误判为相同的感知哈希。
- 默认去重窗口为 5 秒，只保留最近 16 个指纹，过期自动淘汰。
- 去重只阻止重复发送，不修改真正发送的原始内容。

### FR-05：回声抑制

- `SetClipboardContentText/Image` 写入前登记内容指纹和本地写入 token。
- 后端能够提供 sequence/generation 时，回声记录必须与写入后的 generation 关联。
- 监听事件与待抑制指纹匹配时标记为 `local-echo` 并忽略。
- 同一 selection 的多次通知均应抑制，直到 selection 被其他内容覆盖或记录超时。
- 时间窗口仅作为异常兜底，不能作为唯一依据。

### FR-06：Wayland 原生后端

- 启动时检测 `WAYLAND_DISPLAY` 以及 `ext_data_control_manager_v1` 或 `zwlr_data_control_manager_v1`。
- 本机 KDE 验证基线为 `zwlr_data_control_manager_v1 v2`。
- 每个 seat 只建立一个 data-control device；当前版本只处理默认 seat。
- 一个 selection offer 收集完整 MIME 列表后，按 FR-03 选择一种 MIME 并请求一次数据。
- 内容通过协议提供的 FD 有界读取；selection 变化、超时或程序退出时关闭 FD。
- 原生连接不做周期性重启；仅在连接错误时按 1、2、4、8、16、30、60 秒上限退避重连。
- 新 selection 到达后重置退避。
- 原生后端运行时不得存在本程序启动的 `wl-paste`、`wl-copy`、`xsel` 或 `xclip` 子进程。

### FR-07：命令后端回退

- `auto` 模式下原生协议不可用或初始化失败时回退命令后端。
- 运行中的原生连接异常默认先重连，不因一次瞬时错误永久降级。
- 连续失败达到明确阈值后可以降级，但必须记录原因和后端变化。
- 命令后端继续使用 v0.10 的幂等 Stop、等待子进程退出和指数退避逻辑。
- 命令后端的周期轮换必须保证旧进程退出后才能启动新进程。

### FR-08：Windows 序列号

- `WM_CLIPBOARDUPDATE` 回调只读取 `GetClipboardSequenceNumber` 并非阻塞投递事件。
- 相同 sequence number 不重复安排读取。
- 内容读取继续由唯一 worker 串行完成。
- 剪贴板被其他程序占用时使用短退避串行重试；任意时刻仍只允许一个读取操作。
- 最终仍执行 FR-04 内容指纹去重，因为 sequence 变化不代表内容变化。

### FR-09：macOS 与 X11

- macOS 保留 `NSPasteboard changeCount` 作为 generation，接入统一处理器和内容去重。
- X11 保留命令后端作为兼容路径，接入统一处理器。
- 本需求不强制在 v0.11 重写 macOS JXA 和 X11 协议实现。

### FR-10：资源边界

- 单次读取默认超时 5 秒。
- 单次内容默认上限 128 MiB，超过上限应关闭数据 FD、记录 `content-too-large` 并跳过，不允许分配无界内存。
- 图片像素指纹只在去重窗口内出现疑似重复时计算，避免所有图片都额外解码。
- 停止服务后 5 秒内必须完成 worker、连接、FD 和子进程清理。

### FR-11：可观测性

普通日志至少包含：

- 当前 backend 和选择原因；
- backend 启动、降级、重连、停止；
- 内容类型、字节数和读取耗时；
- 跳过原因：`duplicate-raw`、`duplicate-pixels`、`local-echo`、`unsupported`、`timeout`、`content-too-large`；
- 事件合并数量；
- 当前读取并发数的调试断言。

日志不得包含正文或稳定哈希。调试模式可输出本次进程内的 HMAC 摘要前缀。

## 7. 配置需求

建议新增可选顶层配置：

```yaml
clipboard:
  backend: auto             # auto | native | command
  dedupWindowMs: 5000
  readTimeoutMs: 5000
  maxContentBytes: 134217728
  imagePixelDedup: true
```

默认值：

| 字段 | 默认值 | 说明 |
|---|---:|---|
| `backend` | `auto` | Wayland 优先原生；其他平台使用各自原生/现有实现 |
| `dedupWindowMs` | `5000` | 允许范围 0～60000，0 表示仅关闭跨事件去重，不关闭回声抑制 |
| `readTimeoutMs` | `5000` | 允许范围 500～60000 |
| `maxContentBytes` | `134217728` | 允许范围 1 MiB～1 GiB |
| `imagePixelDedup` | `true` | 仅在原始字节不同且疑似重复时解码比较 |

同时支持环境变量 `CLIPBOARD_BACKEND` 覆盖 YAML，便于无配置变更回滚。非法配置必须在启动时给出明确错误，不静默采用其他值。

## 8. 非功能需求与验收指标

### 8.1 正确性

- 同一文本或原始图片在 5 秒内产生 2～100 次事件，只发送一次。
- 像素完全相同但 PNG 元数据不同的两张图片只发送一次。
- 任意一个像素不同的图片必须发送，不得被误去重。
- 5 秒内连续复制两个不同文本或不同图片，两者都必须发送。
- 远端写入本地后不得产生出站回声。
- 启动时不得自动发送启动前已有的剪贴板内容。

### 8.2 串行保证

- 单元、race 和集成测试观测到的最大剪贴板读取并发数必须等于 1。
- Windows 窗口回调和 Wayland 事件接收函数内不得读取、解码或发送内容。
- 事件积压时内存中不得形成无界队列。

### 8.3 KDE Wayland 验收

- 连续执行 100 次 Spectacle 截图，桌面无可感知冻结。
- 测试期间 `journalctl --user` 中新增的 `DataControlOffer: timeout reading from pipe` 数量为 0。
- 每次语义不同的截图只产生一条本地 `ClipboardChange` 和一组目标发送。
- 原生后端下进程树中没有本程序启动的 `wl-paste`。
- 24 小时 soak test 后仍只有一个 Wayland data-control 连接，无 FD、goroutine 或内存持续增长。

### 8.4 性能与稳定性

- 空闲 CPU 平均占用低于 0.1%。
- 非阻塞平台回调在 10ms 内返回（不含系统调度异常）。
- 默认大小限制内的内容不得因内部固定缓冲区被截断。
- backend 连接断开后应自动恢复；退避期间不忙循环。
- `go test -race ./...` 不出现数据竞争。

### 8.5 跨平台构建

- Linux amd64 实机测试通过。
- Linux 发布矩阵在 `CGO_ENABLED=0` 下全部构建通过。
- Windows amd64/386/arm64 构建通过，Win10 KVM 实机测试通过。
- macOS amd64/arm64 构建和现有集成测试通过。

## 9. 测试计划

### 9.1 单元测试

1. 密集事件合并，handler 最大并发为 1。
2. 读取期间到达事件，只补读最新 generation 一次。
3. 原始内容指纹去重和窗口过期。
4. 相同像素、不同 PNG 编码的精确去重。
5. 单像素不同图片不去重。
6. 本地写入回声抑制；真实新内容不被误抑制。
7. 超时、超限、取消和关闭路径。
8. backend 自动选择、显式选择和非法配置。
9. 重连退避增长、上限和成功重置。
10. Stop 幂等且等待所有资源退出。

### 9.2 协议级测试

使用 fake Wayland connection/offer 覆盖：

- offer 包含多个文本和图片 MIME 时只请求一个优先 MIME；
- selection 清空；
- offer 在读取中被新 selection 替换；
- 数据 FD 部分读取、超时、提前关闭和超限；
- compositor 断开与重连；
- 停止时仍有未完成读取。

### 9.3 Linux 实机测试

- KDE Wayland：文本、Spectacle 全屏/区域截图、大图、连续截图。
- 命令后端强制模式：验证现有 `wl-paste` 生命周期不回归。
- X11/XWayland：文本和图片读写。
- 对每个场景记录事件数、读取数、发送数、耗时和 KWin 日志。

### 9.4 Windows 实机测试

- 在 Win10 KVM 中测试文本、截图、大图和剪贴板短暂被占用。
- 验证 `GetClipboardSequenceNumber` 相同事件被跳过。
- 验证窗口回调立即返回，处理器最大并发为 1。
- 验证远端写入不回声。

### 9.5 回归测试

- `go test ./...`
- `go test -race ./...`
- `go vet ./...`
- Linux 真实 Wayland gated integration test。
- Windows amd64/386 测试包交叉编译与构建。
- GitHub Actions 现有 Linux、Windows、macOS integration workflows。

## 10. 实施计划

### 阶段 A：v0.11 统一处理层

1. 新增 `ClipboardConfig` 及校验、默认值和示例配置。
2. 将现有 `consumeClipboardEvents` 演进为拥有 generation/dirty 状态的 `clipboardProcessor`。
3. 实现有界指纹缓存、图片条件像素指纹和进程内 HMAC 日志标识。
4. 将远端写入抑制从纯时间判断改为内容指纹 + generation/token。
5. Windows 接入 `GetClipboardSequenceNumber`。
6. Linux 命令后端、Windows、macOS 接入统一处理器。
7. 增加单元测试、race 测试和跨平台构建。
8. 发布候选版本，在当前 KDE 和 Win10 KVM 进行 A/B 验证。

阶段 A 本身不替换 Linux 后端；本次 v0.11 在阶段 A 完成后继续一次性交付阶段 B。

### 阶段 B：v0.11 原生 Wayland

1. 做最小技术验证：单连接发现 manager/seat、接收 offer、选择 MIME、读取 FD、取消和重连。
2. 优先向 `golang.design/x/clipboard` 上游实现单 selection watcher；评估完成前不直接采用多格式 `Watch`。
3. 如上游方案不能满足交付周期，在 `internal/waylandclipboard` 实现最小纯 Go 适配器并补充许可证归属。
4. 新增 `native` backend；按本次一次性交付决定，Linux Wayland 的 `auto` 直接优先解析为 `native`。
5. 发布部署后执行 100 次截图验收，并启动 24 小时 soak test 持续观察资源增长。
6. 保留 `command` 回退，并验证一键回滚。
7. 更新 README、测试文档和故障排查说明。

### 阶段 C：发布与部署

1. 在 GitHub 主仓库当前 `main` 提交并 push。
2. 创建版本标签，等待 GitHub Release 全平台构建完成。
3. 下载并校验 Linux amd64 和 Windows amd64 官方资产。
4. 备份并更新 Linux 运行文件，确认 backend、版本、进程树和日志。
5. 通过 QEMU Guest Agent 备份并更新 Win10 二进制，确保程序运行于交互用户 Session。
6. 使用 `rsync -a --exclude='.git/' --exclude='.git'` 从 GitHub 主仓库同步到 `/data/code/private/clipboard-sync`。
7. 在私有仓库检查差异、提交并 push；不制造空提交。

## 11. 回滚方案

1. 运行时设置 `CLIPBOARD_BACKEND=command` 并重启服务，可跳过原生 Wayland 后端。
2. 保留前一版本二进制，运行异常时恢复并重启。
3. 原生连接失败不得删除或覆盖用户剪贴板内容。
4. 新配置全部可省略；回滚旧二进制时应忽略未知顶层字段或在部署时同步恢复配置备份。
5. Windows 更新继续保留带版本和日期的旧二进制备份。

## 12. 风险与缓解

| 风险 | 影响 | 缓解措施 |
|---|---|---|
| data-control 在不同 compositor 上实现差异 | 原生后端不可用或行为不同 | 能力探测、命令回退、fake protocol 测试、KDE 实机测试 |
| 图片像素指纹增加 CPU/内存 | 大图处理变慢 | 仅疑似重复时串行计算；内容上限；可配置关闭 |
| 去重窗口误吞用户重复复制 | 重复动作不再触发远端消息 | 远端状态未变化；不同内容绝不去重；窗口可配置 |
| 上游库 API 不满足单连接要求 | 阻塞原生方案 | 上游 PR 与内部最小适配器双路径，不直接使用并发多 watcher |
| 原生读取卡住 | 后续内容无法处理 | 可取消 FD、5 秒超时、最新 generation dirty 标记、重连 |
| 回声指纹失效 | 设备间循环同步 | sequence/generation + 内容双重判断，保留短时间兜底 |
| 调试日志泄露内容特征 | 隐私风险 | 不记录正文和稳定哈希；只使用进程内随机 HMAC 标识 |

## 13. 完成定义

只有满足以下条件，需求才视为完成：

- 所有 FR 项均有实现或明确记录为后续版本范围。
- 自动化测试证明剪贴板读取最大并发数为 1。
- KDE 100 次截图无冻结、无新增 data-control timeout、无重复发送。
- Win10 KVM 文本和图片同步正常，无回声和消息循环阻塞。
- 原生 Wayland 24 小时 soak test 无资源增长。
- 命令后端回滚已验证。
- GitHub Release 全平台成功，正式资产哈希已验证。
- GitHub 主仓库完成后，按约定通过 rsync 排除 `.git` 同步并提交私有仓库。

## 14. 参考资料

- [`golang.design/x/clipboard v0.8.0` README](https://github.com/golang-design/clipboard/blob/v0.8.0/README.md)
- [`golang.design/x/clipboard v0.8.0` 多格式 Watch 实现](https://github.com/golang-design/clipboard/blob/v0.8.0/clipboard.go)
- [wlr data-control protocol](https://wayland.app/protocols/wlr-data-control-unstable-v1)
- [`wl-clipboard --watch` 官方项目](https://github.com/bugaevc/wl-clipboard)

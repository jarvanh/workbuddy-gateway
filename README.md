# WorkBuddy Local Gateway

纯 Go、零 CGO 依赖的跨平台单二进制网关，支持国内站与国际站、多账号调度、OpenAI / Anthropic 协议及可选网页控制台。

## 快速开始

基于腾讯 **CodeBuddy** 协议开发的**纯 Go、零 CGO 依赖、跨平台单二进制**本地 AI 代理网关。默认无 Web UI，全部通过命令行（CLI）完成登录、凭据续期与服务控制；可选开启网页管理台（`-webui`，见下文）。

**v1.14.0**：新增配置热生效、全局账号与模型网页开关、凭据 JSON 编辑、端口设置及日志保留/清理。旧配置缺省日志保留天数时不自动删除；端口修改仍需重启。请先升级二进制，再添加新增配置字段。

**v1.14.1**：网页凭据页已移除，新增只读定时任务时间页；执行记录来自现有后端日志，无新增调度模块。高频任务日志按分钟汇总，状态变化即时记录。

**同时支持两个上游站点**（同一套 `/v2/plugin/*` 协议，凭据按站点隔离，账号池可混挂轮询）：

| 站点 | 上游 | 登录方式 | 登录命令 |
|---|---|---|---|
| 国内站 | `copilot.tencent.com` / `www.codebuddy.cn` | 微信 / 企业微信扫码 | `login` |
| 国际站 | `www.workbuddy.ai` | 浏览器内登录（邮箱 / 验证码 / SSO） | `login -intl` |

---

## 目录

- [核心特性](#核心特性)
- [命令总览](#命令总览)
- [serve](#serve)
- [login](#login)
- [status](#status)
- [refresh](#refresh)
- [monitor](#monitor)
- [webui（网页控制台）](#webui网页控制台)
- [probe](#probe)
- [warmup（5 小时窗口主动触发）](#warmup5-小时窗口主动触发)
- [reset](#reset)
- [restricted（上游风控限制处置）](#restricted上游风控限制处置)
- [version / help](#version--help)
- [多账号池](#多账号池)
- [模型列表与倍率](#模型列表与倍率)
- [站点路由](#站点路由)
- [客户端接入](#客户端接入)
- [各平台部署](#各平台部署)
- [安全提示](#安全提示)
- [从源码构建](#从源码构建)

---

## 核心特性

- **国内 / 国际双站反代**：两个站点走同一套协议，凭据通过 `edition` 字段区分，刷新与对话自动路由到各自上游。
- **模型完全透传**：客户端传什么 `model` 就原样中继到上游，无白名单限制。`/v1/models` 仅用于客户端自动补全，不影响实际转发。
- **模型列表双来源合并**：实时接口 + npm 静态目录，按 ID 去重、接口优先；失败用本地缓存，两边都失败且无缓存时该站点本轮不展示模型（不影响调用）。
- **模型倍率与价格探测**：促销生效时展示 `credits` × factor；促销过期或接口无有效倍率时由同站点账号实测（启动即探测、重置后立即探测、每模型 12 小时一轮）；探测选号须满足保底余额 `minBalanceGuard`，免费模型不受此限。
- **后台价格探测默认关闭**：仅显式传入 `-allow-price-probes` 才开启后台实测循环（真实生成请求可能消耗积分）；正常客户端请求照常根据 `usage.credit` 学习免费 / 收费状态，不受此开关影响。
- **5 小时窗口主动触发（本 fork）**：`warmup` 段独立于上面的开关，默认开启，按价格上限筛选模型各打一次最小请求以维持窗口活跃。
- **多账号池 + 轮询负载均衡**：`-auth` 逗号分隔或 `-auth-dir` 目录，请求按 round-robin 分发；国内站与国际站账号可混挂。
- **上游错误重试与账号回退**：网络层瞬时错误（EOF、连接复用失效等）先对同一账号原地重试（默认 3 次），仍失败自动回退账号池下一个账号；上游 `408/5xx` 瞬时错误、非流式聚合期间的流中断同样跨账号代偿，全部账号都失败才向客户端返回 502，绝不把截断内容伪装成完整结果。
- **403 语义细分**：仅当响应体确认鉴权失效（401 / invalid token / 登录过期等）才禁用账号并删除凭据文件；无鉴权失败特征的 `403`（疑似 WAF/CDN 拦截页）只短冷却 60 秒并回退换号，避免误删凭据。
- **模型级隔离**：`6004` 只冷却触发它的账号 + 模型，`14018` 只阻断该账号的当前收费模型，不再因为一个模型拖垮整个账号。
- **免费站点优先**：同一模型若「一个站点免费、另一个站点收费」，优先使用免费站点账号直至其受限；两个站点都收费（仅倍率不同）时不做倾斜，正常轮询。
- **免费/收费学习**：按「账号 + 模型」从响应 `usage.credit` 学习；`credit=0` 且样本足够（`total_tokens ≥ 100`）才判定免费，避免小样本误判。
- **每日自动签到与 Buddy 旅行**：国内站每日签到后默认自动派 Buddy 旅行（到达自动领取奖励、空闲自动随机地点派遣）、跑成长任务并用补登卡自动补断登（保住连登奖励），国际站默认自动签到；均可通过 config.json 的 `checkin` 段分别关闭。
- **凭据热加载（免重启）**：默认每 5 秒扫描凭据来源，新增 / 更新 / 删除凭据免重启生效。
- **授权失效自动禁用**：401/403 / `invalid token` / 登录过期时禁止调度、删除凭据文件并写入失效标记，重新 `login` 后自动恢复。
- **后台自动续期**：每 5 分钟检查 Token，距过期不足 15 分钟自动刷新并写回凭据文件；另可按 `-keepalive-hours` 在固定时刻主动刷新全部账号（默认每天 22:00），避免长期空闲账号静默失效。
- **流式分片规范化**：把上游每个分片携带的 `finish_reason:""` 归一化为 `null`，避免 Anthropic 翻译层误判 `stop_reason` 导致工具不执行。
- **工具调用序列自愈**：出站前按 `tool_call_id` 修复并行调用中夹入 message 的历史结构，合并 Responses API 拆散的并行调用，并删除无配对调用、孤儿或重复结果，避免国际站返回 `11148 tool_call_sequence_broken`。
- **OpenAI 兼容协议**：`/v1/chat/completions`（SSE 流式 + 非流式聚合）、`/v1/responses`（Responses API）、`/v1/models`、`/health`。
- **Anthropic 协议原生入口**：`/v1/messages`（SSE 流式 + 非流式聚合）与 `/v1/messages/count_tokens`，Claude Code 等 Anthropic 客户端免外部翻译层直连，鉴权兼容 `x-api-key` 头。

---

## 命令总览

```text
workbuddy-gateway [command] [options]

命令:
  serve     启动本地网关（默认命令，不带子命令时等同 serve）
  login     登录并获取 / 更新凭据
  status    查看账号池状态
  refresh   手动刷新所有账号访问令牌
  monitor   前台实时监控：账号表格 + 模型统计附表 + 最近日志
  probe     主动探测账号对指定模型的免费 / 收费属性（需 serve 运行中）
  warmup    5 小时窗口主动触发：开关 / 时刻 / 价格上限 / 指定模型（需 serve 运行中）
  reset     清空除登录凭据外的全部本地数据，并重新拉取模型与倍率
  version   查看版本信息
  help      查看帮助
```

全局选项（对所有命令可用）：

| 选项 | 默认 | 说明 |
|---|---|---|
| `-addr <ip>` | `127.0.0.1` | 网关监听地址 |
| `-port <port>` | `8317` | API 监听端口；显式传入时覆盖 `config.json` 的 `gateway.apiPort` |
| `-auth <path>` | 自动发现 | 凭据文件路径，支持逗号分隔多个 |
| `-auth-dir <dir>` | 空 | 凭据目录，自动加载目录内所有 `workbuddy*.json` |
| `-proxy <url>` | 空 | 上游请求代理，如 `http://127.0.0.1:7890`、`socks5://...` |
| `-verbose` | `false` | 输出详细调试日志 |
| `-intl` | `false` | 仅 `login` 生效：登录国际站 |
| `-reload-interval <sec>` | `5` | 凭据热加载扫描间隔，`0` 关闭 |
| `-models-refresh <min>` | `60` | 模型目录刷新间隔，`0` 关闭 |
| `-allow-price-probes` | `false` | 允许后台自动价格探测；会发起真实模型生成请求，可能消耗积分。默认关闭，不影响正常请求学习、目录刷新及显式 `probe` 命令 |
| `-webui` | `false` | 启用独立网页控制台（默认 `http://127.0.0.1:8316/ui/`）；管理 Key 与模型 API 鉴权由 `config.json` 配置 |

**价格探测默认关闭（含旧版本升级）**：只有显式指定 `-allow-price-probes` 或 `-allow-price-probes=true` 才开启后台价格探测。旧启动脚本中的 `-disable-price-probes` 仍可使用；两个参数同时传入且旧禁用参数为 `true` 时，禁用优先，与参数顺序无关。仅传入 `-disable-price-probes=false` 不会开启探测。此开关是启动参数，修改后需重启服务。

```bash
go build -o workbuddy-gateway .
./workbuddy-gateway login
./workbuddy-gateway serve -webui
```

Windows 可使用 `.\workbuddy-gateway.exe`。管理端口默认 `8316`，模型 API 默认 `8317`。

- 首次启动会在工作目录生成完整的默认 `config.json`；旧配置自动补齐缺失的 `identity`、`concurrent` 配置，并补入默认的 `workspace root → workspace base`、`are paths the user → are paths that the user` 兼容替换，已有自定义值不覆盖。默认单账号模型并发 `4`，全局不限；修改 `concurrent.MaxAcc/MaxGlobal` 可热加载。
- **后台价格探测默认关闭**，只有显式传入 `-allow-price-probes` 才开启；主动生成请求可能消耗积分。
- 网页配置页支持风险确认后的完整 JSON 编辑；API/Web 监听端口修改可热切换，已有连接与流继续完成。新端口被占用或配置无法应用时保留旧运行状态；显式 `-port` 仍优先于配置文件。
- `config.json` 和账号凭据含敏感信息，已由 Git 忽略，不要公开分享。
- 多 Harness 可使用 `upstream.requestProfile: "official"` 开启统一出站兼容；旧配置省略该项保持原行为。见[出站兼容说明](docs/outbound-compatibility.md)。

## 文档

| 内容 | 位置 |
|---|---|
| 完整使用手册、配置、客户端接入与部署 | [docs/user-guide.md](docs/user-guide.md) |
| 目录职责与命名约定 | [docs/project-layout.md](docs/project-layout.md) |
| 贡献者及方案署名 | [docs/contributors.md](docs/contributors.md) |
| 测试入口与说明 | [tests/README.md](tests/README.md) |
| 多 Harness 出站对齐、协议参数与兼容边界 | [docs/outbound-compatibility.md](docs/outbound-compatibility.md) |
| DSH 历史回放、GUI 环境句与 11128 兼容说明 | [docs/11128-dsh-history.md](docs/11128-dsh-history.md) |

## 构建与测试

```bash
# 默认监听 127.0.0.1:8317，自动加载当前目录下所有 workbuddy*.json
workbuddy-gateway serve

# 自定义端口与监听地址
workbuddy-gateway serve -port 9000 -addr 0.0.0.0

# 显式指定多个凭据文件（逗号分隔，轮询）
workbuddy-gateway serve -auth workbuddy.json,workbuddy2.json

# 目录模式：加载目录内所有 workbuddy*.json
workbuddy-gateway serve -auth-dir ./auths

# 上游走代理 + 详细日志（客户端鉴权在 config.json 的 gateway 段配置）
workbuddy-gateway serve -proxy http://127.0.0.1:7890 -verbose

# 关闭凭据热加载
workbuddy-gateway serve -reload-interval 0

# 关闭模型目录自动刷新
workbuddy-gateway serve -models-refresh 0
```

启动后提供的端点：

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/v1/chat/completions`、`/chat/completions` | Chat Completions，支持 SSE 流式与非流式 |
| POST | `/v1/responses`、`/responses` | OpenAI Responses API |
| POST | `/v1/messages` | Anthropic Messages API（Claude Code 直连，内部转 Chat 走同一条上游管线） |
| POST | `/v1/messages/count_tokens` | Anthropic 令牌计数（本地估算，CJK 1 token/字、ASCII 4 字符/token） |
| GET | `/v1/models`、`/models` | 模型列表，响应头 `X-Model-Source` 标注来源 |
| GET | `/health`、`/ping` | 健康检查，返回 `version`、`model_count`、`model_source` |
| POST | `/admin/probe` | 供 `probe` 命令调用，**仅接受回环来源** |
| GET \| POST | `/admin/warmup` | 5 小时窗口触发状态查询 / 立即执行一轮，**仅接受回环来源** |
| POST | `/admin/reload` | 让运行中进程重读 `config.json`，**仅接受回环来源** |
| GET | `/ui/` | 网页控制台静态页面（独立端口，仅 `-webui` 启用时存在） |
| GET | `/admin/api/*` | 网页控制台数据接口（独立端口，仅 `-webui` 启用时存在，需管理 Key） |
| GET | `/` | 简单文本说明 |

从 v1.13.15 起，Responses `function_call_output.output` 支持文本和图片内容块数组：
文本原样保留在 `tool` 结果中，`input_image` 图片提升为全部配对工具结果之后的
`user` 多模态消息，图片 URL 和数据不变。图片不再被序列化成 Base64 文本混入
上下文，避免截图会话异常消耗大量 token，最终触发 `11133 model_param_invalid`。
并行工具调用的结果仍连续排列，不会因插入图片而破坏配对。普通字符串和 JSON
工具结果保持原行为；网关不截断操作手册或会话历史。`11133` 本身是上游通用参数
错误码，不仅限于图片或上下文长度问题；排障仍需结合模型、TraceID 与日志。

从 v1.13.17 起，网关在客户端未声明输出预算（`max_tokens` 与 `max_completion_tokens`
都不带有效值）时，按模型目录声明的输出上限补一个 `max_tokens`。上游按「输入 token +
输出预算」判断是否超出模型窗口，而请求不带该字段时上游会为输出默认预留 384000：
标称 1048576 窗口的 `deepseek-v4.1-flash` 实际只剩约 664576 可用输入，长会话会在远未
到窗口上限时被 `11133 model_param_invalid` 拒绝，表现为「聊着聊着就 400」。实测
678912 输入不带预算返回 `400/11133`，带 `max_tokens=128000` 返回 `200`。客户端显式
传入的值一律不覆盖；目录未收录的模型保持原行为，网关不做猜测。注意可用输入与输出上限
共用同一窗口（可用输入 = 窗口 − 声明的输出预算），需要更长输入时应让客户端声明更小的
输出预算，而不是更大。

后台任务（`serve` 启动后按启用条件运行）：

| 任务 | 周期 | 说明 |
|---|---|---|
| Token 续期检查 | 5 分钟 | 距过期不足 15 分钟自动刷新 |
| 额度扫描 | 5 分钟 | 每凭据 10 秒超时，超时保留旧值；`剩余=0` 标记付费耗尽 |
| 模型目录刷新 | 60 分钟 | 实时接口 + npm 目录，合并去重后写缓存 |
| 模型价格探测 | 默认关闭；开启后首轮约 20 秒，随后 2 或 30 分钟；同站点同模型至少间隔 12 小时 | 仅 `-allow-price-probes` 开启；无探测记录时单轮最多 30 个，之后最多 5 个；真实生成请求可能消耗积分 |
| 5 小时窗口主动触发（本 fork） | 每天 `warmup.time`（默认 `04:00`） | 按价格上限筛选模型各打一次最小请求；`warmup` 段默认开启 |
| 每日主动探测（本 fork） | 跟随 warmup 时刻，或独立 `06:00` | warmup 关闭时按 `probe.schedule.time` 执行 |
| 每日签到与 Buddy 旅行（本 fork） | 每天 `UTC+8 09:00` | 国内站签到+Buddy旅行、国际站签到；`checkin` 段可分别关闭 |
| 状态快照 | 3 秒 | 写 `workbuddy-status.json` 供 `monitor` 读取 |
| 凭据热加载 | 5 秒 | 扫描凭据新增 / 更新 / 删除 |

**上游超时策略**

网关不再对上游请求设置「整条流的总超时」，避免长时间但持续有输出的流被中途掐断（会丢失 usage、返回残缺内容，并被上游代理判为故障触发 503）：

| 阶段 | 策略 |
|---|---|
| 连接与响应头 | `ResponseHeaderTimeout` 默认 300 秒。上游对 3MB+ 大请求排队+预处理可能接近 1 分钟（实测 2.3MB 请求曾需 50.8s 才回响应头），因此默认放宽到 5 分钟兜底 |
| 流式响应体 | 空闲读超时默认 120 秒：持续有数据就永不超时，只有 120 秒无任何新数据才判定卡死并中断 |
| 控制类短请求 | 令牌刷新 / 额度查询 / 模型目录等 60 秒超时 |
| 服务端写响应 | 不设总时长上限（原 300 秒），长流不会被服务端截断 |
| 网络层瞬时错误 | 同一账号先原地重试（默认 3 次，间隔 500ms），仍失败回退账号池下一个账号；客户端已断开则立即终止 |

**上游错误重试与账号回退**

| 错误类型 | 处理策略 |
|---|---|
| 网络层错误（EOF、`use of closed network connection` 等） | 同一账号原地重试 `networkRetries` 次（默认 3）→ 回退下一个账号 → 全部失败返回 502 `upstream_network_error` |
| 上游 `408 / 5xx` 瞬时错误 | 回退下一个账号代偿 → 全部失败返回 502 `upstream_error` |
| `429` / `6004` 频率限制、`14018` 额度耗尽 | 冷却/阻断后回退下一个账号（原有逻辑） |
| `401` 或带鉴权失败特征的 `403`（invalid token / 登录过期等） | 禁用账号并删除凭据文件 → 回退下一个账号（原有逻辑） |
| 无鉴权失败特征的 `403`（疑似 WAF/CDN 拦截页） | 仅冷却 60 秒 → 回退下一个账号，不禁用账号、不删凭据 |
| 其余 `4xx`（参数错误等请求级问题） | 原样透传，不重试不回退（换账号无意义） |
| 非流式聚合期间流中断 | 聚合完成前未向客户端写出任何字节 → 回退账号池重新请求（最多 3 次），绝不返回截断内容 |
| 流式传输中途断流 | 已有部分内容发出，无法透明重试 → 下发 `upstream_stream_interrupted` 明确错误事件（不伪造正常结束） |

两个上游超时与重试次数可在工作目录 `config.json` 的 `upstream` 段覆盖（超时单位秒，省略或非正数则用默认值；`networkRetries` 省略用默认值，显式 `0` 关闭原地重试；`earlyFlushGraceSeconds` 省略用默认 30 秒，显式 `0` 完全禁用响应头先行）：

```json
{
  "upstream": {
    "headerTimeoutSeconds": 300,
    "idleTimeoutSeconds": 120,
    "transientRetries": 2,
    "earlyFlushGraceSeconds": 30
  }
}
```

**响应头先行（early flush）**

客户端经 Cloudflare Tunnel 等反向代理访问时，CDN 边缘对「已建连但迟迟拿不到响应头」的请求有 ~100 秒硬超时（HTTP 524）。实测大上下文请求 + 并发排队时，账号调度与上游首字延迟叠加会把首字节时间（TTFB）推过 100 秒：请求明明活着，却被边缘掐断。

流式请求启用响应头先行：宽限期内失败仍返回真实 HTTP 状态码（下游可按码重试）；宽限期耗尽仍没等到上游响应，网关提前下发 `200 + text/event-stream` 响应头，CDN 的等待计时随之停止。此后上游若失败，降级为 SSE `error` 事件（与流中断同一红线：不伪造 `[DONE]`）；若成功则照常透传流，客户端无感。仅流式路径启用；非流式在本地聚合，不适用。

**瞬时网络错误重试**

网关与上游 CDN 边缘节点之间的单条 TCP 连接可能被对端重置（`connection reset by peer`）、被关闭（`use of closed network connection`），或命中已被回收的 keep-alive 连接。这类错误属于瞬时故障，与请求体大小无关（实测 >5MB 请求 95% 成功，而 0.5MB 请求也会偶发失败）。

只有**请求头尚未写出**时，才能确认上游不可能处理本次 POST，网关才对瞬时连接错误重试。`Do` 返回 EOF、RST 或超时但请求头已写出时，上游可能已经收到并处理请求；即使尚未收到响应头，也不会自动重放，以免重复生成。

| 项 | 默认 | 说明 |
|---|---|---|
| 重试次数 | 2 | `upstream.transientRetries` 可覆盖；显式设为 `0` 可禁用 |
| 重试间隔 | 300ms | 给上游边缘节点留出恢复时间 |
| 重试范围 | 请求头未写出时的瞬时错误 | 包括建连阶段 EOF、RST、`broken pipe`、`connection refused` 等 |
| 不重试 | — | 请求头已写出的 EOF/RST/超时（执行结果不确定）、客户端取消、上游 HTTP 错误响应 |
| 连接处理 | 新连接 | 重试时设置 `Connection: close` 并 `CloseIdleConnections()`，避免复用坏连接 |

重试会在日志中留下明确记录：

```text
[网络重试] traceId=... requestId=389 账号=example.json 第 1/2 次重试，上一尝试请求头未写出，已重建连接
```

启动横幅会打印生效值，便于确认。流被中断时不会伪造 `[DONE]`（chat）或 `response.completed`（Responses），而是下发明确的 `upstream_stream_interrupted` / `response.failed` 错误事件，避免下游把残缺输出当成完整结果。

### 签到与 Buddy 旅行

默认行为（均可关闭）：

| 站点 | 自动化 | 默认 |
|---|---|---|
| 国内站 | 每日签到 | 开启 |
| 国内站 | 签到后派 Buddy 旅行（到达自动领取奖励 → 空闲自动随机地点派遣） | 开启 |
| 国内站 | 断登自动补签（成长中心补登卡补当月零分断点） | 开启 |
| 国际站 | 每日自动签到 | 开启 |

```json
{
  "checkin": {
    "cn": { "enabled": true, "travel": true, "growth": true, "makeup": true },
    "intl": { "enabled": true }
  }
}
```

说明：

- 签到在服务启动、凭据热加载与每天 `UTC+8 09:00` 自动触发；「今日已签」按幂等成功处理。
- Buddy 旅行是独立活动，与签到结果解耦：签到成功或今日已签都会尝试执行旅行流程。
- 旅行接口固定走官方活动域名 `www.workbuddy.cn`，与对话上游不同源。
- 账号尚未领取 Buddy（猫猫）时不自动领取（首次领取涉及官方新手任务与协议确认），日志会提示到 [官方成长中心](https://www.workbuddy.cn/profile/growth-center) 手动领取，之后即可全自动派遣。
- 旅行中 / 今日派遣达上限时自动跳过，不做重复操作。
- 断登自动补签用成长中心补登卡补当月零分断点：待补日期只信服务端日历（`heatmap.today` + `score==0`，已补登记录不算待办），每轮最多 1 张（官方补登卡上限 4 张，留余量给真实断登），补登请求不自动重试；无卡、日历结构异常或上游返回「无需补登」时不出卡，本轮结论随签到通知推送（`checkin` 事件，见「事件告警」）。

---

## login

登录并保存凭据。国内站输出终端 ASCII 二维码；国际站在浏览器内完成。

```bash
# 国内站（微信 / 企业微信扫码）
workbuddy-gateway login

# 保存到指定文件（多账号推荐）
workbuddy-gateway login -auth workbuddy2.json

# 国际站（浏览器内完成，邮箱 / 验证码 / SSO）
workbuddy-gateway login -intl
workbuddy-gateway login -intl -auth workbuddy-intl.json
```

说明：

- 默认保存到 `workbuddy.json`；`-auth` 可指定其他路径。
- 国际站凭据写入 `edition: "intl"`，与国内站凭据可混挂在同一账号池。
- 重新登录会覆盖原凭据并自动清除该账号的失效标记，无需重启服务（热加载会生效）。

---

## status

查看账号池状态，包含站点、冷却、额度与 Token 过期时间。

```bash
workbuddy-gateway status
```

输出示例：

```text
================== WorkBuddy 账号池状态 ==================
账号总数: 2

--- 账号 #1 ---
凭据文件:     workbuddy.json
站点:         国内站 (copilot.tencent.com)
用户昵称:     user-a
用户 UID:     uid-xxx
企业 ID:      (个人账号)
认证域名:     www.codebuddy.cn
冷却状态:     可用
Token 状态:   有效
过期时间:     2026-09-22 12:32:07 (剩余 119h30m0s)
```

---

## refresh

立即刷新所有账号的 Access Token（正常情况下由后台每 5 分钟自动检查，无需手动执行）。

```bash
workbuddy-gateway refresh
```

- 成功 / 失败 / 跳过（授权失效）会分别统计。
- 刷新失败若属于授权类错误，会禁用该账号并删除凭据文件。

---

## monitor

前台实时监控，周期刷新展示「账号表格 + 模型统计附表 + 最近日志」，`Ctrl+C` 退出。

```bash
# 必须在 serve 的工作目录执行（读取 workbuddy-status.json）
cd /opt/workbuddy-gateway
workbuddy-gateway monitor

# 附加展示 systemd 服务最近日志（Linux）
workbuddy-gateway monitor -journal workbuddy-gateway

# 附加展示指定日志文件
workbuddy-gateway monitor -logfile /var/log/workbuddy-gateway.log

# 调整刷新间隔与日志行数
workbuddy-gateway monitor -interval 2 -lines 20
```

| 选项 | 默认 | 说明 |
|---|---|---|
| `-interval <sec>` | `3` | 状态刷新间隔 |
| `-journal <svc>` | 空 | 同时展示 `journalctl -u <svc>` 最近日志 |
| `-logfile <path>` | 空 | 同时展示指定日志文件末尾内容 |
| `-lines <n>` | `15` | 每次展示的日志行数 |

**账号表格**

```text
账号池: 共 2 个 | 可用 1 | 冷却 0 | 付费耗尽 1 | 过期 0 | 失效 0
+------+----------------+------------+--------+------------+---------------------+----------+----------+--------+------------+------------+------------+
| 序号 | 凭据文件       | 账号       | 站点   | 状态       | Token 有效期        | 总额度   | 已用     | 剩余   | 套餐       | 免费模型   | 模型冷却   |
+------+----------------+------------+--------+------------+---------------------+----------+----------+--------+------------+------------+------------+
| 1    | workbuddy.json | user-a     | 国内站 | 可用       | 2026-09-22 12:32:07 | 2300     | 1200     | 1100   | pro        | 2          | 0          |
| 2    | workbuddy2.json| user-b     | 国际站 | 付费耗尽   | 2027-09-05 01:57:00 | 1100     | 1100     | 0      | Pro试用    | 2          | 0          |
+------+----------------+------------+--------+------------+---------------------+----------+----------+--------+------------+------------+------------+
```

状态取值：`可用`、`冷却`、`付费耗尽`、`已过期`、`失效`。

“免费模型”列按账号所属站点，统计下方模型统计附表中对应倍率为 `0.00x` 的模型数量，不再统计该账号自己的实测免费记录。该数量仅用于展示，不代表该账号一定可调用这些模型；实测账本、`probe`、账号调度和计费判断仍保持原有逻辑。

套餐取值：`Pro试用`（上游 `ProTrialStatus=1`）、`pro`（上游 `IsPaidUser=true`）、`免费`（其余，含识别不出）。

**模型统计附表**

```text
模型统计 (来源 live-api@2026-09-17 14:57):
+----------------------------+------------------+------------------+----------+----------+---------------+-----------------+------------+
| 模型                       | 国内倍率         | 国际倍率         | 可用账号 | 请求     | 平均首字(5h)  | 平均总耗时(5h)  | 总Token(M) |
+----------------------------+------------------+------------------+----------+----------+---------------+-----------------+------------+
| hy3                        | 0.00x            | 0.00x            | 8        | 3        | 1.9s          | 2.3s            | 0.12M      |
| deepseek-v4.1-flash        | 0.03x            | 0.00x            | 5        | 12       | 820ms         | 3.4s            | 1.75M      |
| hy4-preview                | 0.00x            | 收费(倍率未知)   | 8        | 4        | 1.3s          | 4.1s            | 0.00M      |
+----------------------------+------------------+------------------+----------+----------+---------------+-----------------+------------+
```

| 列 | 含义 |
|---|---|
| 模型 | 模型 ID |
| 国内倍率 | 国内站生效倍率（`credits` × 促销 factor）；免费显示 `0.00x`，促销过期或接口无有效倍率时显示 `-`，实测确认收费显示 `收费(倍率未知)` |
| 国际倍率 | 国际站同上 |
| 可用账号 | 当前可调度该模型的账号数（已计入账号冷却、模型冷却、模型额度阻断） |
| 请求 | 客户端请求次数 |
| 平均首字(5h) | 最近 5 小时滚动窗口内的平均首字响应时间（TTFT），按小时分桶、自动淘汰过期样本 |
| 平均总耗时(5h) | 最近 5 小时滚动窗口内的平均总耗时 |
| 总Token(M) | 进程启动后累计的上游 usage token，单位百万；优先 `usage.total_tokens`，没有则用 `prompt_tokens + completion_tokens`；上游未返回 usage 的请求不估算 |

---

## webui（网页控制台）

默认关闭。显式 `-webui` 后在 `gateway.webPort`（默认 `8316`）提供独立网页控制台，用于管理账号池开关、模型列表开关，查看和清理日志、预览及编辑 `config.json`、查看定时任务记录，并在「访问设置」页切换模型 API Key 校验或编辑端口。网页不展示凭据页面。它与模型 API 端口分离，未启用时管理端口不监听。

```bash
# 只有显式 -webui 才开启网页控制台；模型 API 仍监听 8317
workbuddy-gateway serve -webui

# 方式一：本机或 SSH 本地端口转发打开页面，一键生成管理 Key
ssh -L 8316:127.0.0.1:8316 root@服务器
# 浏览器打开
# http://127.0.0.1:8316/ui/

# 方式二：不打开页面，直接在服务器上手动设置管理 Key（建议 32 位随机字符串）
python3 - <<'PY'
import json, secrets, pathlib
p = pathlib.Path("/opt/workbuddy-gateway/config.json")
cfg = json.loads(p.read_text())
cfg.setdefault("gateway", {})["adminKey"] = secrets.token_hex(16)  # 32 字符
p.write_text(json.dumps(cfg, ensure_ascii=False, indent=2) + "\n")
print("已写入 gateway.adminKey，运行中的新版本约 1 秒热生效")
PY
systemctl restart workbuddy-gateway
```

| 项 | 说明 |
|---|---|
| 入口 | `GET /ui/`（独立管理端口，静态外壳不含任何数据） |
| 数据接口 | `GET /admin/api/status`、`/admin/api/models`、`/admin/api/logs`、`/admin/api/config`、`/admin/api/tasks`，**一律需要 `Authorization: Bearer <adminKey>`** |
| 写入接口 | `POST /admin/api/setup`（仅本机首次生成管理 Key）、`POST /admin/api/settings`（模型 API 鉴权与端口）、`POST /admin/api/account-toggle` / `model-toggle`（账号/模型开关）、`POST /admin/api/credential-edit`（凭据 JSON）、`POST /admin/api/log-settings` / `log-cleanup`（保留设置与过期清理） |
| 页面 | 总览（含国内站 / 国际站数量与已停用账号）、账号池（全局调度开关）、模型列表（启停开关，含停用模型）、日志（保留天数与清理）、配置（Key 脱敏预览；确认警告后可编辑完整配置，systemPrompt 正文直接展示）、定时任务（只读日志视图）、访问设置（鉴权与端口编辑）。网页不再提供凭据列表或编辑器 |
| 数据来源 | 复用 `serve` 的状态快照、完整模型目录、`logs/` 日志与本地配置/凭据文件；开关受控写入配置并应用原有调度过滤 |
| 定时任务 | 周期、启用状态、最近执行、完成时间、执行结果、下次预计执行与日志说明；仅展示、不触发任务或修改调度 |

#### 只读定时任务与访问设置

- 「定时任务」通过只读 `GET /admin/api/tasks` 从现有 `logs/gateway-*.log` 中的 `[定时任务]` JSON 记录聚合 11 项后台任务，包含启动续期补跑。没有新增任务调度模块、任务状态文件或持久化登记表；关闭 debug 仍会记录任务事件。
- 每条任务事件包含任务 ID、轮次 TraceID、进程 PID、开始/完成时间、执行结果和已知下次计划。只聚合当前进程启动后的记录，旧进程、损坏行不充当当前成功记录；没有记录时显示“无执行记录”。日志扫描预算为 16 MiB，达到预算会明确提示。
- 配置检查、凭据扫描、状态快照这三项高频任务，每 60 秒记录最近一次执行，实际变化/状态变化即时记录。任务页每 15 秒读取日志；周期性下次时间可按日志中的定时锚点推算，并标记“推算”，事件触发可能提前。一次性任务完成后不再安排下一次；过期且无法推算的计划等待新日志，不伪造执行结果。
- 网页凭据页和编辑器已移除，不再自动读取或展示完整凭据。原 `/admin/api/credentials` 与 `/admin/api/credential-edit` 后端接口暂保留兼容，仍受独立管理鉴权、同源、文件范围和版本冲突保护，网页不再调用。
- 「访问设置」可编辑 `gateway.apiPort` / `gateway.webPort`，保存调用原来的 `POST /admin/api/settings`。配置端口与实际监听端口分别显示，修改后**需重启网关**，不会直接中断当前页面或流式请求；显式 `-port` 仍优先于配置。
- 「配置」页点击「编辑配置」后先展示风险警告，确认后通过 `GET /admin/api/config-edit?acknowledged=1` 加载真实原文（包含 Key）；仅通过可信网络、HTTPS 或 SSH 转发使用。编辑器不把正文保存到浏览器存储，离开配置页、退出编辑或会话失效时清除原文，未保存修改需确认丢弃。
- 「校验并保存」通过 `POST /admin/api/config-edit` 提交 `content`、读取时的 SHA-256 `revision` 和 `acknowledged: true`。配置最大 1 MiB，必须是符合既有字段及类型规则的单个 JSON 对象；拒绝将 `[REDACTED]` 当作 Key。读取后被其他网页操作或服务器修改过的配置返回 `409`，不覆盖新版本；网页保留输入供核对，重新加载前提示丢弃未保存修改。
- 保存完整配置等效于直接修改服务器文件：可热配置立即生效，监听端口保持不变直到重启；日志保留规则可能触发不可撤销的删除。管理 Key 改变后退出旧会话、要求重新登录；清空管理 Key 需额外确认并提交 `confirmAdminKeyRemoval: true`，之后需在服务器本机重新设置。配置写入或热应用失败会尽力恢复旧文件，日志只记录阶段、结果和长度，不记录正文或 Key。
- 「日志」页通过 `GET/POST /admin/api/log-settings` 读写 `logs.retentionDays`，通过 `POST /admin/api/log-cleanup`（空 JSON 对象）清理过期日志，返回删除数量、回收字节、正在写入跳过数量与失败数量。
- 所有新接口均需要独立管理 Key，并复用同源校验和 TraceID 落盘审计；模型 API Key 不能编辑凭据、修改端口或清理日志。

- 未启用 `-webui` 时，管理端口不监听，`/ui` 与 `/admin/api/*` 也不注册路由。
- **管理 Key 的两种设置方式**（二选一即可）：
  1. **页面一键生成**：`adminKey` 为空时，在服务器本机、或用 SSH 本地端口转发（`ssh -L 8316:127.0.0.1:8316 用户@服务器`）后访问 `http://127.0.0.1:8316/ui/`，点击生成 32 字符随机 Key；Key 写入 `config.json` 并**仅在弹窗中展示一次**。远程（非本机）访问不能触发一键生成，避免他人抢先生成；但远程仍可用已设置好的 Key 登录。
   2. **手动编辑配置文件**：直接在服务器上编辑 `config.json`，把 `gateway.adminKey` 设为你自己的随机字符串（建议 32 位），保存后约 1 秒生效。此方式不需要本机访问，也不需要页面操作；登录时填写该字符串即可。
- `/admin/api/logs` 只允许读取 `logs/` 下命名受限的普通、调试、部署和前端审计日志，拒绝任意路径与目录穿越；返回日志先做秘密值脱敏，且单次读取有约 1 MiB 的扫描预算，超长单行会被省略。
- 前端为原生 HTML/CSS/JS，通过 `go:embed` 嵌入二进制，无额外构建步骤与运行时依赖；管理 Key 只保存在当前标签页的 `sessionStorage`，验证成功后才保存。
- 状态快照缺失或损坏时接口返回 `503`，页面保留上次数据并提示，不会把读取失败显示成零账号。

---

## probe

免费 / 收费属性按「账号（含站点）+ 模型」学习，只有该账号真正请求过该模型才会写入账本。默认调度优先使用有余额账号，**余额耗尽的账号几乎不会被选中，也就学不到属性**。`probe` 用于主动补课。

> 注意：额度耗尽的账号会被上游整体拒绝（`14018 Credits exhausted`），此时连免费模型也会失败。要验证某模型是否免费，请使用**额度未耗尽**的账号。

```bash
# 探测全部账号，每个账号取模型目录前 5 个模型
workbuddy-gateway probe

# 只探测指定账号
workbuddy-gateway probe -auth workbuddy4.json

# 指定模型
workbuddy-gateway probe -auth workbuddy4.json -models hy3,deepseek-v4.1-flash

# 指定数量上限（默认 5，上限 50）
workbuddy-gateway probe -auth workbuddy4.json -limit 8
```

| 选项 | 默认 | 说明 |
|---|---|---|
| `-auth <path>` | 全部账号 | 只探测指定凭据（文件名或路径均可） |
| `-models <m1,m2>` | 目录前几个 | 指定要探测的模型 |
| `-limit <n>` | `5` | 未指定 `-models` 时探测的模型数量，上限 50 |

输出示例：

```text
正在请求 http://127.0.0.1:8317/admin/probe（账号=workbuddy4.json，模型=hy3）...

账号                   站点   模型     结果     credit  tokens  说明
workbuddy4.json        intl   hy3      paid     0.42    820     usage.credit=0.42，收费

汇总: paid=1
```

结果状态：

| 状态 | 含义 |
|---|---|
| `free` | `usage.credit=0` 且 `total_tokens ≥ 100`，已学习为免费 |
| `paid` | `usage.credit > 0`，已学习为收费 |
| `unknown` | 未返回 `credit`，或 `credit=0` 但样本过小 |
| `quota` | `14018` 额度耗尽，记为该账号该模型收费并阻断该模型 |
| `rate_limited` | `6004` 模型级限流，只冷却该模型 |
| `auth_failed` | 授权失效（probe 不会自动禁用账号） |
| `skipped` | 账号失效或无凭据 |
| `error` | 网络 / 协议错误 |

> 原理：`probe` 作为客户端调用运行中服务的 `/admin/probe`。账本保存在 `serve` 进程内存中，独立进程直接写状态文件会被服务快照覆盖，因此探测必须由运行中的服务执行。该接口仅接受回环来源；模型 API 启用 Key 校验时同样需要携带 `gateway.apiKey`。

---

## warmup（5 小时窗口主动触发）

部分上游模型按「首次调用后的 5 小时」计算窗口额度。不主动打一次请求，窗口就不会开始计时，白天真正要用时反而只剩很短的可用时间。

`warmup` 每天在指定时刻（默认 `04:00`，配置时区）对筛选出的模型各发一次最小请求，把窗口提前打开。

**默认开启**，默认时刻 `04:00`，默认只触发价格不高于 `0.06` 的模型。

```bash
# 查看当前配置与最近触发结果
workbuddy-gateway warmup status

# 开关
workbuddy-gateway warmup on
workbuddy-gateway warmup off

# 设置触发时刻（配置时区 HH:MM）
workbuddy-gateway warmup time 04:30

# 价格上限：0 = 只触发已确认免费的模型
workbuddy-gateway warmup price 0.06
workbuddy-gateway warmup price 0

# 指定模型（优先级高于价格筛选）；clear 恢复价格筛选
workbuddy-gateway warmup models hy4-preview-f,glm-5.3-flash
workbuddy-gateway warmup models clear

# 立即执行一轮（不等到点）
workbuddy-gateway warmup run
```

| 子命令 | 默认 | 说明 |
|---|---|---|
| `status` | — | 查看配置、命中模型与最近触发结果 |
| `on` / `off` | `on` | 开关 5 小时窗口主动触发 |
| `time <HH:MM>` | `04:00` | 每日触发时刻（配置时区，即 `routing.tz`） |
| `price <n>` | `0.06` | 价格上限；`0` = 只触发已确认免费的模型 |
| `models <a,b>` | 空 | 指定模型列表，优先级高于价格筛选；`clear` 清除 |
| `catch-up <hours>` | `4` | 补触发窗口（自计划时刻起算） |
| `retry <minutes>` | `10` | 补触发重试间隔 |
| `notify on\|off` | `off` | 每轮结束后是否推送结果（走 `notify` 通道） |
| `probe-time <HH:MM>` | `06:00` | warmup 关闭时的每日主动探测时刻 |
| `probe-follow on\|off` | `on` | 主动探测是否跟随 warmup 时刻 |
| `run` | — | 立即执行一轮 |

子命令会改写工作目录的 `config.json` 并通过 `/admin/reload` 让运行中的 `serve` 立即生效，无需重启。

**价格筛选口径（绝不烧钱）**：只对「已确认免费」或「价格明确已知且不超过上限」的模型下手；价格未知、或「付费但无法定标」的模型一律跳过。显式 `models` 指定的模型按用户点名执行，不套价格上限。

**结果记录与补触发**

每轮结果落盘到 `wb-warmup.json`（当前周期逐模型状态 + 最近 14 天汇总）。以下三类「到点没成功」会在补触发窗口内按间隔自动重试：

| 情形 | 处理 |
|---|---|
| 触发时刻服务还没启动 | 进程起来后发现当天周期未完成，立即补一轮（记录标记「补触发」） |
| 模型正在冷却（`6004`） | 保持 `pending`，按 `retry` 间隔重试 |
| 网络不畅 / 上游报错 | 同上 |

只有**授权失效**判定为结构性失败不再重试。超过 `catchUpHours` 仍未成功的，标记失败并归档。

**与主动探测结合**

| warmup | 主动探测执行时刻 |
|---|---|
| 开启（默认） | 与 warmup 同刻执行 |
| 关闭 | `probe.schedule.time`，默认 `06:00` |

用 `warmup probe-follow off` 可让探测脱离 warmup 走独立时刻。

`config.json` 对应配置：

```json
{
  "warmup": {
    "enabled": true,
    "time": "04:00",
    "maxPrice": 0.06,
    "models": [],
    "catchUpHours": 4,
    "retryMinutes": 10,
    "notify": false
  },
  "probe": {
    "schedule": {
      "enabled": true,
      "followWarmup": true,
      "time": "06:00"
    }
  }
}
```

---

## reset

清空**除登录凭据以外**的全部本地数据，并重新拉取模型与倍率。

```bash
workbuddy-gateway reset
```

清理范围：

- `workbuddy-status.json`（账号与模型状态快照）
- `wb-models-cache.json`（模型目录、倍率、价格探测结论）
- `*.disabled` / `*.json.disabled`（授权失效标记）
- `logs/`（运行日志）

保留：`workbuddy*.json` 登录凭据。

清理后会立即重新拉取模型目录与倍率。账号账本同时存在于 `serve` 进程内存中，若服务正在运行，请重启使其同步归零：

```bash
systemctl restart workbuddy-gateway
```

---

## restricted（上游风控限制处置）

上游 403 `{"code":11140,"msg":"request illegal"}`（「账号存在异常行为，已被限制使用」）是**账号级风控限制**：与 429/6004 频率限制、14018 额度耗尽、12153 凭据失效都不同——凭据仍然有效（额度/套餐等只读接口正常），仅模型请求被策略拒绝，官方解除途径是提交反馈申诉（SUBMIT_FEEDBACK）。

网关处置（照搬 workbuddy2api-hub / magpie 的社区方案，2026-10-26 国际站 5 账号事故后移植）：

1. **独立分类**：`restricted` 状态独立于冷却/失效，告警（notify 事件 `restricted`）与控制台明确写「需申诉」，不再误报为「频率限制」。
2. **命中即停**：默认（`upstream.restricted403RotateMax: 0`）第一个账号命中后立即停止本请求返回 429 + 申诉指引，绝不换号重试——换号重试会成批送出连坐证据（社区实测一个任务打死整池账号）。
3. **持久化**（窗口可配，缺省 2 小时；生产 2026-10-31 起设 72 小时完全暂停）：`wb-restricted-state.json` 记录受限账号与站点熔断；重启、凭据热加载、Token 续期都不会把受限账号放回池子；只有**真实成功一次**（chat/probe 返回 200）才自动解除。窗口由 `upstream.restrictedWindowHours` 配置（1～720 小时；上游解封以「天」计，官方口径 1-3 个工作日，短窗口到期后的首发复验只会反复重新记账）。
4. **整池熔断**：同站点 15 分钟内 ≥2 个账号被限 → 该站点停发 30 分钟，期间一个上游请求都不发，选号层（含免费/预算轮）直接跳过。
5. **后台任务跳过**：warmup / 价格探测 / 续期保活 / 每日签到跳过受限账号与熔断站点；warmup 命中后保留 pending（解封后自动恢复），不做自动探测。

```bash
workbuddy-gateway restricted                     # 查看受限账号与站点熔断状态
workbuddy-gateway restricted clear all          # 申诉通过后手动解除（或指定凭据文件名）
curl -H "Authorization: Bearer <apiKey>" http://127.0.0.1:8318/admin/restricted                    # 管理接口查看
curl -X POST -H "Authorization: Bearer <apiKey>" -d '{"clear":"all"}' http://127.0.0.1:8318/admin/restricted
```

> 误用 `clear` 只会提前把受限账号放回池子；解除的正道是官方申诉通过后让网关真实成功一次。

---

## version / help

```bash
workbuddy-gateway version    # 输出 WorkBuddy Local Gateway vX.Y.Z
workbuddy-gateway help       # 输出完整帮助
workbuddy-gateway -v         # 同 version
workbuddy-gateway -h         # 同 help
```

---

## 多账号池

三种配置方式：

```bash
# 方式一（推荐）：自动发现
# 把多个凭据文件放进工作目录，无需任何参数
workbuddy-gateway serve

# 方式二：-auth 逗号分隔
workbuddy-gateway serve -auth workbuddy.json,workbuddy2.json

# 方式三：-auth-dir 目录
workbuddy-gateway serve -auth-dir ./auths
```

行为说明：

- **轮询**：请求按 round-robin 在可用账号间分发。
- **网络错误回退**：上游网络错误先原地重试再自动换号，全部账号失败才返回 502（见「上游错误重试与账号回退」）。
- **429 冷却**：`6004` 只冷却触发模型；无法归因到模型的 429 才进入账号级冷却，冷却到期自动恢复。
- **授权失效**：401/403 类错误禁用账号并删除凭据文件，同时写 `*.disabled` 标记；重新 `login` 后自动恢复。
- **额度耗尽**：`剩余=0` 标记「付费耗尽」，仍可服务已确认免费的模型。
- **热加载**：默认每 5 秒扫描，新增 / 更新 / 删除凭据免重启。
- **串行化**：同一账号请求严格排队，避免并发双发触发风控；不同账号可并行。

---

## 模型列表与倍率

**列表来源**：实时接口 `GET {Base}/v2/enterprises/personal/models` 与 npm 包静态目录，按模型 ID 去重、**接口优先**。

```text
两路都成功  → 合并去重
一路成功    → 使用成功那路
两路都失败  → 使用本地缓存 wb-models-cache.json
失败且无缓存→ 该站点本轮不展示模型（不影响模型调用）
```

**免费站点优先**：若某模型出现「一个站点免费、另一个站点收费」，调度优先使用免费站点的账号，直到该站点账号全部不可用（冷却 / 耗尽 / 失效）才回退到另一站点；若两个站点都免费或都收费（只是倍率不同），则不设优先，保持正常轮询。

**倍率**：

```text
1. 促销生效中：生效倍率 = credits × factor（factor=0 → 0.00x）
2. 促销已过期：接口 credits 不可信（上游常把促销价固化进 credits），
   探测出结果前显示 -，随后由实测决定
3. 模型不在接口目录中：同样交由实测决定
4. 无促销且 credits 有值：直接展示该倍率
```

**价格探测（默认关闭）**：仅显式传入 `-allow-price-probes` 才开启后台探测，优先选择有余额的同站点账号，余额未知时也可能使用。它会发起真实生成请求（`max_tokens=300`），可能消耗积分；频率 / 批量限制不等于积分预算，目前没有累计积分消耗上限。

默认关闭只影响后台主动探测：目录刷新照常获取官方价格及促销时间；正常客户端请求照常根据 `usage.credit` 学习收费 / 免费（零积分且总 token 数至少 100 才认定免费）。显式 `probe` 命令仍可主动实测，同样可能消耗积分。

**本 fork 的选号约束（防烧穿）**：开启后台探测后，选号仍按 fork 的保底余额规则过滤：

| 条件 | 是否用于探测 |
|---|---|
| 已知额度耗尽（`剩余=0`） | ❌ 跳过 |
| 付费模型 + 余额 < `minBalanceGuard` | ❌ 跳过，并记录日志（写明剩余额度与阈值） |
| 付费模型 + 余额充足 | ✅ 选中 |
| **免费模型** | ✅ 不受保底余额限制（零消耗） |
| 价格未知 / 额度未知 | ✅ 不受限 |

> 探测会真实扣费（这是学习「收费」属性的唯一方式），因此必须和调度主链路一样过 `minBalanceGuard`。但**免费模型的路径始终放行** —— 零余额账号仍要靠它学习免费属性，否则会形成「余额低 → 学不到免费 → 永远只能走付费」的死锁。
>
> 注意：`warmup`（5 小时窗口主动触发）走的是另一条路径，默认开启、不受 `-allow-price-probes` 门闸限制；要一并停掉需把 `warmup.enabled` 设为 `false`。

```text
探测免费 → 展示 0.00x；仍符合候选条件时，至少间隔 12 小时复测
探测收费 → 展示 收费(倍率未知)；仍符合候选条件时也可能再次探测
14018 / 无 usage.credit / 样本过小 → 不覆盖，保持未知
```

探测调度（仅显式开启后生效）：

| 时机 | 说明 |
|---|---|
| 服务启动 | 启动后约 20 秒执行首轮 |
| 首次 / 重置后 | 单轮最多 30 个，快速补齐结论 |
| 收敛后 | 单轮最多 5 个，每模型 12 小时最多一次 |
| 待探测未清空 | 用 2 分钟短间隔追赶，清空后回到 30 分钟 |
| 目录刷新成功 | 立即触发一轮 |
| 凭据变化 | 立即触发一轮（含「原本没有某站点账号、后来加入」的情况） |

仅探测被实际请求过、或接口明确需要确认的模型，避免无谓消耗额度。

探测用最小请求（`max_tokens: 300`）——该值不是随手定的：判定「免费」要求 `total_tokens ≥ 100`
（`modelFreeMinTokens`），请求太小会因样本不足而学不到价格属性。5 小时窗口主动触发（warmup）
复用同一请求体，既开窗口又能学到属性。

`/v1/models` 响应头 `X-Model-Source` 与 `/health` 的 `model_source` 会标注目录来源。

---

## 事件告警（notify）

网关内的**每日签到结果（含旅行/成长/补签摘要）、账号冷却 / 模型冷却 / 无可用账号（503）** 事件可即时推送到 Telegram 或任意 webhook（默认关闭，不影响现有行为）。

```json
{
  "notify": {
    "enabled": true,
    "type": "telegram",
    "minIntervalSeconds": 60,
    "events": ["checkin", "cooldown", "model_cooldown", "no_account"],
    "botTokenEnv": "TELEGRAM_BOT_TOKEN",
    "chatIdEnv": "TELEGRAM_CHAT_ID"
  }
}
```

| 字段 | 说明 |
|---|---|
| `type` | `telegram`（调用 Bot API）/ `webhook`（POST JSON 到 `webhook` 字段的 URL） |
| `botTokenEnv` / `chatIdEnv` | **推荐**：从环境变量读取凭据，避免 token 落盘；取不到时回落 `botToken` / `chatId` 明文字段 |
| `minIntervalSeconds` | 同一事件 Key 的最小发送间隔（默认 60s），防止冷却风暴刷屏 |
| `events` | 允许的事件类型；省略表示全部 |

**设计约束**：全程异步（绝不阻塞调度）；发送失败只记日志；队列满丢弃。

### 通知版式

Telegram 通道使用与全库通知统一的 HTML 版式（真源：`jarvanh/actions` 的
`.github/scripts/telegram/tg_notify.sh` 与 `docs/telegram-notify.md`）：
标题 + 分隔线、`标签：值` 取值行、分节 `{emoji} 分节 · N`、树形条目
`<code>  ├─/└─ </code>`，机器值走等宽 `<code>`、动态内容一律转义。
超过 4000 字符按换行分片发送（不切断 UTF-8 多字节字符）。

webhook 通道仍发送纯文本 `title`/`body`（webhook 是通用 JSON 消费者，不假设 HTML）。

**冷却告警会列出当前全部仍在冷却中的账号与模型**（按恢复时间升序、全量不折叠），
让读者在一条告警里看清「现在还有谁不可用、什么时候恢复」。账号级给完整日期时间，
模型级只给 `HH:MM`（模型可能很多，完整日期会撑爆条目）。本次触发的账号/模型已写在
告警头部，不在清单里重复。

---

## 站点路由

v1.14+ 提供「价格驱动的站点路由」：在调度前判定**该模型允许走哪些站点、是否允许在免费时段外花钱**，付费模型不再依赖黑白名单一刀切。

### 核心机制

- **站点五态**：`ALLOWED`（无限制）/ `IN_WINDOW`（免费时段内）/ `BUDGETED`（时段外但当日预算未耗尽）/ `FORBIDDEN`（超价 / 区间外拒绝 / 预算耗尽）。全部站点 `FORBIDDEN` 时入口直接 403（零额度消耗）。
- **价格上限 `maxPrice`**（默认 0.06）：有效倍率超过阈值即拒绝——0.29x 等高价模型自动封死，无需逐个加黑名单。
- **保底余额 `minBalanceGuard`**（默认 10）：账号余额低于阈值时只调度免费模型，防止账号被烧到 0。**主动探测与 5 小时窗口触发的选号同样受此约束**（会真实扣费）；免费模型不受限。
- **破例预算 `fallbackBudget.credits`**（默认 1）：时段外默认拒绝；配置后允许破例，按**实际 credit** 逐日累计，超限熔断（免费请求 credit=0 不计数）。
- **冲突优先级**：`minBalanceGuard` > `fallbackBudget`——保底拦截时请求未发出，零扣费，不消耗预算。
- **账号顺序 `accountOrder: expiringFirst`**：授权（凭据）快到期的账号优先消耗。
- **cheapest-first**：多站可选时按有效倍率升序。
- **幽灵模型护栏**：未匹配任何规则的模型，同样要过价格闸（见下）。目录外的高价新模型不会被"没配规则"漏掉。
- **`maxPrice` 按规则覆盖**：`rules[].maxPrice` 未配置时沿用全局，配置后按规则生效。

### 幽灵模型护栏

`defaultPolicy: allow` 时，未匹配任何规则的模型原本直接放行、完全不过价格闸。这意味着**任何不在配置里的模型都可能裸奔**——ja 事故正是这么烧的（当年的 `glm-5.3-flash` 是目录外幽灵模型，无规则、无价格信息，一路真实计费到烧光）。

护栏 `ghostPaidBlocked` 让未匹配规则的模型也过一遍价格闸：

| 价格结论 | 护栏行为 |
|---|---|
| 确认免费 | 放行 |
| 完全未知（无任何价格数据） | 放行 |
| 已知 ≤ `maxPrice` | 放行 |
| 已知 > `maxPrice` | **拒绝** |

> **为什么"完全未知"反而放行？** 拒绝会阻断请求 → 再也拿不到真实 credit 证据 → 模型被永久锁死在"付费"，连后来变免费都观测不到（死锁式误杀）。宁可放过，不可锁死。

### 价格来源优先级

`catalog`（官方倍率，促销有效时才可信）> `ledger`（真实请求大样本 credit/tokens）> `probe`（探测小样本，仅作 free/paid 存在性判断）。

> 探测用最小请求（max_tokens:300），其单价会把真实消耗高估约 14 倍，因此**不用于比价与预算**；比价以真实消耗大样本为准。目录外模型经 `priceAnchor`（默认 glm-5.3-flash@cn=0.06x）换算到同一倍率标尺。

### 配置示例

```json
{
  "routing": {
    "tz": "Asia/Shanghai",
    "defaultPolicy": "allow",
    "accountOrder": "expiringFirst",
    "minBalanceGuard": 10,
    "maxPrice": 0.06,
    "priceAnchor": { "model": "glm-5.3-flash", "site": "cn", "multiplier": 0.06 },
    "rules": [
      { "models": ["hy4-preview-f"],
        "sites": [ { "site": "intl" }, { "site": "cn" } ] },
      { "models": ["hy4-preview"],
        "sites": [
          { "site": "intl" },
          { "site": "cn",
            "window": { "start": "23:00", "end": "08:00" },
            "outside": "reject",
            "fallbackBudget": { "credits": 1 },
            "active": { "from": "2026-09-11", "until": "2026-10-11" },
            "onExpire": "reject" } ] },
      { "models": ["glm-5.3-flash"],
        "sites": [ { "site": "intl" }, { "site": "cn" } ] },
      { "models": ["some-expensive-model"],
        "maxPrice": 0.5,
        "sites": [ { "site": "intl" }, { "site": "cn" } ] }
    ]
  }
}
```

### 字段说明

| 字段 | 说明 |
|---|---|
| `tz` | 时区，默认 `Asia/Shanghai`（无 tzdata 环境回落 UTC+8） |
| `defaultPolicy` | 未匹配规则的模型：`allow`（默认，兼容现状）/ `reject` |
| `accountOrder` | 账号排序：`expiringFirst`（默认，授权快到期优先）/ 留空保持轮询原序 |
| `minBalanceGuard` | 保底余额（credit），低于它不调度付费模型；0 关闭 |
| `maxPrice` | 价格上限（有效倍率），超过即拒绝 |
| `priceAnchor` | 倍率校准锚点：已知倍率的模型 + 其站点 |
| `rules[].models` | 匹配的模型名（大小写不敏感） |
| `rules[].maxPrice` | 该规则的价格上限；省略则用全局 `maxPrice`，显式 `0` 表示禁止该规则的付费模型 |
| `rules[].sites[].window` | 每日免费时段（HH:MM，支持跨午夜如 23:00-08:00） |
| `rules[].sites[].outside` | 时段外策略：`reject`（默认）/ `prefer`（放行但降权） |
| `rules[].sites[].fallbackBudget` | 时段外破例的每日 credit 上限 |
| `rules[].sites[].active` | 生效日期区间，`until` 为排他日期（该日 00:00 起失效） |
| `rules[].sites[].onExpire` | 区间外行为：`reject`（默认）/ `warn` / `allow` |

### 运行行为

- 全站 `FORBIDDEN` → 入口 403 `model_routing_blocked`，零额度消耗；
- `FORBIDDEN` 站点在账号选择的所有轮次（含失败回退换号）中均被排除；
- 被全禁的模型不再出现在 `/v1/models`；
- 启动横幅展示：规则数 / maxPrice / 保底余额 / 账号序；
- **预算持久化**：消耗记在工作目录 `wb-routing-spend.json`，原子写入，重启后自动恢复（不再清零）；
  仅真实扣费才落盘，免费请求（`credit=0`）不产生磁盘 IO；超过 3 天的记录自动淘汰。

---

## 客户端接入

网关启动后服务地址为 `http://127.0.0.1:8317/v1`。

curl：

```bash
curl -N -s http://127.0.0.1:8317/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"hy4-preview","messages":[{"role":"user","content":"你好"}],"stream":true}'
```

Python OpenAI SDK：

```python
from openai import OpenAI

client = OpenAI(base_url="http://127.0.0.1:8317/v1", api_key="none")
resp = client.chat.completions.create(
    model="hy4-preview",
    messages=[{"role": "user", "content": "写一个快速排序"}],
)
print(resp.choices[0].message.content)
```

Claude Code（Anthropic 协议直连，免外部翻译层）：

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:8317
export ANTHROPIC_API_KEY=你的模型APIKey   # config.json 未开启 apiKeyEnabled 时随意填
claude
```

> 网关在 `/v1/messages` 入口完成 Anthropic → Chat 双向转译：思维链回译为 `thinking` 内容块、
> `tool_calls` 回译为 `tool_use`、`finish_reason` 映射 `stop_reason`（`tool_calls→tool_use`、
> `length→max_tokens`），并复用账号池轮询、工具序列自愈与 WAF 脱敏管线。
> 模型名直接填网关 `/v1/models` 列表中的 ID。

DSH（`~/.dsh/settings.yaml`）：

```yaml
llm-pi-ai:
  providers:
    workbuddy-local:
      baseURL: http://127.0.0.1:8317/v1
      apiKeyEnv: LOCAL_API_KEY   # 任意字符串即可
      api: openai-completions
      models:
        - id: hy4-preview
          contextWindow: 1000000
          maxTokens: 128000
```

---

## 各平台部署

### Windows

1. 从 [Releases](https://github.com/jarvanh/workbuddy-gateway/releases) 下载 `workbuddy-gateway-windows-amd64.exe`。
2. 在 PowerShell / CMD 中进入文件所在目录：

   ```powershell
   .\workbuddy-gateway-windows-amd64.exe login
   .\workbuddy-gateway-windows-amd64.exe serve -port 8317
   ```

3. 开机自启：`Win+R` → `shell:startup`，把 exe 快捷方式放入启动文件夹，并在快捷方式“目标”后追加 `serve`。

### Linux

```bash
# x86_64
wget https://github.com/jarvanh/workbuddy-gateway/releases/latest/download/workbuddy-gateway-linux-amd64
sudo install -m 755 workbuddy-gateway-linux-amd64 /usr/local/bin/workbuddy-gateway

# ARM64
wget https://github.com/jarvanh/workbuddy-gateway/releases/latest/download/workbuddy-gateway-linux-arm64
sudo install -m 755 workbuddy-gateway-linux-arm64 /usr/local/bin/workbuddy-gateway

workbuddy-gateway login
workbuddy-gateway serve -addr 127.0.0.1 -port 8317
```

#### systemd 服务（推荐）

创建 `/etc/systemd/system/workbuddy-gateway.service`：

```ini
[Unit]
Description=WorkBuddy Local Gateway (CodeBuddy OpenAI-compatible proxy)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=/opt/workbuddy-gateway
ExecStart=/opt/workbuddy-gateway/workbuddy-gateway serve -addr 0.0.0.0 -port 8317
Restart=on-failure
RestartSec=5
User=root
NoNewPrivileges=true
ProtectSystem=full
ProtectHome=false

[Install]
WantedBy=multi-user.target
```

部署与启动：

```bash
sudo mkdir -p /opt/workbuddy-gateway
sudo cp workbuddy-gateway /opt/workbuddy-gateway/
sudo /opt/workbuddy-gateway/workbuddy-gateway login
sudo systemctl daemon-reload
sudo systemctl enable --now workbuddy-gateway
sudo systemctl status workbuddy-gateway
sudo journalctl -u workbuddy-gateway -f
```

> `WorkingDirectory` 决定自动发现的凭据目录。把多个凭据文件放进该目录即可组成账号池，新增 / 更新 / 删除会自动热加载。

常用运维：

```bash
sudo systemctl restart workbuddy-gateway
sudo systemctl stop workbuddy-gateway
sudo systemctl disable workbuddy-gateway
```

对外开放时（例如局域网其他设备）把 `-addr` 改为 `0.0.0.0`，并在 `config.json` 中开启模型 API Key 校验：

```json
{"gateway":{"apiKeyEnabled":true,"apiKey":"sk-changeme"}}
```

```ini
ExecStart=/opt/workbuddy-gateway/workbuddy-gateway serve -addr 0.0.0.0
```

### macOS

1. 下载 `workbuddy-gateway-darwin-arm64`（Apple Silicon）或 `workbuddy-gateway-darwin-amd64`（Intel）。
2. 移除隔离属性：

   ```bash
   chmod +x workbuddy-gateway-darwin-arm64
   xattr -d com.apple.quarantine workbuddy-gateway-darwin-arm64 2>/dev/null || true
   ```

3. 登录与启动：

   ```bash
   ./workbuddy-gateway-darwin-arm64 login
   ./workbuddy-gateway-darwin-arm64 serve
   ```

4. 开机自启（launchd）：创建 `~/Library/LaunchAgents/com.workbuddy.gateway.plist`：

   ```xml
   <?xml version="1.0" encoding="UTF-8"?>
   <!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
   <plist version="1.0">
   <dict>
     <key>Label</key><string>com.workbuddy.gateway</string>
     <key>ProgramArguments</key>
     <array>
       <string>/path/to/workbuddy-gateway-darwin-arm64</string>
       <string>serve</string>
       <string>-port</string><string>8317</string>
     </array>
     <key>RunAtLoad</key><true/>
     <key>KeepAlive</key><true/>
     <key>WorkingDirectory</key><string>/path/to/workbuddy-gateway-dir</string>
   </dict>
   </plist>
   ```

   ```bash
   launchctl load ~/Library/LaunchAgents/com.workbuddy.gateway.plist
   ```

---

## 安全提示

- `workbuddy*.json` 包含真实访问凭据（Access Token / Refresh Token），**严禁提交到 Git 或公开分享**；本仓库 `.gitignore` 已排除。
- 网关默认只监听 `127.0.0.1`。需要局域网 / 公网访问时改用 `-addr 0.0.0.0`，并在 `config.json` 中开启模型 API Key 校验（`gateway.apiKeyEnabled`），或置于反向代理之后。
- `config.json` 中的 `gateway.adminKey` 与 `gateway.apiKey` 按要求明文保存，请限制该文件权限；网页配置预览隐藏 Key，管理员明确确认编辑警告后才在编辑器显示真实 Key。不要分享编辑器截图或在公共设备上编辑，完整配置只保留在当前页面内存中，不进入审计日志。
- `/admin/probe` 仅接受回环来源调用。
- 不再需要某账号授权时，删除对应凭据文件并在 CodeBuddy 控制台撤销授权。

---

## 从源码构建

需要 `go.mod` 声明的 **Go 1.26.5 或更新工具链**：

```bash
git clone https://github.com/jarvanh/workbuddy-gateway.git
cd workbuddy-gateway

go vet ./...
go test ./...
```

> **自动发布**：推送 `v*` 标签（如 `v1.13.1`）即触发 GitHub Actions（`.github/workflows/release.yml`）自动运行 `go vet` + `go test`、交叉编译全部 5 个平台产物（版本号取自标签名，通过 `-ldflags -X main.version=` 注入）、生成 SHA-256 校验文件并创建 GitHub Release。
>
> ⚠️ 版本号注入依赖 `main.go` 里的 `version` 保持 `var`（不是 `const`）：Go 的 linker 无法对 `const` 做 `-X` 注入，改成 `const` 会让 tag 发版产物永远显示源码里的开发版本号。本 fork 不同步上游的 `const version` 改动，正是为此。

PowerShell 一键构建：

```powershell
.\scripts\test.ps1                 # Go、网页和构建脚本回归
.\build.ps1                       # 校验并构建全部 9 个平台/架构目标
.\build.ps1 -Only linux -Architecture amd64
```

Go 单元测试保持与 `package main` 源码同目录，以保留私有函数测试和标准 `go test ./...` 行为；它们不进入生产构建。独立网页与构建脚本测试集中在 `tests/`。完整文档集中在 `docs/`，根目录保留构建入口与配置示例。

本项目仅用于个人学习与技术研究，请遵守上游服务条款。完整安全提示与免责声明见[使用手册](docs/user-guide.md#安全提示)。

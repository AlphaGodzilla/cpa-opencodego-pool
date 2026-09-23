# SPEC — CLIProxyAPI 插件 `opencodego-pool`

按用量在多个 opencode api-key 之间做会话粘性路由。

---

## 1. 交付物

| 项 | 值 |
|---|---|
| 插件 ID | `opencodego-pool`（= 动态库文件名去扩展名 = `plugins.configs` 的键） |
| 语言 / 构建 | Go，`go build -buildmode=c-shared` |
| 产物 | `opencodego-pool.dylib`（macOS arm64），放 `plugins/<GOOS>/<GOARCH>/` 或 `plugins/` |
| 声明 `schema_version` | 6 |
| 发布 | 只本地构建；仓库结构预留插件商店发布格式，但本次不做发布资产 |

## 2. 背景约束（来自宿主源码侦察，全部有 file:line 依据）

1. `openai-compatibility` 的每个 `api-key-entry` → 一个 auth record：
   - `ID = "openai-compatibility:" + lower(trim(compat.name)) + ":" + sha256(kind ‖ \0 ‖ key ‖ \0 ‖ base-url ‖ \0 ‖ proxy-url)[:12]`
   - `Attributes` 含 `api_key`（明文）、`base_url`、`compat_name`、`provider_key`、`config_index`、`source`、`weight`
2. `scheduler.pick` 的候选经过了敏感字段脱敏（`api_key` / `token` / `secret` / `proxy_url` 等一律剔除）。
   → **调度器拿不到明文 key**，无法直接调用 usage 接口。
3. 明文 key 只有两个插件可见通道：`model.for_auth` 的 `Attributes`（未脱敏）和 `quota.fetch`。
   `host.auth.*` 对 config 合成的 auth **完全不可见**（无 auth 文件、非 runtime-only）。
4. `scheduler.pick` 能拿到客户端 header（`Options.Headers`）。
5. CPA 不认识 `x-opencode-session`；内建 session affinity 只认 `X-Session-Affinity` / `Session-Id` / `Thread-Id` 等。
   → 内建粘性不可用，必须自实现。
6. `model.for_auth` **只对新增/变更的 auth 触发**（未变更的 auth 不产生 update，因此不重跑模型注册）。
   → 它是纯粹的「新建 key」事件流；**删除 key 没有事件**，删除检测必须另找信号。
7. config.yaml 热重载存在（fsnotify + 150ms 防抖 + sha256 去重）。`plugin.reconfigure` 在**每次**重载时对所有已加载插件触发。
11. **`model.for_auth` 对只声明 `model_provider` 的插件不会被调用**：宿主 `Host.ModelsForAuth` 会先用
    `h.modelProvider(pluginID)`（只有注册过模型的插件才有值）或 `AuthProvider.Identifier()` 与该 auth 的
    provider key 比对，不匹配直接 `continue` 跳过。要匹配上必须冒名声明 `executor` / `auth_provider`，
    会污染宿主的模型注册与执行路径 → 本插件放弃该通道。
8. `plugin.reconfigure` 排在 auth diff **之前**、同一 goroutine 串行 → **必须立即返回，禁止同步 HTTP**。
9. `cliproxy_plugin_shutdown`（C 钩子）调用前只等在途的「宿主→插件」调用排空，**不算插件自己的 goroutine**。
10. 候选列表每次 pick 都从活的 `m.auths` 重建，**无缓存延迟**。

## 3. 声明的能力（4 个）

| 能力 | 用途 |
|---|---|
| `scheduler` | `scheduler.pick` 路由决策 |
| `usage_plugin` | `usage.handle`：429 快路径 + 会话↔凭证结果校准 |
| `quota_provider` | 把缓存用量接进 CPA 管理面板的配额列 |
| `management_api` | 只读资源页 + JSON 路由 |

**不声明** `model_provider` / `model_registrar` / `executor` / `auth_provider` / `model_router` / `request_interceptor`。

> `model_provider`（`model.for_auth`）原计划作为 key 发现通道，实测不可用，已移除（见 §2.11）。

## 4. 数据模型

```go
type Window struct {
    Status   string    // "ok" / "rate-limited" / …
    Percent  float64
    ResetsAt time.Time
}

type Usage struct {
    Rolling, Weekly, Monthly Window
}

type Key struct {
    AuthID    string    // 宿主确定性生成的 auth record ID
    Provider  string    // provider_key，如 openai-compatible-opencodego
    Token     string    // attrs["source"] 里的 12-hex 短标识（资源页展示用）
    APIKey    string    // 明文；只驻内存，不进日志、不进任何管理响应
    BaseURL   string
    Usage     Usage
    HasUsage  bool      // false = 未知
    FetchedAt time.Time
    FailStreak int
    Revoked   bool      // usage 接口返 401/403
    Suspected429At time.Time
    LastCandidateSeen time.Time
}

type binding struct {
    AuthID   string
    Provider string
    Expiry   time.Time   // 24h 滑动续期
}
```

- 全部**纯内存**，进程退出即丢。
- 会话绑定：`map[sessionID+"\x00"+providerKey]binding`，上限 10000，超限按 LRU 淘汰。

## 5. 调度算法（`scheduler.pick`）

```
1. 池判定：候选的 attrs["base_url"] 以 base_url_prefix 开头（大小写不敏感，均已 trim）
   无匹配 → Handled=false
2. session := headers["X-Opencode-Session"]（大小写不敏感）
   session == "" → Handled=false（完全不干预，交回内建调度）
3. 按 provider_key 分桶。若候选分属多个 provider_key（mixed）→ Handled=false
4. eligible = 桶内候选 − 以下任一成立的：
   a. Revoked（usage 接口 401/403）
   b. 任一窗口 Status != "ok"
   c. 命中 exclude_keys
   d. 被 429 快路径标记为「疑似打满」
5. eligible 为空 → 兜底集合 = {Usage 已知且存在非 ok 窗口} 的 key，
   取 max(三窗口 percent) 最小者放行；兜底集合也为空 → 取 authID 字典序最小者
6. pin_sessions[session] 命中且该 key 在桶内且未被排除 → 选它
7. 会话绑定命中且该 key 在 eligible 内 → 选它并续期 TTL
   绑定命中但不在 eligible → 立即改绑（落到第 8 步）
8. 排序取首：
   主键 = Usage.Rolling.Percent 升序        （weekly / monthly 不参与排序）
   未知（HasUsage=false）或陈旧（FetchedAt 早于 now-stale_after）→ 视为 +Inf，排最后
   次键 = AuthID 字典序                     （确定性，绝无随机）
9. 写入 / 更新会话绑定并返回 {Handled:true, AuthID}
```

- 同时把所有桶内候选标记 `LastCandidateSeen = now`（供删除检测用）。
- **任何内部异常一律退化为 `Handled:false`**——`scheduler.pick` 返回 error 会让本次请求直接失败，不能冒这个险。

## 6. key 发现与生命周期

**发现通道 = 插件自己解析 config.yaml + 用实时候选持续校验配方**

```
触发重读（两条，任一即可）
  a. plugin.reconfigure（宿主每次 config.yaml 重载都会触发）→ 立即返回，异步触发重读
  b. 独立的 30s mtime/size 巡检（兜住「文件改了但宿主重载失败」的窗口）

重读 config_path
  → 解析 openai-compatibility，只取未 disabled、且 base-url 前缀匹配的 provider
  → 对每个 api-key-entry 复算 token（配方见下）→ (token, provider_key, base_url, api_key)
  → 与旧表 diff：新增 token → 立即异步补拉一次 usage；消失的 token → 移除该 Key 及其全部会话绑定

token 配方（复刻宿主 internal/watcher/synthesizer.StableIDGenerator.Next）
  kind   = "openai-compatibility:" + lower(trim(compat.name))   // name 为空则用 "openai-compatibility"
  digest = sha256(kind ‖ \0 ‖ trim(api-key) ‖ \0 ‖ trim(base-url) ‖ \0 ‖ trim(proxy-url))
  token  = hex(digest)[:12]
  // 同一 kind 内出现完全相同的 (key, base-url, proxy-url) 时，第 N 次出现追加 "-N"（N 从 1 起）

与实时候选对账（每次 scheduler.pick 都做）
  → 候选的 attrs["source"] 形如 "config:<provider>[<token>]"
    该键名不含任何脱敏敏感词（api_key/token/secret/... 都不匹配），所以一定可见
  → 用 token 反查 Key 表，得到「候选 ↔ Key」映射
  → 本次 pick 存在 base-url 匹配的候选、但映射成功数为 0 → 判定配方/配置失配：
     host.log error + 本次 pick 一律 Handled=false（退回内建调度）
  → 映射成功的候选记 LastCandidateSeen = now
```

> **为什么不用 `model.for_auth`**：宿主 `Host.ModelsForAuth` 先用能力匹配筛掉插件——只声明 `model_provider`
> 时 `h.modelProvider(pluginID)` 恒为空，插件直接被 `continue`；要匹配必须额外声明 `executor`/`auth_provider`。
> 而且它只对新增/变更的 auth 触发，删除 key 仍然没有事件。

**删除检测**

```
主通道：config.yaml 重读后的 diff（权威、且删除也有事件）
兜底（配置文件连续读不到时生效）：
    LastCandidateSeen 早于 now - 3*poll_interval
    且 FailStreak >= 3（usage 拉取连续失败）
    → 移除
```

`Revoked` 的 key **不**移除——保留在资源页上暴露，便于轮换。

**轮询**
- `time.Ticker`，间隔 `poll_interval`（默认 600s），worker 并发上限 4。
- 经 `host.http.do` 发：

  ```
  GET <usage_url>
  Authorization: Bearer <api_key>
  Accept: application/json
  User-Agent: opencode-go-quota
  ```

- `host.http.do` **无 timeout 字段** → 自建 `request_timeout`（默认 15s）计时兜住；
  超时后放弃等待并计入 `FailStreak`（底层 goroutine 会存活到 HTTP 层自行放弃，这是已知且接受的泄漏）。
- 解析容错：字段缺失 → 该窗口视为未知；出现新窗口 → 忽略；`percent` 非数字 → 该窗口视为未知。
- 200 → 写入 Usage、`FetchedAt=now`、`FailStreak=0`、清 `Revoked`/`Suspected429At`
- 401 / 403 → `Revoked=true` + `host.log` error
- 其它非 200 / 网络错误 / 超时 → `FailStreak++`

**429 快路径**
```
usage.handle(record)
  → record.Failed && record.Failure.StatusCode == 429
    且 record.AuthID 属于本池
  → 置 Suspected429At = now（立即从 eligible 移除）
  → 触发一次补拉（同一 authID 60s 内只触发一次，节流）
```
**怀疑有最短保留期 `SuspectHold = 60s`（2026-09 修订）。** 最初的规则是「补拉成功即用真实 usage 覆盖」，
实测发现这条规则会让快路径形同虚设：429 触发的补拉若赶上用量接口滞后、仍报该 key 健康，429 证据就被
立刻抹掉，key 马上又成为最优候选。现在改为：

- 保留期内，成功的 usage 读取**不清除**怀疑（`Registry.StoreUsage` 的守卫）
- 保留期在**判定时**求值（`State.Suspected429(now)`），所以即使一直没再拉取，60s 后该 key 也会自动恢复可用
- 60s 与 `DefaultSuspectThrottle` 对齐：保留期一结束，下一次带外补拉正好可以执行，用新鲜数据重新评估

代价：因非配额原因（瞬时抖动、按模型限制）429 的 key 最多被冷落 60s。两个 key 同时进入保留期时仍有兜底放行，
不会中断服务。

**关闭**
- `plugin.quiesce`（RPC）与 C 的 `cliproxyPluginShutdown` 都要：
  停止 ticker、停止补拉 worker、**join 所有本插件启动的 goroutine**。
  宿主只等它自己的在途调用排空，不 join 会导致 shutdown 返回后调 `host.call` 失败、库可能已被 unmap。

## 7. 配置 schema

```yaml
plugins:
  enabled: true
  configs:
    opencodego-pool:
      enabled: true
      priority: 100                                       # 必需高于其它 scheduler 插件

      usage_url: "https://opencode.ai/zen/go/v1/usage"    # 必需；不做任何推导

      config_path: "/path/to/config.yaml"                 # 必需；不填则插件拒绝处理任何 pick
      base_url_prefix: "https://opencode.ai"              # 可选，默认值
      poll_interval: "600s"                               # 可选，默认 600s
      session_ttl: "24h"                                  # 可选，默认 24h
      max_sessions: 10000                                 # 可选，默认 10000
      request_timeout: "15s"                              # 可选，默认 15s
      stale_after: "30m"                                  # 可选，默认 30m
      log_level: "warn"                                   # 可选，默认 warn
      exclude_keys: []                                    # 见下
      pin_sessions: {}                                    # 见下
```

**`exclude_keys` / `pin_sessions` 的引用方式**（三种写法任一匹配即可）：
1. 完整 api-key
2. auth ID（`openai-compatibility:opencodego:<12hex>`）
3. 短标识 token（12-hex，资源页会显示）

`pin_sessions` 的键 = `x-opencode-session` 的原始值。

**`usage_url` 为空时**：不处理任何 pick（全部 `Handled:false` 交回内建调度）+ `host.log` error。宁可退回默认行为，也不静默劣化。

**`config_path` 缺失 / 不可读 / 配方对不上（候选池非空但映射成功数为 0）时**：同样拒绝处理任何 pick + `host.log` error。
带着可能是错的 key 映射去调度，会把会话粘到一个错误的 key 上，比不粘更糟。

## 8. 管理端

- `GET /v0/management/plugins/opencodego-pool/state`（需管理密钥）
  每 key：`token`、`base_url`、三窗口、`status`、**被排除原因**、绑定会话数、`last_fetched_at`、`fail_streak`、`revoked`；
  另含全局统计：池大小、eligible 数、命中类型计数（绑定命中 / 新分配 / 兜底 / pin / 未干预）。
  **绝不返回明文 api-key。**
- `GET /v0/resource/plugins/opencodego-pool/status`（GET 免鉴权）
  同一份数据的只读 HTML 表格，**内嵌在 .dylib 里**（宿主不提供静态文件服务）。
  无任何写操作。（注意：插件**不**提供未鉴权的写接口）

## 9. quota_provider

- `identifier` → `opencodego-pool`
- `describe` → `supported_providers` 列出所有已知的 `provider_key`，`supports_reset = false`
- `fetch` → 纯读缓存，**不打上游**：
  - `groups[].buckets[]`：rolling / weekly / monthly 三个 bucket
    `remaining_fraction = (100 - percent)/100`，`reset_time = ResetsAt`
  - `summary`：`rolling_percent` / `weekly_percent` / `monthly_percent`
- `reset` → `{success:false, message:"not supported"}`

## 10. 明确不做

- 不写 config.yaml、不改 per-key `weight`（回避宿主的非原子写入，且每次增删会掐断该 provider 全部在途流）
- 不落盘任何状态
- 不干预无 `x-opencode-session` 的请求（**已知打满的 key 仍可能被内建调度选中 —— 这是明确确认接受的代价**）
  - 2026-09 补充：本部署在 provider 上设了 `disable-cooling: true`，宿主不会在 429 后把凭证冷落下线。
    两者叠加使这类请求完全没有用量保护，已向用户说明并确认**保持不干预**。
    如果需要兜底，把 `disable-cooling` 改回默认即可让宿主自己恢复冷却。
- 不防「旧轮询器残留」（纯内存、无状态文件，文件锁无意义；多跑一个只是多发一份 HTTP）
- 不做插件商店发布资产、不做端到端回放脚本

## 11. 已知风险（接受）

| 风险 | 来源 | 影响 |
|---|---|---|
| 删一个 key 会掐断该 provider 全部在途流 | 宿主 `Manager.Remove` → `CloseExecutionSession(CloseAllExecutionSessionsID)` | 手工删 key 时正在跑的会话会断 |
| config.yaml 非原子写入 | 宿主 `SaveConfigPreserveComments` 用 `os.Create` 截断重写 | 高并发时批量改 key 可能触发一次加载失败；已确认手工编辑，风险低 |
| `model.for_auth` 观察依赖宿主内部行为 | CPA 升级可能改变触发条件 | 交叉校验会在不一致时告警；若两端同时失效则静默 |
| 另一个 scheduler 插件优先级更高 | 宿主只咨询最高优先级的 scheduler 插件 | 本插件完全不生效；靠 `priority: 100` 规避 |
| `host.http.do` 无 timeout | 宿主实现 | 自建计时兜住，超时后底层 goroutine 存活到 HTTP 层放弃 |
| 删除检测在 config.yaml 连续读不到时退化到启发式 | 宿主无删除事件 | 见 §6 兜底规则；`config_path` 已是必需项，正常情况走不到这里 |

## 12. 验收

1. **单元测试**（`go test ./...`）
   排序与 tie-break / 排除规则 / 全排除兜底 / 绑定续期·改绑·TTL·LRU / pin / exclude /
   未知与陈旧排序 / usage 解析容错 / 401·403 / 429 节流 / authID 配方 / 配置默认值与校验
2. **进程内集成测试**（`internal/service`）
   可编程的假 usage HTTP server + 假 host 桥，端到端驱动
   `plugin.register` → config.yaml 同步 → `scheduler.pick` → `usage.handle` → `quota.fetch`，
   断言选中的 authID 序列。覆盖：新会话取最少、同会话稳定、打满改绑、
   新增 key 立即参与、删除 key 释放绑定、配置不可读时拒绝路由、
   配方失配后能自愈、429 快路径、管理端不泄漏 api-key。

3. **ABI 端到端检查**（`make abicheck`）
   用 `dlopen` + 真实 C 函数表加载构建出的 `.dylib`，走完
   `cliproxy_plugin_init` → `plugin.register` → `host.http.do` → `quota.fetch` →
   `scheduler.pick` → `cliproxy_plugin_shutdown`。
   这是唯一能发现符号名、函数表布局或 JSON 信封不匹配的检查——这类问题
   否则会在宿主加载时静默失败。（此检查已捕获过一个真实缺陷：插件把整个
   lifecycle 请求当配置解析，从未解开 `config_yaml` 外层。）
4. **手动清单**（详见 README §5）
   `GET /v0/management/plugins` 确认 `registered: true` 且 `effective_enabled: true` →
   开资源页 → 发请求验证命中。

## 13. 已确认的部署取值

来自用户实际的 `openai-compatibility` 配置：

```yaml
openai-compatibility:
  - name: opencode_go
    base-url: https://opencode.ai/zen/go/v1/chat/completions
    api-key-entries:
      - api-key: oc_sk_xxxx01
        weight: 1
      - api-key: oc_sk_xxxx02
        weight: 1
    disable-cooling: true
```

由此确定：

- `provider_key` = `openai-compatible-opencode_go`（下划线保留）
- auth ID = `openai-compatibility:opencode_go:<12位token>`
- `base_url_prefix` 保持默认 `https://opencode.ai`（前缀匹配到 origin，能吃掉 `/chat/completions` 路径）
- `usage_url` = `https://opencode.ai/zen/go/v1/usage`（**必须手填**，无法由带 `/chat/completions` 后缀的 base-url 推导）
- `config_path` = 部署时 CLIProxyAPI 实际使用的 config.yaml 路径

`disable-cooling: true` 关掉了宿主的凭证冷却，本插件因此成为唯一的用量保护层；
`429 fast path` 是否随上游 429 增长，需要在真机联调时确认。

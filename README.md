# opencodego-pool

CLIProxyAPI 插件：在同一个 `openai-compatibility` provider 的多个 opencode api-key 之间，按**上游真实用量**做会话粘性路由。

- **新会话** → 路由到 `rolling` 窗口用量最低的 key
- **同一个 `x-opencode-session`** → 始终复用同一个 key，绝不随机漂移
- **key 打满/失效** → 立即改绑到当前用量最低的 key
- key 增删**无需重启**，靠 config.yaml 热重载自动识别

设计依据、宿主约束与全部决策记录见 [SPEC.md](SPEC.md)。

---

## 1. 它为什么必须是一个插件

CLIProxyAPI 的 `scheduler.pick` 候选中，`api_key` 属于敏感字段会被脱敏剔除，所以调度插件拿不到明文 key，也就无法调用上游的 usage 接口。本插件因此自己解析 `config.yaml` 拿到 key，并用一份**与宿主相同的哈希配方**算出每个 key 的短标识（token），再拿这个 token 去和实时候选里的 `source` 属性对账。

每次 pick 都会做这个对账：**配方一旦失效，匹配数归零，插件立刻拒绝处理并退回宿主的默认调度，而不是继续用错的映射做路由。**

## 2. 安装

```bash
make build                      # 产出 bin/opencodego-pool.dylib
make install INSTALL_DIR=/path/to/cpa/plugins/darwin/arm64
# 或直接拷进 CLIProxyAPI 的插件目录
```

宿主只在**两个**位置查找（`internal/pluginhost/platform.go` 的 `candidateDirs`）：

```
<plugins_dir>/<GOOS>/<GOARCH>/     例：plugins/darwin/arm64/
<plugins_dir>/
```

**文件名（去掉扩展名）就是插件 ID**，必须与 `plugins.configs` 下的键完全一致 —— 所以文件必须叫
`opencodego-pool.dylib`。

⚠️ **`plugins.dir` 的相对路径是相对 CPA 进程的 cwd 解析的**，不是相对 `config.yaml`。
`ResolvePluginsDir` 只展开 `~`，其余走 `filepath.Clean`。如果你用相对路径启动 CPA 而 cwd 不是预期目录，
宿主会在别处找插件。排查时直接看 `GET /v0/management/plugins` 返回的 `plugins_dir` 字段——那是解析后的真实路径。

在 CLIProxyAPI 的 `config.yaml` 里开启：

```yaml
plugins:
  enabled: true                 # 必须为 true，否则插件不会生效
  configs:
    opencodego-pool:
      enabled: true               # 必写！见下方说明
      priority: 100             # 必须高于其它 scheduler 插件，否则本插件不会被咨询

      usage_url: "https://opencode.ai/zen/go/v1/usage"   # 必需，且不能由 base-url 推导
      config_path: "/path/to/cpa/config.yaml"            # 必需

      # 以下全部可选，列的是默认值
      base_url_prefix: "https://opencode.ai"
      poll_interval: "600s"
      session_ttl: "24h"
      max_sessions: 10000
      request_timeout: "15s"
      stale_after: "30m"
      log_level: "warn"
      author: "local"                                        # 这两项不能为空，见 §8
      repository: "https://example.com/opencodego-pool"       # 填你自己的仓库更好
      exclude_keys: []
      pin_sessions: {}
```

### ⚠️ `enabled: true` 必须显式写出来

宿主把 `plugins.configs.<id>.enabled` 的缺省值规范化为 **`false`**：

```go
// internal/config/config_types.go —— PluginInstanceConfig.UnmarshalYAML
defaultEnabled := false
c.Enabled = &defaultEnabled        // 只有显式出现 enabled: 键才会被覆盖
```

而 `internal/pluginhost/host.go` 的加载循环是：

```go
item, ok := rc.Items[file.ID]
if !ok {
    item = defaultRuntimeItemConfig(file.ID)   // Enabled: false
}
if !item.Enabled {
    continue                                   // ← 文件被发现，但永不注册
}
```

所以有两种写法会让插件「装上但未注册」：

1. **`plugins.configs.opencodego-pool` 整项缺失** —— 走 `defaultRuntimeItemConfig`，`Enabled` 为 false
2. **该项存在但漏写 `enabled: true`** —— nil 被规范化为 false

> 官方插件文档里「`enabled` 未写时按启用处理」这一句与代码不符，以代码为准。

### 本部署的实际取值

据当前的 `openai-compatibility` 配置（provider 名 `opencode_go`，base-url 带 `/chat/completions` 后缀）：

| 项 | 值 |
|---|---|
| `provider_key`（候选属性与日志里可见） | `openai-compatible-opencode_go` |
| auth ID 形如 | `openai-compatibility:opencode_go:<12位token>` |
| `base_url_prefix` | 保持默认 `https://opencode.ai` 即可（匹配到 origin 为止） |
| `usage_url` | **必须手填**：`https://opencode.ai/zen/go/v1/usage` |

**为什么 `usage_url` 不能推导**：base-url 是 `.../zen/go/v1/chat/completions`，而用量接口是
`.../zen/go/v1/usage` —— 两者不是简单的后缀替换关系。这正是把 `usage_url` 定为
「由配置给出、不做推导」的原因。

### 关于 `disable-cooling: true`

这份配置在 provider 上关掉了宿主的凭证冷却机制，意味着 **429 之后宿主不会主动把该 key 冷落下线**。两个后果：

1. **本插件成为唯一做用量保护的一层。** 资源页上的 `rebound` 与 `429 fast path` 计数尤其值得盯。
2. 429 快路径仍应生效：它由用量上报链路投递（`usage.handle`），与冷却开关无关。
   ⚠️ 这一点尚未在真机确认，首次联调时请重点看 `429 fast path` 计数是否随上游 429 增长。
3. **无 `x-opencode-session` 的请求将完全没有用量保护。** 插件按设计不干预这类请求
   （见下节取舍），而宿主又因为 `disable-cooling: true` 不会把打满的 key 摘掉 —— 于是
   它们会持续 round-robin 到已经打满的 key 上。**前提是 opencode 客户端在所有路径上都带这个头**；
   若某条路径不带，那条路径就等于退回无保护状态。

**`usage_url` 和 `config_path` 缺一不可。** 任一缺失、文件读不到、或 token 配方对不上时，插件会拒绝处理每一次 pick 并写 error 日志，把调度权交还宿主。宁可退回默认行为，也不带着可能是错的映射去路由。

`exclude_keys` 与 `pin_sessions` 里的引用接受三种写法，任一匹配即可：完整 api-key、auth ID（`openai-compatibility:opencode_go:<token>`）、或资源页上显示的 12 位 token。

```yaml
      exclude_keys:
        - "sk-xxxxxxxx"                                  # 按 api-key
        - "openai-compatibility:opencode_go:<12位token>"   # 按 auth ID
      pin_sessions:
        "session-abc": "<12位token>"                       # 把某会话钉死在某个 key 上
```

> **token 只能从运行中的插件读出来。** 它是
> `sha256(kind ‖ \0 ‖ api-key ‖ \0 ‖ base-url ‖ \0 ‖ proxy-url)[:12]`，必须用你的**真实** api-key 才算得出。
> 资源页的 `token` 列和 `/state` JSON 的 `token` 字段就是它。
>
> 本文档早期示例里出现过的 `be6bf799f8f0` / `06ff50e27a06` 是**占位符** `oc_sk_xxxx0N` 的哈希，
> 抄它不会匹配到任何东西 —— 而且 `exclude_keys` 的未命中只会记一条 warn（不泄漏引用内容），
> 不会报错，所以务必从页面上复制。

## 3. 路由规则

```
1. 只处理 base_url 以 base_url_prefix 开头的候选和 provider，其余交还宿主
2. 请求没有 x-opencode-session 头        → 不干预，宿主内建调度接管
3. 候选无法映射回已知 key               → 不干预 + 报错
4. 候选跨多个 provider                   → 不干预
5. 排除：usage 404/401 撤销、任一窗口 status != "ok"、
        命中 exclude_keys、处于 429 保留期内（60s）
6. 全部被排除 → 兜底放行“离天花板最远”的那个（已撤销/被 exclude 的永不释放）
7. pin_sessions 命中且可用 → 直接使用
8. 会话已有绑定且仍可用 → 复用（续期 TTL）
   绑定已不可用         → 立即改绑到规则 9 选出的 key
9. 排序：rolling.percent 升序；未知或超过 stale_after 未刷新的排最后；
        平手按 auth ID 字典序（确定性，绝无随机）
```

排序键只取 `rolling.percent`：weekly/monthly 逼近 100% 时是硬门槛（规则 5），不参与排序。

## 4. 运维须知（两条来自宿主的坑）

**⚠️ 删掉一个 key 会掐断该 provider 下所有在途流。** 宿主 `Manager.Remove` 会调用整个 provider 级别的 `CloseExecutionSession`。手工删 key 时，正在跑的其他 opencode 会话会一起断。

**⚠️ 宿主自己写 config.yaml 不是原子的。** 走管理面板/management API 改 key 时，宿主用截断重写；如果重载恰好落在写入中间，会加载失败并且哈希不更新，运行时停在旧配置直到下一次事件。手工编辑（编辑器通常写临时文件再 rename）没有这个问题。**避免在高并发时段批量改 key。**

## 5. 验证

```bash
# 1. 插件是否注册成功
curl -s -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" \
  http://127.0.0.1:8317/v0/management/plugins | jq '.plugins[] | select(.id=="opencodego-pool")'
# 关注 registered: true 与 effective_enabled: true

# 2. 只读资源页（免鉴权）
open http://127.0.0.1:8317/v0/resource/plugins/opencodego-pool/status

# 3. JSON 快照（需管理密钥）
curl -s -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" \
  http://127.0.0.1:8317/v0/management/plugins/opencodego-pool/state | jq
```

资源页上要看的：

| 列 | 含义 |
|---|---|
| `token` | 该 key 的短标识，用 `exclude_keys` / `pin_sessions` 时引用它 |
| `rolling` / `weekly` / `monthly` | 各窗口用量，以及被上游标记的状态 |
| `state` | `usable` 还是 `excluded`，以及**被排除的具体原因** |
| `sessions` | 当前绑定到该 key 的会话数 |
| `fetched` / `last seen` | 用量最后一次成功刷新的时间 / 最后一次出现在候选里的时间 |
| Counters | `rebound` 涨得快说明 key 频繁打满；`mapping failed` **非零即表示配方或配置失配，需要立刻排查** |

**页面永远不显示明文 api-key**，JSON 快照同样不包含。

## 6.1 宿主版本兼容：schema 版本靠回显，不靠硬编码

宿主在 `plugin.register` 请求里把自己的 `pluginabi.SchemaVersion` 传进来，并**拒绝任何声明版本高于它的插件**：

```go
// internal/pluginhost/rpc_client.go
if resp.SchemaVersion > pluginabi.SchemaVersion {
    return pluginapi.Plugin{}, fmt.Errorf("plugin schema version %d is not supported", resp.SchemaVersion)
}
```

**这个拒绝完全到不了插件这边**——`plugin.register` 仍然返回成功，唯一症状是管理接口里 `registered: false`。
硬编码一个高版本，就会在老宿主上静默装不上。实测：v7.2.150 的 `SchemaVersion` 是 **5**，硬编码 6 直接注册失败。

所以插件回显宿主宣告的版本（上限为本插件理解的最高版）：

```go
func resolveSchemaVersion(hostVersion uint32) uint32 {
    if hostVersion == 0 { return 1 }        // 宿主把缺失视为最初的契约
    if hostVersion > SchemaVersion { return SchemaVersion }
    return hostVersion
}
```

宿主宣告的版本低于本插件的最高版时，`plugin.register` 会写一条 warn，提示较晚加入的能力可能被忽略。

已知的版本差异：

| 能力 | v7.2.150（schema 5） | v7.3.15（schema 6） |
|---|---|---|
| `scheduler` | ✓ | ✓ |
| `usage_plugin` | ✓ | ✓ |
| `management_api` | ✓ | ✓ |
| `quota_provider` | **不存在** | ✓ |

> `schema_version` 只跟踪 RPC JSON 契约，**不跟踪能力增删**（官方说明：能力新增不递增版本号）。所以插件无法从版本号推断某个能力是否可用——只能声明，由宿主忽略不认识的字段。

## 6. 已知取舍

| 项 | 取舍 |
|---|---|
| 状态 | **纯内存**，进程重启后会话重新分配（不做落盘） |
| 无 `x-opencode-session` 的请求 | 完全不干预（已确认的取舍）。**叠加本部署的 `disable-cooling: true` 后，这类请求零保护**：宿主既不会摘掉打满的 key，插件也不参与选择 |
| 轮询间隔 | 默认 600s；429 快路径把发现延迟压回秒级，但**静默打满**仍可能滞后一个周期 |
| 429 保留期 | 固定 60s：窗口内成功的 usage 读取不能清除怀疑，避免滞后的用量接口抹掉 429 证据；代价是误判时该 key 最多被冷落 60s |
| 宿主版本兼容 | schema 版本由宿主宣告、插件回显（见下），所以同一个构建能在新旧宿主上加载。但**能力**是按宿主版本走的：`quota_provider` 在 **v7.2.150 不存在**，所以那个版本上 CPA 管理面板的配额列不会有本插件的数据（插件自己的资源页不受影响） |
| 会话绑定 | TTL 24h 滑动续期，上限 1 万条 LRU 淘汰 |
| 插件被替换/禁用 | 旧实例的轮询协程可能继续跑（纯内存、无状态文件，多跑一个只是多发一份 HTTP） |

## 7. 开发

```bash
make check      # go vet + go test
make race       # 竞态检测
make build      # 构建 .dylib
make abicheck   # 用 dlopen + 真实 C 函数表加载 .dylib，端到端验证 ABI
```

代码结构：

```
main.go                    C ABI 导出、方法分发
internal/
  envelope 语义见 hostbridge  JSON 信封与 plugin→host 调用
  keysource/                token 哈希配方 + config.yaml 解析
  usage/                    usage 响应模型与容错解析
  pool/                     key 注册表、会话绑定、选择算法（纯逻辑，无 IO）
  pluginconfig/             插件自身配置
  service/                  编排：配置同步、轮询、每个 RPC 方法
  mgmtui/                   内嵌的资源页
```

测试策略：`pool` / `usage` / `keysource` / `pluginconfig` 是纯逻辑单测；`service` 用一个可编程的假 usage HTTP server + 假 host 桥，端到端驱动 `register → config 同步 → scheduler.pick → usage.handle`，断言选中的 auth ID。

## 8. 排障

| 现象 | 先看什么 |
|---|---|
| 资源页显示 "Routing is disabled" | 页面上的 Problems 列表；通常是 `usage_url` / `config_path` 没配 |
| 装了但 `registered: false` | 宿主 `validPlugin()` 要求 `Metadata` 的 Name / Version / Author / **GitHubRepository 全部非空**。这几项为空时 `plugin.register` 这个 RPC 仍然返回成功，宿主随后静默拒绝，插件侧看不到任何错误。日志里找 `returned invalid metadata or no capabilities`。本插件的 `author` / `repository` 有非空默认值，只有在配置里显式写成空字符串才会触发 |
| 插件列表里根本没有 | dylib 不在 `plugins_dir` 下的 `darwin/arm64/` 或它本身；或文件名不等于配置键（ID 来自文件名） |
| `configured: false` | `plugins.configs.opencodego-pool` 整项缺失 |
| `enabled: false` | 该项存在但漏写 `enabled: true`（缺省值被规范化为 false） |
| 插件完全不生效 | `plugins.enabled` 是否为 true；`priority` 是否低于其它 scheduler 插件 |
| `mapping failed` 计数在涨 | `GET /v0/management/plugins/opencodego-pool/state` 看是否 `healthy`；对照 config.yaml 的 `name` / `base-url` 是否被改过 |
| 用量一直是 `never` | usage 接口是否可达；`log_level` 调到 `debug` 看拉取失败原因 |
| 某些 key 一直 `excluded` | 页面上会写明原因（`exhausted: weekly=rate-limited` / `revoked` / `excluded by config` / `recent upstream 429`） |
| 改了 key 但插件没反应 | 确认 config.yaml 里 `plugins.enabled` 仍为 true；插件同时有 30s 的 config.yaml mtime 巡检兜底 |

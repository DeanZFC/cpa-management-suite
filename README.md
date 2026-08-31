# CPA Management Suite

这是一个面向 CLIProxyAPI 的公共管理套件插件。当前提供账号管理和模型价格管理：按认证账号设置最大并发容量，并提供请求、Token 和费用统计；后续可继续扩展其他管理模块。

## 能力

- 调度时只选择 `active < capacity` 的认证账号。
- 请求经过实际账号选择后绑定账号，在成功、失败、拒绝和取消时释放并发。
- 通过 `usage.handle` 按 CPA 提供的 `AuthID` 累计请求和 Token。
- 管理页面可编辑容量、启用/停用账号、清空统计。
- 管理页面可直接新增和编辑 CPA 认证账号：支持粘贴或选择 JSON 文件，并编辑显示名称、代理、Base URL、优先级、备注、WebSocket 和 API 模式。
- 新增/编辑通过 CPA SDK 的 `host.auth.save` 和 `host.auth.get` 完成，保存后由 CPA 自动重新加载认证文件。
- 状态 JSON 和价格快照原子写入，放在插件挂载目录中，容器更新不会丢失。
- 费用规则参考 CPA Usage Keeper：从 Models.dev 同步模型价格，分别计算普通输入、缓存读取、缓存写入和输出。
- 管理页面的“模型价格”模块支持查看、搜索、手动修改和新增模型价格，也支持点击“同步价格”强制从 Models.dev 拉取最新价格；同步和手动修改都会持久化到价格快照。

页面顶部使用模块菜单布局，当前包含“账号管理”和“模型价格”。切换到“模型价格”后点击“同步价格”即可立即拉取价格，不受自动刷新间隔限制；也可以直接编辑表格中的价格后保存。

## 构建 Linux amd64

插件必须使用与 CPA 匹配的 Go SDK。构建时需要准备 CPA 源码目录，并通过 `go mod edit` 指向它。假设源码目录是 `/tmp/cliproxyapi-reference`：

```bash
cd cpa-management-suite
./build-linux.sh
```

脚本会自动准备 `/tmp/cliproxyapi-reference` 中的 CPA 源码并生成 `cpa-management-suite.so`。也可以手动执行：

```bash
git clone --depth=1 https://github.com/router-for-me/CLIProxyAPI.git /tmp/cliproxyapi-reference
go mod edit -replace github.com/router-for-me/CLIProxyAPI/v7=/tmp/cliproxyapi-reference
GOOS=linux GOARCH=amd64 CGO_ENABLED=1 \
  go build -buildmode=c-shared -o cpa-management-suite.so .
rm -f cpa-management-suite.h
```

如果 CPA 源码不在该目录，把 `go mod edit` 命令中的路径替换成实际路径。不要把 macOS 生成的 `.dylib` 放到 Linux CPA 容器中；Linux 必须生成 `.so`。构建完成后可以删除本地 `replace`：

```bash
go mod edit -dropreplace github.com/router-for-me/CLIProxyAPI/v7
```

## CPA 配置

插件目录必须挂载到容器的 `/CLIProxyAPI/plugins`：

```yaml
services:
  cli-proxy-api:
    volumes:
      - ./cliproxyapi/config.yaml:/CLIProxyAPI/config.yaml
      - ./cliproxyapi/auths:/root/.cli-proxy-api
      - ./cliproxyapi/logs:/CLIProxyAPI/logs
      - ./cliproxyapi/plugins:/CLIProxyAPI/plugins
```

首次安装插件时，CPA 仍需要启用插件目录；完成安装后，CPA Management Key、`default_capacity`、拒绝策略和价格同步配置可以直接在 CPAMP 的“插件管理 → 编辑配置”页面修改，不需要再登录服务器编辑配置文件。

如果插件尚未被 CPA 发现，才需要在 CPA 的 `config.yaml` 中加入或确认：

```yaml
plugins:
  enabled: true
  dir: "/CLIProxyAPI/plugins"
  configs:
    cpa-management-suite:
      enabled: true
      cpa_management_key: ""
      priority: 100
      state_file: "/CLIProxyAPI/plugins/account-capacity-state.json"
      pricing_file: "/CLIProxyAPI/plugins/account-capacity-pricing.json"
      pricing_url: "https://models.dev/api.json"
      pricing_refresh_hours: 24
      default_capacity: 1
      reject_when_full: true
```

CPA 的 `UsageRecord` 只提供 Token，不提供统一费用字段。插件会读取 Models.dev 的模型价格目录并保存本地快照；模型未匹配到价格时，该请求费用为 0。价格单位与 Keeper 一致，为 USD/百万 Token。

费用计算与 Keeper 一致：普通输入为 `max(input - cache_read - cache_creation, 0)`，再分别乘以普通输入、缓存读取、缓存写入和输出价格，最后乘以模型倍率。插件启动时加载本地快照，并每小时检查一次，快照超过 `pricing_refresh_hours` 后自动更新。价格同步失败时继续使用本地快照。

`state_file` 和 `pricing_file` 是高级路径配置，默认自动放在 `/CLIProxyAPI/plugins/` 下，正常使用不需要填写。通过页面保存配置时，留空的高级路径仍会回退到默认值。

## Docker 两套安装

分别准备目录并复制同一个 `cpa-management-suite.so`：

```bash
sudo mkdir -p /root/cpa-1/cliproxyapi/plugins /root/cpa-2/cliproxyapi/plugins
sudo cp cpa-management-suite.so /root/cpa-1/cliproxyapi/plugins/
sudo cp cpa-management-suite.so /root/cpa-2/cliproxyapi/plugins/
```

然后分别重启：

```bash
cd /root/cpa-1 && sudo docker compose up -d --force-recreate cli-proxy-api
cd /root/cpa-2 && sudo docker compose up -d --force-recreate cli-proxy-api
```

管理页面在 CPA 管理页面的插件菜单中打开，或直接访问。CPA Management Key 在 CPA 的“插件管理 → CPA Management Suite → 编辑配置”中填写，页面顶部不再显示密钥输入框：

```text
/v0/resource/plugins/cpa-management-suite/dashboard
```

页面中的修改操作使用当前插件配置中的 `CPA Management Key`。两套 CPA 使用各自的 `config.yaml`、插件状态文件和管理密钥，互不影响。

`cpa_management_key` 会由插件注入管理页面，用于浏览器调用 CPA 的管理接口。它属于敏感配置，请只允许可信用户访问插件页面，不要提交到 GitHub 或公开截图。

账号管理接口：

```text
GET  /v0/management/account-capacity/accounts
PUT  /v0/management/account-capacity/accounts              # 容量、启停和标签
POST /v0/management/account-capacity/accounts/auth         # 新增认证文件
GET  /v0/management/account-capacity/accounts/auth         # 按 auth_index 读取认证文件
PUT  /v0/management/account-capacity/accounts/auth         # 编辑并覆盖认证文件
```

当前 CPA 插件 SDK 没有提供删除认证文件的 `host.auth.delete` 回调，因此页面的“停用”是安全的逻辑停用；不会提供可能造成误删的伪删除按钮。物理删除仍应使用 CPA 自带的认证文件管理页面。

模型价格接口：

```text
GET  /v0/management/account-capacity/pricing             # 读取价格
PUT  /v0/management/account-capacity/pricing             # 保存或新增单个模型价格
POST /v0/management/account-capacity/pricing/sync       # 强制同步 Models.dev
```

## 注意

项目名称已从早期版本的 `CPA Account Capacity` 改为 `CPA Management Suite`。为了兼容已安装版本，管理接口和状态文件仍保留 `account-capacity` 路径及文件名。

当 CPA 自身开启 Home 模式时，Home 控制平面会接管认证调度；本插件面向普通单机调度模式。插件更新时不要删除 `account-capacity-state.json`，否则会丢失容量设置和用量统计。

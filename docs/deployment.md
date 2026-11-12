# 部署

cmdgate 有三种运行方式，共用同一份程序和同一套数据布局。

| 模式 | 隔离边界 | 用途 |
|---|---|---|
| dev | 无 | 熟悉用法、调试操作 |
| local | 独立 OS 用户 | 个人在本机使用 |
| server | 另一台机器 | 团队共用 |

## dev 模式

```bash
cmdgate serve --dev
```

以当前用户运行，数据目录是 `~/.cmdgate-dev`。在其他终端里使用前先设置：

```bash
export CMDGATE_DATA_DIR=~/.cmdgate-dev
```

这个模式下，任何以你的身份运行的程序都能读到凭证、批准请求。不要在里面放真实凭证。

## local 模式

### 安装

可使用 [README 中的一键安装脚本](../README.zh-CN.md#安装)，或从 [GitHub Releases](https://github.com/zreo0/cmdgate/releases) 下载对应平台的归档，校验 `SHA256SUMS` 后解压执行：

```bash
sudo ./cmdgate install --mode local
```

安装做了这些事：

- 把二进制复制到 `/opt/cmdgate/bin/cmdgate`，属主为 root
- 创建隐藏的系统用户 `_cmdgate`，没有登录 shell 和个人目录
- 创建数据目录 `/opt/cmdgate`
- 写入 root 所有的系统 PATH 配置：macOS 使用 `/etc/paths.d/cmdgate`，Linux 使用 `/etc/profile.d/cmdgate.sh`
- 注册并启动系统服务（macOS 上是 launchd，Linux 上是 systemd）

安装后打开新的登录 shell，即可直接运行 `cmdgate`。macOS 依靠 `path_helper` 加载配置，Linux 依靠登录环境加载 `/etc/profile.d`；不加载这些配置的 shell 或 Agent 运行器仍需在其环境中设置 PATH。已经运行的终端和 Agent 不会自动获得新的环境，应从新 shell 启动。

PATH 配置目录必须由 root 所有且普通用户不可写，配置文件使用 `0644` 权限。重复安装不会追加重复内容；遇到同名自定义文件、符号链接或不安全权限时拒绝覆盖。安装器不修改个人 shell 文件、不创建 `/usr/local/bin` 快捷入口。服务启动路径和脚本的固定 PATH 保持不变。管理操作始终使用上面的绝对路径，避免从用户可写目录加载同名程序。升级已有安装时重新运行 `install`，让系统服务切换到受保护的二进制路径；Linux 上已有服务还需执行 `sudo systemctl restart cmdgate`。

服务端只监听 `/opt/cmdgate/run/cmdgate.sock`，不开任何网络端口。

没有 systemd 的环境（如容器）加 `--no-service`，然后自己以 `_cmdgate` 用户启动 `cmdgate serve`。

### 配置审批渠道

```bash
sudo /opt/cmdgate/bin/cmdgate channel add telegram
```

向导会依次要求：

1. 输入或确认渠道名称，默认 `telegram`，可用 `--name ops` 提供另一个默认值；同名配置需明确确认覆盖，回车取消
2. 在 Telegram 里找 @BotFather 创建一个 bot，粘贴它的 token
3. 用你自己的账号给这个 bot 发一条消息，向导据此记下审批人
4. 给审批人起一个别名（默认 `owner`），它会出现在审计日志里
5. 在 Telegram 里点一下测试消息上的按钮，确认链路是通的

**审批账号是整个系统的信任根。** 不要在 Agent 能操作的电脑上登录这个账号的网页版或桌面版，否则能操作浏览器的 Agent 可以自己点批准。

操作名和参数会经过 Telegram 的服务器。介意的话，这个渠道不适合你。

### 安装操作和凭证

```bash
sudo /opt/cmdgate/bin/cmdgate admin sync ./my-actions
sudo /opt/cmdgate/bin/cmdgate secret set DEPLOY_TOKEN
sudo /opt/cmdgate/bin/cmdgate admin doctor
```

`secret set` 在终端里隐藏输入。请自己输入，不要让 Agent 代劳。

### 迁走旧凭证

把原来放在 `.env`、`~/.aws`、`~/.kube`、shell 配置里的生产凭证移入 cmdgate，并从原处删除。这一步不做，Agent 可以直接使用旧凭证，cmdgate 起不到作用。

### 日常管理

所有管理命令都需要 `sudo`，因为系统分不清敲命令的是你还是 Agent。macOS 可以给 `sudo` 开启 Touch ID。

```bash
sudo /opt/cmdgate/bin/cmdgate admin request list         # 查看请求
sudo /opt/cmdgate/bin/cmdgate admin request show <id>
sudo /opt/cmdgate/bin/cmdgate admin audit tail -n 50     # 审计日志
sudo /opt/cmdgate/bin/cmdgate admin approve <id>         # 渠道故障时的应急批准
sudo /opt/cmdgate/bin/cmdgate admin prune --older 30d    # 清理旧请求
sudo /opt/cmdgate/bin/cmdgate admin doctor               # 健康检查
```

服务端日志在 `/opt/cmdgate/log/server.log`（macOS）或 `journalctl -u cmdgate`（Linux）。

### 卸载

```bash
sudo /opt/cmdgate/bin/cmdgate uninstall            # 保留数据目录
sudo /opt/cmdgate/bin/cmdgate uninstall --purge    # 连同数据目录一起删除
```

卸载时只删除内容和权限仍与安装器生成结果一致的 PATH 配置，不论是否传入 `--purge`；修改过的文件会保留并提示。此前手动写入个人 shell 配置的 PATH 不会被清理，已运行进程的 PATH 也不会被修改。

## server 模式

服务端装在一台团队成员都能访问的机器上，成员的电脑上只有客户端和一个令牌。

### 服务端

```bash
sudo ./cmdgate install --mode server --listen 0.0.0.0:8443 \
    --tls-cert /etc/cmdgate/cert.pem --tls-key /etc/cmdgate/key.pem
```

证书和私钥要让 `_cmdgate` 用户可读。

默认必须提供 TLS。如果加密由前面的反向代理或 VPN 负责，可以用 `--insecure` 监听明文 HTTP，服务端启动时会打印警告。不建议直接暴露在公网上。

旧版本遗留的待审批请求没有完整交付记录，升级后需取消并重新提交，或由管理员核实后使用应急审批入口。

渠道、操作和凭证的配置与 local 模式相同，都在服务端机器上用 `sudo` 完成。

### 用户

```bash
sudo /opt/cmdgate/bin/cmdgate user add alice       # 输出一个令牌，只显示这一次
sudo /opt/cmdgate/bin/cmdgate user remove alice    # 吊销，立即生效
sudo /opt/cmdgate/bin/cmdgate user list
```

服务端只保存令牌的 hash。

### 客户端

```bash
cmdgate login https://cmdgate.internal:8443    # 粘贴令牌
cmdgate list
```

自签证书用 `cmdgate login --ca cert.pem <url>`。登录信息保存在 `~/.config/cmdgate/client.json`，`cmdgate logout` 删除。

登录后的用法和本机完全一样。令牌只能用来发起请求和查看自己的请求，不能批准，也不能调用管理接口。它存在成员的电脑上，Agent 读得到，这是设计内的。

### 按操作授权

```yaml
requesters: [alice, bob]
approval:
    approvers: [owner]
    allow_self: false
```

- `requesters`：只有列出的用户能看到和发起这个操作
- `approvers`：只有列出的审批人能批准
- `allow_self: false`：别名与请求者同名的审批人不能批准这个请求。要用这一项，审批人的别名需要与用户名一致

## 迁移

```bash
# 旧机器
sudo /opt/cmdgate/bin/cmdgate admin export backup.cgx

# 新机器，先安装
sudo /opt/cmdgate/bin/cmdgate install --mode local
sudo /opt/cmdgate/bin/cmdgate admin import backup.cgx
sudo /opt/cmdgate/bin/cmdgate admin sync ./my-actions
```

导出包用口令加密，包含渠道配置、用户和凭证。它不包含：

- **操作脚本**。它们的源头是你的仓库，在新机器上重新 `sync`
- **请求记录和审计日志**。加 `--with-history` 才带上
- **部署方式**。新机器的模式、监听地址、TLS 保持它自己的设置，所以本地导出的包可以导入到 server 模式的机器

## 数据目录

```
/opt/cmdgate/
    VERSION            布局版本
    config.yaml        模式、监听地址、渠道的非敏感配置
    users.yaml         server 模式的用户和令牌 hash
    actions/           已安装的操作
    actions.lock       每个操作的 hash 和安装时间
    secrets/           凭证，每个一个文件
    requests/<id>/     请求记录、输出、文件参数的快照
    audit/             按月的审计日志
    home/              脚本运行时的 HOME
    log/               服务端日志
    run/cmdgate.sock   客户端入口
```

定义类内容（配置、操作、安装记录）归 root，服务用户只读。运行数据和凭证归服务用户。除 `run/` 外，普通用户都读不到。

用 `CMDGATE_DATA_DIR` 可以换一个数据目录。

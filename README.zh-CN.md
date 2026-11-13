<div align="center">

# cmdgate

面向 AI Agent 的命令审批网关：隔离凭证、远程审批、按策略执行。

[![CI](https://github.com/zreo0/cmdgate/actions/workflows/ci.yml/badge.svg)](https://github.com/zreo0/cmdgate/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/zreo0/cmdgate)](https://github.com/zreo0/cmdgate/releases)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Platforms](https://img.shields.io/badge/platforms-macOS%20%7C%20Linux-lightgrey)](#安装)

**简体中文** · [English](README.md)

[安装](#安装) · [配置](#配置) · [通知与审批渠道](#通知与审批渠道) · [编写命令](#编写命令) · [开发](#开发)

</div>

---

你想让 Agent 帮忙部署应用、修改数据，却不想把生产密钥和所有操作权限直接交给它。

cmdgate 把需要权限的命令交给独立服务执行。Agent 提交命令和参数，常规任务按策略自动执行，敏感任务等待你通过远程渠道（如 Telegram）确认。完成后，Agent 获取状态和脱敏后的输出，继续处理后续工作。

## 那么有人要问了：Agent 不是自带权限审批吗？或者上下文要求禁止危险操作不就好了吗？

![四格小剧场](docs/assets/agent-approval-comic.zh-CN.png)

*虚构小剧场：当 Agent 为了完成任务自作主张……*

上下文里的“不要做”不能代替权限隔离；内置审批也取决于实际配置和覆盖范围。cmdgate 把凭证和执行权限放到独立服务中，让需要审批的命令必须经过服务端授权才能执行。

## 特点

| 特点 | 解决什么问题 |
| --- | --- |
| 凭证隔离 | 独立 OS 用户持有凭证，Agent 无需直接接触生产密钥 |
| 远程审批 | 在独立于 Agent 工作环境的渠道中查看请求、批准或拒绝 |
| 按策略授权 | 默认需要审批；明确配置为 `auto` 的命令自动执行，减少重复确认 |
| 执行前预检 | 提前检查条件、展示执行计划，检查失败就停止请求 |
| 审批与执行绑定 | 审批关联具体参数和命令目录 hash，执行前再次验证，一次批准只使用一次 |
| 结果可追溯 | 保存请求状态和审计记录，向 Agent 返回脱敏后的执行输出 |
| 本机与团队共用 | 本机通过 Unix socket 连接，共享服务通过 TLS 和用户令牌访问 |

安全边界依赖受信任的管理员和命令脚本，以及 Agent 无法取得管理员权限或直接读取凭证。cmdgate 不是不可信脚本的沙箱，详见[安全说明与威胁模型](SECURITY.md)。

## 安装

安装最新稳定版：

```bash
curl -fL --progress-bar https://raw.githubusercontent.com/zreo0/cmdgate/main/scripts/install.sh | sh
```

[安装脚本](scripts/install.sh)会显示阶段提示和下载进度；系统安装会在下载、校验完成后请求 sudo 权限。脚本会自动：

- 识别 macOS / Linux 和 arm64 / amd64，下载对应二进制并校验 SHA-256。
- 通过 sudo 创建独立用户 `_cmdgate` 和系统服务，将程序安装到 `/opt/cmdgate/bin/cmdgate`，数据保存在 `/opt/cmdgate`。
- 注册系统 PATH，保留已有配置和数据；打开新的登录终端后即可使用 `cmdgate`。

Linux 默认使用 systemd。共享服务或其他部署方式见[部署指南](docs/deployment.md)。

> 下文管理员命令统一使用 `sudo cmdgate`，须确保它指向受保护的 `/opt/cmdgate/bin/cmdgate`。若 sudo 找不到命令或指向其他程序，请使用完整路径，例如 `sudo /opt/cmdgate/bin/cmdgate update`。

<details>
<summary>只安装客户端</summary>

连接已有的远端服务，无需 sudo，也不创建本机服务：

```bash
curl -fL --progress-bar https://raw.githubusercontent.com/zreo0/cmdgate/main/scripts/install.sh \
    | sh -s -- --client-dir "$HOME/.local/bin"
export PATH="$HOME/.local/bin:$PATH"
cmdgate login https://your-server.example:8443
```

按提示输入管理员提供的令牌。客户端安装不修改系统 PATH，可按需将上述 `export` 加入自己的 shell 配置。

</details>

<details>
<summary>安装指定版本与升级</summary>

已有系统安装可更新到最新稳定版，也可指定版本：

```bash
sudo cmdgate update
sudo cmdgate update --version v0.1.0
```

将 `v0.1.0` 替换为所需的已发布版本。命令会下载对应平台的官方发布包，校验 SHA256，原子替换二进制，并重启已注册的 systemd 或 launchd 服务。现有配置、凭证、操作和请求历史都会保留，不改写服务定义及 PATH 配置。使用 `--no-service` 安装时，需自行重启手动管理的服务。请在没有操作执行时更新，服务重启会中断正在执行的操作。

下载或校验失败时保留旧二进制；替换后若重启失败，命令报错并保留新二进制，需要修复服务后手动重启。此命令更新本机的系统安装，不更新远端服务或仅安装的客户端。

首次安装，或旧版本尚无 `update` 命令时，使用安装脚本：

将 `v0.1.0` 替换为所需的[已发布版本](https://github.com/zreo0/cmdgate/releases)：

```bash
curl -fL --progress-bar https://raw.githubusercontent.com/zreo0/cmdgate/main/scripts/install.sh \
    | sh -s -- --version v0.1.0
```

省略 `--version` 即安装最新稳定版，已有配置和数据会保留。通过安装脚本升级 Linux 上已有服务后，还需执行 `sudo systemctl restart cmdgate`。仅安装客户端时，使用原来的 `--client-dir` 再次运行客户端安装命令。

</details>

<details>
<summary>卸载</summary>

以下命令按需选择一条：

```bash
sudo cmdgate uninstall          # 移除服务和程序，保留数据
sudo cmdgate uninstall --purge  # 同时删除数据，包括凭证
```

未被修改的系统 PATH 配置会一并清理，自定义内容和个人 shell 配置保留。只安装客户端时，删除客户端二进制即可。

</details>

## 配置

安装后由管理员配置审批渠道、注册命令并保存凭证：

```bash
sudo cmdgate channel add telegram        # 按向导配置远程审批渠道
sudo cmdgate admin sync ./my-commands    # 注册命令目录，格式见下方“编写命令”
sudo cmdgate secret set DEPLOY_TOKEN     # 保存命令需要的凭证，交互输入不回显
sudo cmdgate admin doctor                # 检查权限、命令、凭证和渠道
```

> 从新终端启动 Agent，不加载系统 PATH 的运行器配置见[部署指南](docs/deployment.md)。

把生产凭证从 `.env`、`~/.aws` 等 Agent 可读位置移走。审批账号也应放在 Agent 无法操作的环境中，例如独立手机。

配置完成后，你或 Agent 使用普通用户身份请求命令：

```bash
cmdgate list
cmdgate describe deploy
cmdgate run deploy --env prod --version 1.2.3
cmdgate wait --timeout 100s <id>
```

需要审批时，`run` 返回请求 ID 和 `pending_approval`；批准后由服务端自动执行。`wait` 超时退出码为 `3`，可对同一 ID 继续等待，不要重复提交。也可用 `cmdgate status <id>` 查询，或用 `cmdgate cancel <id>` 取消。取消不能撤销已经产生的外部副作用。

可将提供的 [Agent Skill](examples/skill/SKILL.md) 接入 Agent，让它通过 `list`、`describe` 发现可用命令，并按状态等待和报告结果。

## 通知与审批渠道

渠道负责交付审批请求、接收决定和通知结果。**目前仅实现 Telegram 渠道**，支持配置多个 Bot，以渠道名称区分。

| 操作 | 命令 |
| --- | --- |
| 新增第一个 Bot | `sudo cmdgate channel add telegram` |
| 新增第二个 Bot | `sudo cmdgate channel add telegram --name ops` |
| 查看名称、类型和审批人 | `sudo cmdgate channel list` |
| 删除指定渠道 | `sudo cmdgate channel remove ops` |

新增时先确认渠道名称，默认 `telegram`；`--name` 提供可修改的默认值。同名配置需要明确确认才能覆盖，直接回车取消覆盖。建议使用 `telegram`、`ops` 等明显不同的名称，避免仅大小写或 `-`、`_`、`.` 不同而发生凭证名冲突。

每个请求只选择**一个渠道**，多个渠道时必须在命令定义中指定 `approval.channel`，不会自动广播或切换备用渠道。同一渠道内，**第一个有效的批准或拒绝生效**，后续决定不会改变结果或重复执行。删除渠道前，应先调整引用它的命令配置。

## 编写命令

一个可请求的命令在项目中称为 *action*，由一个目录描述：

```text
my-commands/
└── deploy/
    ├── action.yaml   命令定义、参数和执行策略
    ├── preflight     可选预检脚本
    └── run           实际执行脚本
```

`action.yaml` 示例：

```yaml
# 命令名称，对应 cmdgate run deploy
name: deploy
# 展示给 Agent 和审批人的说明
description: Deploy a released version
# 允许传入的参数，未声明的参数会被拒绝
params:
    # 目标环境
    env:
        # 枚举类型，只接受指定值
        type: enum
        # 可选环境
        values: [staging, prod]
    # 要部署的版本
    version:
        # 文本类型
        type: string
        # 必须完整匹配，例如 1.2.3
        pattern: '\d+\.\d+\.\d+'
# 审批策略
risk:
    # 默认需要人工批准
    default: approve
    # 按顺序匹配，第一条命中的规则生效
    rules:
        # staging 环境匹配此规则
        - when: {env: staging}
          # 匹配时自动执行，无需人工审批
          level: auto
# 仅向 run 注入的凭证名称
secrets: [DEPLOY_TOKEN]
# 人工审批设置
approval:
    # 已配置的渠道名称，多个渠道时必须指定
    channel: telegram
    # 从请求创建开始计算的审批有效期
    timeout: 30m
# 此命令最多同时执行一个请求，其余排队
concurrency: 1
# 脚本执行设置
exec:
    # 从开始执行脚本计时，超时则终止
    timeout: 5m
```

`run` 和 `preflight` 必须可执行。参数通过 `CMDGATE_PARAM_<NAME>` 环境变量传入，声明的凭证通过同名环境变量注入 `run`。脚本输出会在执行结束后脱敏并返回 Agent，可用于传递执行结果。

`preflight` 用来检查条件并向审批人展示计划，不注入凭证；只读性由脚本作者保证。脚本使用固定的系统 PATH，额外工具应使用可信绝对路径。

```bash
chmod +x my-commands/deploy/run
# 如果提供了 preflight，也为它添加执行权限
sudo cmdgate admin sync ./my-commands
```

修改命令后重新执行 `admin sync`。完整参数和策略见[命令编写指南](docs/actions.md)，可参考[示例命令](examples/actions)；其中 `deploy` 仅演示流程，不会真正部署。

## 开发

需要 [go.mod](go.mod) 指定的 Go 版本和 Make；运行隔离验收测试时还需要 Docker。

```bash
make build           # 构建当前平台的 ./cmdgate
make check test      # 格式、静态检查、单元和 CLI 端到端测试
go test -race ./...   # 并发相关改动的检查
make test-isolation  # 一次性 Linux 容器中的权限隔离验收
make test-scripts    # 发布与安装脚本的离线回归
```

<details>
<summary>开发试用：不安装系统服务</summary>

在仓库根目录运行，适合熟悉命令或调试。它以当前用户运行，**没有用户隔离**，请勿使用生产凭证；正常使用请选择上面的安装流程。

```bash
make build
./cmdgate serve --dev
```

另开一个终端，在仓库根目录执行：

```bash
export CMDGATE_DATA_DIR="$HOME/.cmdgate-dev"
./cmdgate admin sync examples/actions
./cmdgate run hello --name there
```

试用结束后，在服务端终端按 Ctrl-C 停止服务，在客户端终端执行 `unset CMDGATE_DATA_DIR`，避免后续命令继续连接试用环境。

</details>

开发约定和测试分工见[贡献指南](CONTRIBUTING.md)。安全问题请按[安全说明](SECURITY.md)报告。

## 许可

[MIT](LICENSE)

# 编写操作

一个操作（action）是一个目录，描述"可以被请求的一件事"。

```
deploy/
    action.yaml
    preflight       可选
    run
```

`preflight` 和 `run` 可以是任何可执行文件，必须带可执行权限。目录里只能有普通文件，出现符号链接会被拒绝。

## action.yaml

```yaml
name: deploy
description: Deploy a released version to an environment
params:
    env:
        type: enum
        values: [staging, prod]
    version:
        type: string
        pattern: 'v?\d+\.\d+\.\d+'
        description: a released version such as 1.2.3
    replicas:
        type: int
        min: 1
        max: 10
        default: "2"
    changes:
        type: file
        max_size: 65536
        optional: true
risk:
    default: approve
    rules:
        - when: {env: staging}
          level: auto
secrets: [DEPLOY_TOKEN]
approval:
    channel: telegram
    timeout: 30m
    approvers: [owner]
    allow_self: true
requesters: [alice, bob]
concurrency: 1
exec:
    timeout: 10m
```

| 字段 | 默认值 | 说明 |
|---|---|---|
| `name` | 必填 | 小写字母、数字、`-`、`_` |
| `description` | 空 | 显示在 `cmdgate list` 和审批消息里 |
| `params` | 无 | 见下文。未声明的参数一律拒绝 |
| `risk.default` | `approve` | `auto` 直接执行，`approve` 需要审批 |
| `risk.rules` | 无 | 按参数值细分，第一条匹配的生效 |
| `secrets` | 无 | 需要注入的凭证名 |
| `approval.channel` | 唯一已配置的渠道 | 配了多个渠道时必须指定 |
| `approval.timeout` | `30m` | 超时未批准则过期，不执行 |
| `approval.approvers` | 渠道里的全部审批人 | 审批人的别名 |
| `approval.allow_self` | `true` | 设为 `false` 时，别名与请求者同名的审批人不能批准 |
| `requesters` | 不限制 | 允许发起请求的用户名 |
| `concurrency` | 不限制 | 同时执行的上限，超出的排队 |
| `exec.timeout` | `10m` | 执行超时，超时后终止整个进程组 |

没写 `risk` 的操作需要审批。写错的字段名会导致加载失败，而不是被忽略。

## 参数类型

| 类型 | 约束 | 说明 |
|---|---|---|
| `string` | `pattern` | 正则必须匹配整个值 |
| `enum` | `values` | 必须是列出的值之一 |
| `int` | `min`、`max` | 十进制整数 |
| `file` | `max_size`（默认 1 MiB） | 调用者用 `--file <param>=<path>` 上传本地文件 |

每个参数都可以加 `description`、`default`（未传时使用）或 `optional: true`（可以不传）。

参数约束越严越好。一个 `pattern` 很宽的自由文本参数，cmdgate 只能保证它被完整地展示给审批人，判断它是否合理是审批人的事。

## 脚本拿到什么

脚本由服务端直接执行，不经过 shell。环境变量只有这些：

| 变量 | 内容 |
|---|---|
| `CMDGATE_PARAM_<NAME>` | 参数值，名字转成大写。`file` 类型的值是快照文件的路径 |
| 声明的凭证名 | 凭证值，只注入 `run`，不注入 `preflight` |
| `CMDGATE_REQUEST_ID` | 请求 ID |
| `CMDGATE_ACTION` | 操作名 |
| `CMDGATE_REQUESTER` | 请求者 |
| `PATH`、`HOME`、`TMPDIR` | `HOME` 是数据目录下的 `home/`，`TMPDIR` 是本次请求的工作目录 |

服务端固定使用 `/usr/bin:/bin:/usr/sbin:/sbin`，不会继承调用者的 `PATH` 或搜索 Homebrew 目录。额外工具应由管理员安装到调用者不可修改的路径，并在脚本中使用绝对路径；解释器和工具依赖也必须保持同样的权限边界。

脚本没有终端，必须完全非交互。标准输出和标准错误合并保存，返回给调用者之前会把凭证值替换成 `[REDACTED:<名字>]`。

退出码为 0 表示成功，非 0 表示失败，并原样成为 `cmdgate run` / `cmdgate wait` 的退出码。

## preflight

`preflight` 在创建请求时运行，不带任何凭证，60 秒超时。

- 退出码非 0：请求直接失败，不会发给审批人
- 退出码为 0：它的输出显示在审批消息里

用它做机器能判断的检查（CI 是否通过、版本是否已在 staging 验证、是否在发布窗口内），让审批人只需要判断机器判断不了的部分。

## 输入应该是引用，不是本地文件

服务端以另一个用户运行，读不到调用者的工作目录。所以：

- 发布类操作接收 commit SHA 或版本号，由脚本自己从版本库或制品库拉取
- 编译、测试、打包这类不需要凭证的工作，由 Agent 以自己的身份完成
- 只有需要凭证的那一步走 cmdgate

这样做也更安全：执行的是已经入库的内容，而不是 Agent 可以随时改动的工作目录。

确实要传一个小文件（比如一份 SQL）时用 `file` 类型。内容在创建请求时被快照，审批人看到的和最终执行的是同一份。

## 开发与安装

```bash
# 以自己的身份直接运行，不经过服务端，方便调试
# 声明的凭证如果在当前环境变量里，会原样传给脚本
DEPLOY_TOKEN=test cmdgate dev run ./deploy --env staging --version 1.2.3

# 安装或更新，会先显示与已安装版本的差异
sudo /opt/cmdgate/bin/cmdgate admin sync ./my-actions

sudo /opt/cmdgate/bin/cmdgate admin action list
sudo /opt/cmdgate/bin/cmdgate admin action remove deploy
```

操作的源头是你自己的仓库，建议用 git 管理。服务端只保存安装时的一份副本和它的 hash；已安装的脚本一旦被改动，服务端会拒绝执行，直到重新 `sync`。

## 编写建议

- **一个操作只做一件事**。不要写接收任意命令的万能脚本，那会让审批失去意义
- **让操作幂等**。执行中途服务端重启时请求会被标记为"结果未知"，不会自动重试，需要人来决定是否重跑
- **不要把凭证写进脚本或输出**。输出脱敏只能处理原样出现的凭证值，编码或拆分后的认不出来
- **高风险与低风险用参数区分**，再用 `risk.rules` 分别定等级，而不是写两个几乎一样的操作

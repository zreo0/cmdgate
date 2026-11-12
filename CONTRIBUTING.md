# 参与贡献

## 环境

- Go 版本以 `go.mod` 为准
- Docker，只在运行隔离验收测试时需要

## 常用命令

```bash
make build            # 编译到 ./cmdgate
make test             # 单元测试和端到端测试
make test-isolation   # 在 Docker 里验证独立用户下的权限隔离
make check            # gofmt 和 go vet
make test-scripts     # 离线验证发布和安装脚本
go test -race ./...    # 并发状态流转与资源生命周期
```

## 代码结构

```
cmd/cmdgate/          程序入口
internal/action/      操作目录的加载、参数校验、hash
internal/store/       数据目录：布局、配置、请求记录、审计、凭证、权限表
internal/engine/      请求的生命周期：校验、preflight、审批、执行、恢复
internal/channel/     审批渠道的约定，telegram/ 是目前唯一的实现
internal/server/      HTTP 接口和身份识别
internal/client/      客户端
internal/cli/         命令行
internal/platform/    与操作系统相关的安装步骤
e2e/                  用真实二进制跑的端到端测试
examples/             示例操作和 Skill
```

## 测试

三层测试各有分工：

- `internal/*/` 下的单元测试覆盖参数校验和引擎的状态流转
- `e2e/e2e_test.go` 以当前用户启动真实的二进制，用假的 Telegram 服务走完整链路
- `e2e/isolation_test.go` 在容器里以 root 做真实安装，验证普通用户读不到凭证、改不了操作、批准不了请求

改动涉及权限、身份识别或安装流程时，请运行 `make test-isolation`。

只为核心逻辑和将来容易改坏的地方写测试。

## 改动的原则

设计评审时用这一条检验（详见 [SECURITY.md](SECURITY.md)）：

> Agent 只能请求管理员注册的操作；敏感操作需要有效审批，显式配置为 `auto` 的操作可自动执行。凭证和操作定义必须位于 Agent 的权限之外。

具体到代码：

- 与安全相关的判断放在 `internal/engine`，不要放进渠道或客户端
- 渠道只负责收发。新增渠道时实现 `channel.Channel` 和配置向导，并只使用出站连接
- 新增数据目录下的文件时，同步更新 `internal/store/perms.go` 里的权限表
- 与平台相关的代码只放在 `internal/platform` 和 `internal/server/peercred_*.go`

## 提交

- 提交信息使用 [Conventional Commits](https://www.conventionalcommits.org/)，如 `feat: ...`、`fix: ...`
- 提交前运行 `make check test`
- 用户可见的改动请同步中英文 README 和相关文档，Release 正文自动从提交生成
- 项目规范见 [AGENTS.md](AGENTS.md)

安全问题请不要开公开的 issue，按 [SECURITY.md](SECURITY.md) 报告。


## 覆盖重点

| 层次 | 核心行为 |
| --- | --- |
| action | 参数约束、默认审批、完整目录 hash、快照和符号链接拒绝 |
| engine | 审批绑定、权限、状态持久化失败、取消竞争、限流、并发、超时和恢复 |
| 输出与渠道 | 凭证注入与截断脱敏、完整审批内容交付后才启用按钮 |
| server / CLI e2e | 单实例锁、真实命令链路、Telegram 回调、TLS、令牌吊销、导入导出 |
| isolation | 真实独立用户、服务二进制目录保护、客户端 PATH 安装/重装/卸载及冲突保护、凭证不可读、操作不可写、固定脚本 PATH |
| release scripts | tag 前置检查、提交范围、推送到临时本地 remote、平台选择、下载校验失败不安装 |

测试名和断言应说明行为，而不是复制实现。等待异步结果使用有超时的状态或事件轮询；准备步骤失败应立即报错。限时行为测试中的 timeout 与模拟长轮询延迟是测试语义的一部分，不应机械删除。保留安全回归用例，不以减少测试数量为目标。

## 发布

仓库：`zreo0/cmdgate`。版本由 Git tag 提供，无需修改版本清单，也不维护 CHANGELOG。

1. 提交所有改动，确认 `origin` 指向目标仓库。使用稳定版本格式 `vX.Y.Z`
2. 执行 `./scripts/release.sh`：默认建议本地最高稳定 tag 的下一 patch，无稳定 tag 时建议 `v0.1.0`；回车接受或输入其他 `vX.Y.Z`，再输入 `y` 确认。取消或输入结束都不会创建 tag。确认后拒绝重复 tag、运行 `make check test test-scripts`，再创建 annotated tag
   显式指定 `./scripts/release.sh v0.1.0` 时跳过交互，便于脚本调用
3. 执行 `git push origin refs/tags/v0.1.0`；也可在第 2 步直接传入 `--push`
4. 查看 GitHub Actions 的 Release 工作流：macOS/Linux 测试、race 检查与 Linux 隔离验收通过后才构建和发布

版本建议只依据本地 tag，不会自动 fetch；需要同步远端版本时先执行 `git fetch --tags origin`。预发布和非版本 tag 不参与建议，排序按版本号而非字典序。

脚本不会自动提交文件、推送其他 tag 或覆盖已有 tag。推送失败时 tag 保留在本地，修复网络或 remote 后重试打印的推送命令即可。

归档格式为 `cmdgate_vX.Y.Z_<darwin|linux>_<arm64|amd64>.tar.gz`，包含二进制、许可证和中英文 README，另发布 `SHA256SUMS`。本地构建可运行：

```bash
make dist VERSION=v0.1.0
```

产物位于 `dist/v0.1.0/`。跨平台编译使用 `CGO_ENABLED=0`，版本通过 ldflags 注入。只支持稳定版；预发布版本需要另行设计 latest 的选择规则。

Release 正文取上一个可达 `v*` tag 到当前 tag 的提交标题和短 hash；首版包含全部历史。GitHub Actions 先创建带附件的 draft，再公开发布，避免 latest 指向尚未上传完成的版本。若上传中断留下 draft，删除该未发布 draft 后重新运行失败的工作流；不要移动已发布 tag。

实现参考 [GitHub tag 触发规则](https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows#push) 和 [GitHub CLI Release 文档](https://cli.github.com/manual/gh_release_create)。

首次发布前，将仓库推送到 GitHub。工作流使用仓库自带的 `GITHUB_TOKEN`，仅发布 job 需要 `contents: write`，不需要保存个人令牌。仓库或组织策略必须允许该权限。

安装脚本默认取 `zreo0/cmdgate` 的最新稳定版，也支持 `--repo OWNER/REPO`、`--version vX.Y.Z` 和 `--client-dir DIR`。离线测试替换下载接口，不访问 GitHub、不执行真实 sudo、不安装系统服务。真实线上下载须在首个 Release 发布后验收。

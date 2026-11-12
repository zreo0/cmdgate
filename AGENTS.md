# AGENTS.md

## 项目与协作

- cmdgate 是 Go 编写的特权操作网关，支持 macOS 和 Linux；Go 版本以 `go.mod` 为准
- 中文沟通和注释，命令、错误信息、公开标识使用英文
- 保持最小改动，不做无关重构，不引入暂时用不到的抽象或依赖
- 先核实真实执行路径，再修改；缺陷修复先补复现用例，再验证修复
- 不默认提交、推送、创建 tag 或发布；用户明确授权后再执行这些操作

## 代码风格

- Go 文件统一使用 `gofmt`，保留标准制表符缩进；Shell、YAML 使用 4 空格
- 使用 Go 命名习惯，缩写保持 `ID`、`HTTP`、`URL`、`TLS`，包名简短、小写
- 标准库、项目内部包、第三方包分组导入；只为消除歧义使用导入别名
- 包注释以 `Package <name>` 开头；导出声明的文档以声明名称开头
- 常规文档注释统一使用连续的 `//` 行，段落之间用空的 `//` 行；字段和局部解释也使用独立的 `//` 行，中文句末不加句号
- `.editorconfig` 统一编辑器的换行和缩进；Go 格式仍以 `gofmt` 为准，Makefile 命令使用 tab
- 函数注释解释目的，参数、返回值、资源归属或并发要求不明显时明确说明；测试名称应描述被验证的行为
- 不逐行翻译实现；为非显然的安全边界和取舍解释原因，修改行为时同步修正注释
- 错误应带上下文；需要保留原因时使用 `%w`，用 `errors.Is` / `errors.As` 判别错误
- 不忽略关键读写、状态持久化、进程启动错误；清理时有意忽略错误应能从上下文理解原因
- `context.Context` 放在首个参数；启动 goroutine 时明确取消、结束及等待路径
- 及时关闭文件、响应体和临时资源；测试用 `t.TempDir`、`t.Cleanup`、`t.Helper`
- 以 [Effective Go](https://go.dev/doc/effective_go) 和 [Go Doc Comments](https://go.dev/doc/comment) 为参考，不为样式偏好重写正确代码

## 模块职责与安全边界

- `internal/action`：定义加载、参数校验、hash 与执行快照
- `internal/engine`：请求生命周期、审批授权、并发和限流、执行及恢复；安全决策不能只放在客户端或渠道
- `internal/channel`：出站审批收发；先交付完整内容，再允许审批
- `internal/store`：配置、凭证、请求与审计；新增数据文件必须同步 `perms.go` 权限表
- `internal/server`：传输与身份识别；本机依靠 peer UID，远端依靠 TLS 和令牌
- `internal/cli` / `internal/client`：命令行与客户端；`internal/platform` 承载平台安装逻辑
- 服务二进制固定在 root 所有的 `/opt/cmdgate/bin/cmdgate`，不得改回普通用户可写的目录
- 客户端 PATH 使用 root 管理的系统片段；安装不得覆盖同名自定义文件或符号链接，卸载只清理未修改的托管配置，不改个人 shell 文件
- action 脚本是受信任代码；批准绑定请求参数和目录 hash，执行验证过的快照；`auto` 是管理员显式授权自动执行
- 脚本环境不能继承调用方 PATH 或未声明的凭证；`serve --dev` 没有用户隔离
- 审批和执行状态持久化失败应停止执行；中断后的 `unknown` 请求不能自动重试
- 一个数据目录只能有一个服务实例；生命周期修改需检查锁、取消、等待和终态幂等性

## 测试与验证

- `make check test`：格式、静态检查、单元与真实 CLI 端到端测试
- 并发或生命周期变更：`go test -race ./...`
- 权限、身份、安装或隔离变更：`make test-isolation`，仅在一次性 Linux 容器中以 root 验证
- 发布、安装脚本或工作流变更：`make test-scripts`；构建改动另跑 `make dist VERSION=vX.Y.Z`
- 只测试核心行为、故障边界和容易回归的约束，不为简单包装或实现细节增加测试
- 异步断言等待可观察状态或事件并设置超时，不用固定 sleep 猜测操作已经完成
- 测试必须检查准备步骤的错误；不得访问真实审批账号、生产凭证或修改宿主系统服务
- 必要检查通过后结束；只有新改动、失败或具体疑点才扩大或重复验证

## 文档与发布

- README.md 为英文，README.zh-CN.md 为简体中文；功能、命令和限制必须同步
- 安全边界、威胁模型和已知局限维护在 `SECURITY.md`；操作与部署说明放在 `docs/`，不要把实现细节堆进快速开始
- 使用 Conventional Commits（`feat:`、`fix:`、`docs:`、`test:`、`chore:` 等）
- 发布使用稳定版本 `vX.Y.Z`；`scripts/release.sh` 未传版本时建议本地最高稳定 tag 的下一 patch（首次 `v0.1.0`），允许修改并要求确认；显式传版本时无需交互，`--push` 才主动推送
- GitHub Actions 在 tag 推送后验证、打包并发布；版本通过 ldflags 注入，Release 正文来自提交记录
- 不维护 CHANGELOG；不得覆盖已发布 tag 或绕过校验和发布二进制

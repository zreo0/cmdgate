<div align="center">

# cmdgate

A command approval gateway for AI agents: isolated credentials, remote approvals, and policy-based execution.

[![CI](https://github.com/zreo0/cmdgate/actions/workflows/ci.yml/badge.svg)](https://github.com/zreo0/cmdgate/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/zreo0/cmdgate)](https://github.com/zreo0/cmdgate/releases)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Platforms](https://img.shields.io/badge/platforms-macOS%20%7C%20Linux-lightgrey)](#install)

[简体中文](README.zh-CN.md) · **English**

[Install](#install) · [Configure](#configure) · [Notification and approval channels](#notification-and-approval-channels) · [Write commands](#write-commands) · [Development](#development)

</div>

---

You want an agent to deploy applications and update data without handing it your production keys and unrestricted access.

cmdgate runs privileged commands through a separate service. The agent submits a command and its parameters; routine tasks run automatically under your policies, while sensitive tasks wait for your confirmation through a remote channel, such as Telegram. When execution finishes, the agent receives the status and redacted output to continue its work.

## But don't agents already have permission approvals? Can't you just tell them to avoid dangerous actions?

![Four-panel comic](docs/assets/agent-approval-comic.en.png)

*A fictional scenario: when an agent takes matters into its own hands to finish the task…*

“Don't do that” in a prompt cannot replace permission isolation; built-in approvals also depend on their configuration and coverage. cmdgate keeps credentials and execution privileges in a separate service, where commands requiring approval must receive server-side authorization before they can run.

## Features

| Feature | What it gives you |
| --- | --- |
| Credential isolation | A separate OS user holds credentials, so agents do not need direct access to production keys |
| Remote approvals | Review, approve or deny requests through a channel outside the agent's working environment |
| Policy-based authorization | Approval is required by default; explicitly configured `auto` commands avoid repetitive confirmations |
| Preflight checks | Check conditions and show the execution plan before approval; failed checks stop the request |
| Approvals bound to execution | Decisions bind to specific parameters and an action directory hash, verified again before execution; each approval is used once |
| Traceable results | Persisted request state and audit records, with redacted execution output returned to the agent |
| Local or shared operation | Unix sockets for local use, or TLS and per-user tokens for a shared service |

The security boundary depends on trusted administrators and command scripts, and on the agent having neither administrator privileges nor direct access to credentials. cmdgate is not a sandbox for untrusted scripts. See the [security policy and threat model](SECURITY.md).

## Install

Install the latest stable release:

```bash
curl -fL --progress-bar https://raw.githubusercontent.com/zreo0/cmdgate/main/scripts/install.sh | sh
```

The [installer](scripts/install.sh) shows each stage and download progress. For a system installation, it requests sudo after downloading and verifying the release. It automatically:

- Detects macOS / Linux and arm64 / amd64, downloads the matching binary, and verifies SHA-256.
- Uses sudo to create the `_cmdgate` service user and system service, installs the binary at `/opt/cmdgate/bin/cmdgate`, and stores data under `/opt/cmdgate`.
- Registers the system PATH and preserves existing configuration and data. Open a new login terminal to use `cmdgate`.

Linux uses systemd by default. For shared servers and other deployment options, see the [deployment guide](docs/deployment.md).

> Administrator commands below use `sudo cmdgate`, which must resolve to the protected binary at `/opt/cmdgate/bin/cmdgate`. If sudo cannot find it or resolves to another program, use the full path, for example `sudo /opt/cmdgate/bin/cmdgate update`.

<details>
<summary>Install only the client</summary>

Connect to an existing remote server without sudo or a local service:

```bash
curl -fL --progress-bar https://raw.githubusercontent.com/zreo0/cmdgate/main/scripts/install.sh \
    | sh -s -- --client-dir "$HOME/.local/bin"
export PATH="$HOME/.local/bin:$PATH"
cmdgate login https://your-server.example:8443
```

Enter the token provided by your administrator. Client-only installation does not change the system PATH; add the `export` above to your shell configuration if needed.

</details>

<details>
<summary>Install a specific version or upgrade</summary>

For an existing system installation, update to the latest stable release or select a version:

```bash
sudo cmdgate update
sudo cmdgate update --version v0.1.0
```

Replace `v0.1.0` with the desired published version. The command downloads the matching official release, verifies SHA-256, atomically replaces the binary, and restarts the registered systemd or launchd service. Existing configuration, credentials, actions, and request history are retained; service definitions and PATH settings are left intact. With a `--no-service` installation, restart your manually managed server yourself. Run updates when no actions are executing, since a service restart interrupts them.

Download or validation failures leave the old binary in place. If restarting fails after replacement, the command reports an error and leaves the new binary installed; fix the service and restart it manually. This command updates the local system installation, not a remote server or a client-only binary.

For an initial installation, or an older version that does not yet have `update`, use the installer:

Replace `v0.1.0` with the [published version](https://github.com/zreo0/cmdgate/releases) you want:

```bash
curl -fL --progress-bar https://raw.githubusercontent.com/zreo0/cmdgate/main/scripts/install.sh \
    | sh -s -- --version v0.1.0
```

Omit `--version` to install the latest stable release. Existing configuration and data are retained. When upgrading an existing Linux service through the installer, run `sudo systemctl restart cmdgate` afterward. For a client-only installation, rerun the client installation command with the original `--client-dir`.

</details>

<details>
<summary>Uninstall</summary>

Choose one of these commands:

```bash
sudo cmdgate uninstall          # Remove the service and binary, keep data
sudo cmdgate uninstall --purge  # Also delete data, including credentials
```

Unmodified system PATH snippets are removed too. Customized content and personal shell configuration are preserved. For a client-only installation, delete the client binary.

</details>

## Configure

After installation, an administrator configures an approval channel, registers commands, and stores their credentials:

```bash
sudo cmdgate channel add telegram        # Set up a remote approval channel through the wizard
sudo cmdgate admin sync ./my-commands    # Register command directories; see “Write commands” below
sudo cmdgate secret set DEPLOY_TOKEN     # Store a required credential without echoing input
sudo cmdgate admin doctor                # Check permissions, commands, credentials and channels
```

> Start agents from a new terminal; see the [deployment guide](docs/deployment.md) for runners that do not load the system PATH.

Remove production credentials from agent-readable locations such as `.env` and `~/.aws`. Keep the approval account outside the agent's control, for example on a separate phone.

Once configured, you or the agent can request commands as a regular user:

```bash
cmdgate list
cmdgate describe deploy
cmdgate run deploy --env prod --version 1.2.3
cmdgate wait --timeout 100s <id>
```

When approval is required, `run` returns a request ID and `pending_approval`; the service executes automatically after approval. A `wait` timeout returns exit code `3`: keep waiting on the same ID instead of submitting again. Use `cmdgate status <id>` to check progress or `cmdgate cancel <id>` to cancel. Cancellation does not undo effects that have already occurred.

Give your agent the supplied [Agent Skill](examples/skill/SKILL.md) so it can discover commands through `list` and `describe`, then wait and report results according to the request state.

## Notification and approval channels

Channels deliver approval requests, receive decisions, and send result notifications. **Telegram is currently the only implemented channel**, with support for multiple bots identified by channel name.

| Operation | Command |
| --- | --- |
| Add the first bot | `sudo cmdgate channel add telegram` |
| Add a second bot | `sudo cmdgate channel add telegram --name ops` |
| List names, types and approvers | `sudo cmdgate channel list` |
| Remove a channel | `sudo cmdgate channel remove ops` |

The wizard first asks for a channel name, defaulting to `telegram`; `--name` supplies an editable default. Replacing an existing name requires explicit confirmation; pressing Enter cancels replacement. Use distinct names such as `telegram` and `ops`: names differing only by case or by `-`, `_`, and `.` can currently collide in credential storage.

Each request uses **one channel**. With multiple channels configured, set `approval.channel` in the command definition; there is no automatic broadcast or failover. Within that channel, the **first valid approval or rejection wins**. Later decisions cannot change the result or trigger another execution. Update commands that reference a channel before removing it.

## Write commands

A requestable command is called an *action* in the project and is defined by a directory:

```text
my-commands/
└── deploy/
    ├── action.yaml   Command definition, parameters and execution policy
    ├── preflight     Optional preflight script
    └── run           Execution script
```

Example `action.yaml`:

```yaml
# Command name, used by cmdgate run deploy
name: deploy
# Description shown to agents and approvers
description: Deploy a released version
# Accepted parameters; undeclared parameters are rejected
params:
    # Target environment
    env:
        # Enum type accepts only the listed values
        type: enum
        # Available environments
        values: [staging, prod]
    # Version to deploy
    version:
        # Text value
        type: string
        # Must match the whole value, for example 1.2.3
        pattern: '\d+\.\d+\.\d+'
# Approval policy
risk:
    # Require human approval by default
    default: approve
    # Evaluated in order; the first matching rule wins
    rules:
        # Match the staging environment
        - when: {env: staging}
          # Run automatically when matched, without human approval
          level: auto
# Credential names injected only into run
secrets: [DEPLOY_TOKEN]
# Human approval settings
approval:
    # Configured channel name; required when multiple channels exist
    channel: telegram
    # Approval validity, measured from request creation
    timeout: 30m
# Allow one execution of this command at a time; others queue
concurrency: 1
# Script execution settings
exec:
    # Measured from script start; terminate execution on timeout
    timeout: 5m
```

Both `run` and `preflight` must be executable. Parameters arrive through `CMDGATE_PARAM_<NAME>` environment variables; declared credentials are injected into `run` under their own names. Script output is redacted and returned to the agent after execution, so commands can pass results back.

Use `preflight` to check conditions and show a plan to the approver. It receives no injected credentials; keeping it read-only is the script author's responsibility. Scripts use a fixed system PATH; use trusted absolute paths for additional tools.

```bash
chmod +x my-commands/deploy/run
# If you provide a preflight script, make it executable too
sudo cmdgate admin sync ./my-commands
```

Re-run `admin sync` after changing a command. See the [command authoring guide](docs/actions.md) for the full parameter and policy reference, and [example commands](examples/actions) for working scripts. The supplied `deploy` example demonstrates the flow without performing a real deployment.

## Development

Use the Go version specified in [go.mod](go.mod) and Make. Docker is required for isolation acceptance tests.

```bash
make build           # Build ./cmdgate for the current platform
make check test      # Formatting, static checks, unit and CLI integration tests
go test -race ./...   # Check changes involving concurrency
make test-isolation  # Verify user isolation in a disposable Linux container
make test-scripts    # Offline release and installer regression tests
```

<details>
<summary>Development demo: run without installing a system service</summary>

Run from the repository root to explore commands or debug. This uses your current user and provides **no user isolation**. Do not use production credentials; choose the installation flow above for regular use.

```bash
make build
./cmdgate serve --dev
```

In another terminal, from the repository root:

```bash
export CMDGATE_DATA_DIR="$HOME/.cmdgate-dev"
./cmdgate admin sync examples/actions
./cmdgate run hello --name there
```

When finished, press Ctrl-C in the server terminal and run `unset CMDGATE_DATA_DIR` in the client terminal so later commands do not keep connecting to the demo environment.

</details>

See [Contributing](CONTRIBUTING.md) for development conventions and test responsibilities. Report security issues according to the [security policy](SECURITY.md).

## License

[MIT](LICENSE)

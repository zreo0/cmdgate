---
name: cmdgate
description: Run privileged operations (deploys, production data changes, anything that needs production credentials) through cmdgate. Use whenever a task needs credentials you do not have, or the user asks to release, deploy, migrate or change something in production.
---

# Privileged operations go through cmdgate

Production credentials are not available in this environment on purpose. Every privileged operation is a registered cmdgate action. Do not look for credentials, and do not try to reach the same result another way: if no action fits, say so and stop.

## Find the action

```bash
cmdgate list
cmdgate describe <action>
```

`describe` shows the parameters, their allowed values, and whether the request runs right away (`auto`) or waits for a human (`approve`).

## Request it

```bash
cmdgate run <action> --key value --other value
cmdgate run --file script=./change.sql <action>     # for file parameters
```

Use exactly the parameters the user asked for. Do not adjust a value to make a request pass validation or to avoid an approval.

Read the `state:` line of the output:

| state | meaning | what to do |
|---|---|---|
| `succeeded` | done, output follows | report the result |
| `failed` | the script ran and failed, or a check failed | report the reason and the output, do not retry blindly |
| `pending_approval` | waiting for a human | tell the user it is waiting for their approval, then wait |
| `running` / `approved` | still in progress | wait |
| `denied` / `expired` / `canceled` | not executed | report it, do not resubmit unless the user asks |
| `unknown` | interrupted mid-run, result not known | report it and let the user decide |

## Wait for a pending request

```bash
cmdgate wait --timeout 100s <id>
```

Exit code 3 means it has not finished yet: run the same command again. Approval can take minutes, so keep each wait under your command timeout and repeat.

```bash
cmdgate status <id>     # look without waiting
cmdgate cancel <id>     # withdraw a request that is no longer wanted
```

## Rules

- Never submit the same approval request repeatedly. One request, then wait.
- A denial is an answer. Do not rephrase and resubmit.
- If `cmdgate` reports that it cannot reach the server, tell the user. Do not work around it.
- `cmdgate admin`, `secret`, `channel`, `user` and `install` are for the human administrator. Do not run them.

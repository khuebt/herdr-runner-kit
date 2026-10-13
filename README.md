# herdr-runner-kit

Talk to one main agent and let runners do the work.

herdr-runner-kit keeps your main Claude Code chat for conversation and coordination. It runs every coding, research and report task as a separate Claude Code or Codex **runner** in its own [herdr](https://herdr.dev) tab, with or without a git worktree. Runners report back to the main agent within about a second. When one needs a decision, you can answer from the main chat or from Telegram.

> **Status:** 0.x. A minor release may change commands or file formats before 1.0; `upgrade` asks before installing one. See [versioning](docs/versioning.md).

## How it works

- **The main agent answers directly** for ordinary questions, explanations and quick lookups.
- **Everything else becomes a runner:** code changes of any size, research, analysis reports, and skill or multi-step flows. The main agent first replies with a short brief (goal, approach, checks, kind and branch) and starts the runner when you agree, so you can add what is missing.
- **Each runner is visible.** It is an interactive agent in a herdr tab, so you can watch it or type into it.
- **Results arrive as events.** A runner calls `herdr-runner-kit report`, and the main agent's background `wait` wakes at once. Tasks chained with `--after` start automatically when their parent is done.
- **Decisions never stall a runner.** A runner that needs input stays alive in its session, and your answer resumes it in the same context.

## Requirements

- [herdr](https://herdr.dev), with its server running (tested with 0.9.1)
- [Claude Code](https://claude.com/claude-code) for the main session and Claude runners
- [Codex](https://github.com/openai/codex) for Codex runners (optional)
- git, for worktree runners

## Install

On Linux or macOS (amd64 or arm64):

```sh
curl -fsSL https://raw.githubusercontent.com/khuebt/herdr-runner-kit/main/install.sh | sh
```

It verifies the download, installs to `~/.local/bin` and runs `setup`. `herdr-runner-kit upgrade` installs newer releases. See [installation](docs/installation.md) and [upgrading](docs/upgrading.md).

To build from source:

```sh
go install github.com/khuebt/herdr-runner-kit/cmd/herdr-runner-kit@latest
```

## Get started

```sh
herdr-runner-kit setup     # skill, auto re-arm hook, config and state
herdr-runner-kit doctor    # checks herdr, the agents and Telegram, and prints fixes
herdr-runner-kit main --exec
```

`main --exec` starts the main Claude Code session with the routing rules in its system prompt and, when configured, the Telegram channel. Run it inside herdr, so runners open as tabs next to it. See [setup](docs/setup.md).

Then talk to it as usual:

| You say | What happens |
|---|---|
| "What does `wait` do?" | the main agent answers |
| "Fix the typo in README" | a `fix` runner |
| "Compare fsnotify and polling and write it up" | a `research` runner |
| "Plan the auth feature, then build it on Codex after I approve" | a plan runner, then a code runner held for your approval |

## Kinds

Each runner has a kind, which picks its default agent and whether it gets its own git worktree.

| Kind | Default agent | Worktree | Use for |
|---|---|---|---|
| `code` | Codex | yes | features and refactors |
| `flow` | Claude Opus, high effort | yes | skill flows that change code, such as `--skill ak:cook` |
| `fix` | Claude Sonnet, medium effort | no | small fixes and edits |
| `research` | Claude Opus, high effort | no | investigation and analysis |
| `report` | Claude Opus, high effort | no | written reports and reviews |

Ask for another agent, model or effort in your own words, or change the defaults in the [configuration](docs/config.md). Finished runners close on their own 30 minutes after they finish. Their worktrees and branches stay until `cleanup` removes them; once a branch is merged, the main agent offers to do that, and waits for your yes.

## Telegram

With a bot configured, the kit sends runner news (questions, failures, held tasks, finished chains) to your Telegram chat, and your replies reach the runner that asked: `#t3 use option A`, or just the answer when only one task is waiting. Sending works from any Claude account. Replies need the main session to load the Telegram channel plugin, which some organization policies block. See [Telegram](docs/telegram.md).

## Documentation

- [Installation](docs/installation.md)
- [Upgrading](docs/upgrading.md)
- [Setup, doctor and uninstall](docs/setup.md)
- [Commands](docs/commands.md)
- [Report contract](docs/report-contract.md)
- [Configuration](docs/config.md)
- [Telegram](docs/telegram.md)
- [Versioning and releases](docs/versioning.md)
- [Contributing](CONTRIBUTING.md)

## License

[MIT](LICENSE)

# herdr-runner-kit

Talk to one main agent and let runners do the work.

herdr-runner-kit keeps your main Claude Code chat for conversation and coordination. It runs every coding, research and report task as a separate Claude Code or Codex **runner** in its own [herdr](https://herdr.dev) tab, with or without a git worktree. Runners report back to the main agent within about a second. When one needs a decision, you can answer from the main chat or from Telegram.

> **Status:** early development, not yet released. Commands and file formats may change before 1.0.

## How it works

- **The main agent answers directly** for ordinary questions, explanations and quick lookups.
- **Everything else becomes a runner:** code changes of any size, research, analysis reports, and skill or multi-step flows.
- **Each runner is visible.** It is an interactive agent in a herdr tab, so you can watch it or type into it.
- **Results arrive as events.** A runner calls `herdr-runner-kit report`, and the main agent's background `wait` wakes at once. Tasks chained with `--after` start automatically when their parent is done.
- **Decisions never stall a runner.** A runner that needs input stays alive in its session, and your answer resumes it in the same context.

## Install

Release binaries for Linux and macOS will be published on [GitHub Releases](https://github.com/khuebt/herdr-runner-kit/releases). A one-line installer and `herdr-runner-kit upgrade` arrive with the first release.

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

`main --exec` starts the main Claude Code session with the routing rules in its system prompt and, when configured, the Telegram channel. See [setup](docs/setup.md).

## Documentation

- [Setup, doctor and uninstall](docs/setup.md)
- [Commands](docs/commands.md)
- [Report contract](docs/report-contract.md)
- [Configuration](docs/config.md)
- [Telegram](docs/telegram.md)
- [Versioning and releases](docs/versioning.md)
- [Contributing](CONTRIBUTING.md)

## License

[MIT](LICENSE)

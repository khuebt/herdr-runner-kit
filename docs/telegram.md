# Telegram

herdr-runner-kit sends runner news to your Telegram chat. Your replies reach the main Claude Code session, which routes them to the runner that asked.

## How it works

- **Sending:** when a task reports `needs-human`, fails, stalls, is held, ends turns without reporting, finishes, or a chain stops before its end, a detached process calls the Bot API `sendMessage`. `report` and `wait` never wait for the network. A failed send is recorded as a `notify-failed` event.
- **Replying:** the main session runs the official Telegram channel plugin, which owns the bot's update polling. When your reply arrives there, the main agent passes it to `herdr-runner-kit answer --from-telegram`. The kit never polls Telegram itself, and runners have `TELEGRAM_BOT_TOKEN` cleared so they never take over the bot.

## Credentials

The token and chat ids are read when a message is sent. They are never copied into the kit's config or state, and they never appear in events or errors.

Either point the kit at a dotenv file that holds `TELEGRAM_BOT_TOKEN` and `TELEGRAM_CHAT_ID`:

```yaml
telegram:
  envFile: ~/path/to/.env.local
```

or let it use the channel plugin's own files: the token from `<dir>/.env` and the chats from `allowFrom` in `<dir>/access.json`, where `<dir>` is `telegram.stateDir`, else `$TELEGRAM_STATE_DIR`, else `${CLAUDE_CONFIG_DIR:-~/.claude}/channels/telegram`. `telegram.chatId` overrides the chat ids from either source. Without credentials, Telegram is simply off.

Use the same bot for the plugin and the kit.

## Setting up the plugin

Install the plugin in the Claude config folder of your main session, then disable it, so only the main session loads it:

```sh
CLAUDE_CONFIG_DIR=~/.claude2 claude plugin install telegram@claude-plugins-official
CLAUDE_CONFIG_DIR=~/.claude2 claude plugin disable telegram@claude-plugins-official
herdr-runner-kit setup --claude-config ~/.claude2
```

- The plugin's server needs [Bun](https://bun.sh).
- `setup` writes the plugin's `access.json` with your chat on an `allowlist`, so there is no pairing step. It never changes an existing access list.
- With a dotenv file (`telegram.envFile`), the plugin does not need its own `.env`. `main --exec` passes the token to the main session only.
- `main` enables the plugin for its own session with `--settings`. Every other Claude session leaves it off, so none of them takes over the bot's update polling. If the plugin has its own `.env` and is enabled everywhere, any session that starts it takes over the polling, and `doctor` warns about it.
- Channels can be turned off by an organization's managed settings. Claude then prints `--channels blocked by org policy` at start, and Telegram replies are dropped. Use a config folder whose account allows channels.

## Messages

Every message starts with the task's tag:

```
#t3 needs a decision · fix login
Drop the v1 endpoint? (recommended: yes)
Reply: #t3 <answer>
```

A round of questions arrives as `#t4 needs 2 decisions`, one numbered line per question and `Reply: #t4 1<option> 2<option>, such as 1a 2b`. A `done` lists the runner's own decisions as `Decided:` lines above the summary.

`telegram.notify` selects the events (default: all of `needs-human`, `failed`, `stalled`, `held`, `turn-ended-unreported`, `usage-limit` and `done`). A `stalled` message quotes the prompt the runner is blocked on.

A `done` is held back only when all of these hold, so a chain sends one message at its end:

- it ends the task's first round: no `done`, `send` or `answer` came before it, so a `done` after `send` or `answer` is always sent;
- it settles no decisions; a `done` with `Decided:` lines is always sent;
- a task that waits on it is still `queued` or `held`; once its dependents run, it is sent.

A held-back `done` is recorded as a `notify-deferred` event. When a task later in the chain fails or is stopped, and so never reports the chain's end, the held-back dones before it go out in one message: `#t50 stopped; the chain before it finished: #t45 <title>: <summary>; #t46 …`, each summary cut to 200 characters. This needs `done` in `telegram.notify`.

## Replying

- `#t3 <text>` goes to t3: as the answer to its question, as a new message if it is running, or as the go-ahead for a held task.
- Without a tag, the text goes to the only task waiting for you. With several waiting, the main agent asks which one you mean.
- For a held task, `ok`, `yes` or `duyệt` starts it; any other text starts it with that text as a note. A task held with `spawn --reason`, such as one waiting for a dependency, starts only on a reply tagged with its id.
- `approve spec <name>` or `approve pack <name>` never releases or answers a runner. The main agent confirms with you and records the approval with `project approve`.
- `status?`, `list` and `?` are for the main agent, which answers them itself.
- `status <name>` for a registered project asks the main agent for `project status <name>`. While a task is waiting for you, the text could also be its answer, so the main agent asks which you meant; `#t3 status <name>` sends it to t3.

## Starting the main session

```sh
herdr-runner-kit main          # prints the command
herdr-runner-kit main --exec   # starts it, with the bot token from the kit's config
CLAUDE_CONFIG_DIR=~/.claude2 herdr-runner-kit main --exec   # with the plugin in another config folder
```

The skill and the plugin come from `$CLAUDE_CONFIG_DIR`, or `~/.claude`. `main` warns when that folder has no skill. The command is `HRK_MAIN=<pid> claude --settings '{"enabledPlugins":{"telegram@claude-plugins-official":true}}' --channels plugin:telegram@claude-plugins-official --append-system-prompt-file <claude config>/skills/herdr-runner-kit/SKILL.md`. The skill in the system prompt keeps the routing rules in force for the whole session, including after compaction.

Run one such session per bot. It answers tasks from every repository. A reply sent while no main session is running is lost; send it again once the session is up.

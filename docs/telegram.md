# Telegram

herdr-runner-kit sends runner news to your Telegram chat. Your replies reach the main Claude Code session, which routes them to the runner that asked.

## How it works

- **Sending:** when a task reports `needs-human`, fails, stalls, is held, ends turns without reporting, or finishes a chain, a detached process calls the Bot API `sendMessage`. `report` and `wait` never wait for the network. A failed send is recorded as a `notify-failed` event.
- **Replying:** the main session runs the official Telegram channel plugin, which owns the bot's update polling. When your reply arrives there, the main agent passes it to `herdr-runner-kit answer --from-telegram`. The kit never polls Telegram itself, and runners have `TELEGRAM_BOT_TOKEN` cleared so they never take over the bot.

## Credentials

The token and chat ids are read when a message is sent. They are never copied into the kit's config or state, and they never appear in events or errors.

Either point the kit at a dotenv file that holds `TELEGRAM_BOT_TOKEN` and `TELEGRAM_CHAT_ID`:

```yaml
telegram:
  envFile: ~/path/to/.env.local
```

or let it use the channel plugin's own files: the token from `<dir>/.env` and the chats from `allowFrom` in `<dir>/access.json`, where `<dir>` is `telegram.stateDir`, else `$TELEGRAM_STATE_DIR`, else `${CLAUDE_CONFIG_DIR:-~/.claude}/channels/telegram`. `telegram.chatId` overrides the chat ids from either source. Without credentials, Telegram is simply off.

Use the same bot for the plugin and the kit. Set up the plugin once in Claude Code (`/plugin install telegram@claude-plugins-official`, then follow its pairing steps), and switch its access policy to `allowlist` after pairing, so only your chat can reach the main session.

## Messages

Every message starts with the task's tag:

```
#t3 needs a decision · fix login
Drop the v1 endpoint? (recommended: yes)
Reply: #t3 <answer>
```

`telegram.notify` selects the events (default: all of `needs-human`, `failed`, `stalled`, `held`, `turn-ended-unreported` and `done`). A `done` is sent only for a task that no other task waits for, so a chain sends one message at its end.

## Replying

- `#t3 <text>` goes to t3: as the answer to its question, as a new message if it is running, or as the go-ahead for a held task.
- Without a tag, the text goes to the only task waiting for you. With several waiting, the main agent asks which one you mean.
- For a held task, `ok`, `yes` or `duyệt` starts it; any other text starts it with that text as a note.
- `status?`, `list` and `?` are for the main agent, which answers them itself.

## Starting the main session

```sh
herdr-runner-kit main          # prints: claude --channels plugin:telegram@claude-plugins-official
herdr-runner-kit main --exec   # starts it
```

Run one such session per bot. It answers tasks from every repository. A reply sent while no main session is running is lost; send it again once the session is up.

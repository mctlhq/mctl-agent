# Telegram Bot Setup

mctl-agent can send incident notifications to a Telegram group. This guide walks through creating a bot and connecting it.

## 1. Create a Bot

1. Open Telegram and message [@BotFather](https://t.me/BotFather)
2. Send `/newbot`
3. Choose a display name (e.g., "mctl-agent")
4. Choose a username (e.g., "mctl_agent_bot") -- must end with `bot`
5. BotFather replies with your **bot token** (format: `123456:ABC-DEF...`)
6. Save the token -- this is your `TELEGRAM_BOT_TOKEN`

## 2. Get the Chat ID

**For a group chat:**

1. Create a Telegram group (or use an existing one)
2. Add your bot to the group
3. Send any message in the group
4. Open this URL in a browser (replace `<TOKEN>` with your bot token):
   ```
   https://api.telegram.org/bot<TOKEN>/getUpdates
   ```
5. Find the `"chat":{"id": ...}` value -- group IDs are negative numbers (e.g., `-1001234567890`)
6. This is your `TELEGRAM_CHAT_ID`

**For a direct message (DM):**

1. Send any message to your bot
2. Use the same `getUpdates` URL
3. The chat ID is a positive number (your user ID)

## 3. Configure mctl-agent

Set these environment variables:

```bash
TELEGRAM_BOT_TOKEN=123456:ABC-DEF1234ghIkl-zyx57W2v1u123ew11
TELEGRAM_CHAT_ID=-1001234567890
```

Or in Kubernetes (ExternalSecret from Vault, or plain Secret):

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: mctl-agent-telegram
  namespace: admins
type: Opaque
stringData:
  bot-token: "123456:ABC-DEF1234ghIkl-zyx57W2v1u123ew11"
  chat-id: "-1001234567890"
```

## 4. What Gets Sent

The bot sends notifications for:

| Event | Message |
|-------|---------|
| New incident | Alert type, service, severity, summary |
| Diagnosis complete | Confidence level, analysis, recommended action |
| Fix PR created | PR link with `/approve` and `/reject` commands |
| Status report | Summary of all open tickets |

Severity indicators: critical, warning, info.

## 5. Bot Commands

If the Telegram webhook is configured (`POST /api/v1/telegram`), the bot responds to:

| Command | Action |
|---------|--------|
| `/approve <ticket-id>` | Merge the fix PR |
| `/reject <ticket-id> <reason>` | Close the PR with a comment |
| `/status` | List all open tickets |

To enable commands, set up a Telegram webhook pointing to your agent:

```bash
curl -X POST "https://api.telegram.org/bot<TOKEN>/setWebhook" \
  -H "Content-Type: application/json" \
  -d '{"url": "https://agent.your-domain.com/api/v1/telegram"}'
```

## 6. Verify

Test the connection:

```bash
curl -X POST "https://api.telegram.org/bot<TOKEN>/sendMessage" \
  -H "Content-Type: application/json" \
  -d '{"chat_id": "<CHAT_ID>", "text": "mctl-agent connected"}'
```

If the message appears in your chat, the configuration is correct.

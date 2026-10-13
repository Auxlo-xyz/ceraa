# Mini App features

The Telegram Mini App provides a visual view of Ceraa without replacing the
Telegram controls.

## Main uses

- Browse sessions and open a transcript.
- Start a prompt in the selected session.
- Watch content, thinking, tool progress, permissions, and questions.
- Cancel a running request.
- Answer permission and question cards.
- Review settings, provider state, usage, and cost data.
- Manage user secrets through the settings surface.

The page is embedded in the Ceraa binary. A released binary does not require a
page beside it to serve the Mini App. Changes to the page require a rebuild and
restart.

If the page is stale after a source change, verify that the running binary was
rebuilt and restarted. Do not assume a disk copy beside the executable is the
served page.

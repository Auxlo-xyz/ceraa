# Setting secrets

User integration secrets are stored encrypted and are separate from normal
configuration and provider credentials.

## Telegram commands

- `/secrets`: list the names of user secrets.
- `/setsecret NAME VALUE`: store or replace one user secret.
- `/delsecret NAME`: delete one user secret.

Use a private chat when entering a secret. Never put a secret in a task,
committed file, log, screenshot, or tool result.

The `list_secrets` tool returns names only. The
`run_command_with_secret` tool can inject a named user secret into a command's
environment for one execution. Ceraa does not list its own provider
credentials through that tool, and those credentials cannot be passed through
it.

If a command needs a secret, use the stored name and the required environment
variable name. Do not echo the variable or print the environment.

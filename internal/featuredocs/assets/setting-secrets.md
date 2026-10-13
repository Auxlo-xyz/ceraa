# Setting secrets

Ceraa user secrets are managed in the Telegram Mini App. Do not ask the user to
paste a secret into a Telegram task message or a shell command.

## Add a secret

1. Open the Ceraa Mini App from Telegram.
2. Open the **Agents, Providers & Secrets** tab.
3. Find the **AES-256 Secret Store** panel.
4. Choose **+ Add secret**.
5. Enter the secret name and value.
6. Save it from the Mini App.

Secret values are entered into a password field and are not echoed. The Mini App
shows names and byte lengths, not secret values.

## Manage existing secrets

The same panel lets the user see stored secret names and delete a secret. A
secret deletion is permanent unless the user adds it again.

The Mini App uses the authenticated secret endpoints. It is the user-facing
feature for managing user secrets. The Telegram `/secrets`, `/setsecret`, and
`/delsecret` commands are not the navigation guidance for this feature.

## Agent use

The model may use `list_secrets` to see allowed user-secret names. It never
receives secret values from that listing. If a command needs one, use
`run_command_with_secret` with the stored name and an environment-variable name.
Never echo the variable, print the environment, put the value in a file, or
include it in a result or log.

Ceraa's own provider credentials are separate and are not exposed through the
user-secret listing or command-injection path.

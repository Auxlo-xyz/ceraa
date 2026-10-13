# Authentication

Ceraa supports provider selection from Telegram and can store provider
credentials in its encrypted secret store.

## Normal flow

1. Start Ceraa and send `/start` in Telegram.
2. Choose a provider or saved model profile.
3. Complete the provider's sign-in flow when prompted.
4. Return to the provider menu and select the active model.
5. Send a small test request before starting a larger task.

Codex authentication uses `/codex-login` and reports state with
`/codex-status`. Device authorization must be enabled for the account before
starting that flow. OpenCode Zen and other providers may have their own sign-in
requirements.

Provider credentials are not exposed to the model. Use the provider menu or
provider-specific commands rather than placing credentials in a prompt.

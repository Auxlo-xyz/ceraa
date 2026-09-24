#!/bin/bash
# Start Sooraa agent as a daemon
# Sooraa secrets come from an untracked env file (see sooraa.env.example).
export HOME=/home/assistant
if [ -f /etc/sooraa/sooraa.env ]; then
    . /etc/sooraa/sooraa.env
elif [ -f "$HOME/.config/sooraa/sooraa.env" ]; then
    . "$HOME/.config/sooraa/sooraa.env"
else
    echo "Missing /etc/sooraa/sooraa.env (or $HOME/.config/sooraa/sooraa.env)." >&2
    echo "Copy sooraa.env.example there and fill in the values." >&2
    exit 1
fi
: "${SOORAA_API_URL:?set SOORAA_API_URL in sooraa.env}"
: "${SOORAA_MODEL:?set SOORAA_MODEL in sooraa.env}"
: "${SOORAA_API_KEY:?set SOORAA_API_KEY in sooraa.env}"
: "${TELEGRAM_BOT_TOKEN:?set TELEGRAM_BOT_TOKEN in sooraa.env}"
: "${TELEGRAM_ALLOWED_USER_ID:?set TELEGRAM_ALLOWED_USER_ID in sooraa.env}"
export SOORAA_API_URL SOORAA_MODEL SOORAA_API_KEY TELEGRAM_BOT_TOKEN TELEGRAM_ALLOWED_USER_ID
cd /workspace/sooraa
exec /workspace/sooraa/bin/sooraa telegram

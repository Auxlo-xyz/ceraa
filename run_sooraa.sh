#!/bin/bash
# Sooraa agent launch script

export HOME=/home/assistant
export SOORAA_API_URL="https://api.atria-asi.ai/v1"
export SOORAA_MODEL="atria-dawn-preview"
export TELEGRAM_BOT_TOKEN="8854210450:AAE8W5YbTrb1lHC36CAtzzOIh8BCE3Gyq6E"
export TELEGRAM_ALLOWED_USER_ID="7598252261"

mkdir -p ~/.config/sooraa
cd /workspace/sora-agent-local
exec ./bin/sooraa telegram

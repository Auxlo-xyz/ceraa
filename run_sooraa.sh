#!/bin/bash
# Sooraa agent launch script

export HOME=/home/assistant
export SOORAA_API_URL="https://inference.poolside.ai/v1"
export SOORAA_MODEL="poolside/laguna-xs-2.1"
export SOORAA_API_KEY="sky_qUyPQd2E.NvH8HaZOI5xj1IfuksOfWd2oAweHTRpp"
export TELEGRAM_BOT_TOKEN="8854210450:AAE8W5YbTrb1lHC36CAtzzOIh8BCE3Gyq6E"
export TELEGRAM_ALLOWED_USER_ID="7598252261"

mkdir -p ~/.config/sooraa
cd /workspace/sooraa
exec ./bin/sooraa telegram

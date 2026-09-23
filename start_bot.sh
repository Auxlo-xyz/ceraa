#!/bin/bash
export HOME=/home/assistant
export SOORAA_API_URL="https://api.poolside.ai/v1"
export SOORAA_MODEL="poolside/laguna-xs-2.1"
export SOORAA_API_KEY="sky_qUyPQd2E.NvH8HaZOI5xj1IfuksOfWd2oAweHTRpp"
mkdir -p ~/.config/sooraa
cd /workspace/sooraa
exec ./bin/sooraa telegram

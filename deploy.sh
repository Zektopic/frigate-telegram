#!/usr/bin/env bash
set -e

# =============================================================================
#  frigate-telegram — One-Time Interactive Deployment Script
# =============================================================================
#  This script will:
#    1. Walk you through creating a Telegram bot (if you don't have one)
#    2. Help you find your Telegram chat ID
#    3. Collect all required configuration
#    4. Generate a secure .env file
#    5. Build and start the Docker containers
# =============================================================================

BOLD="\033[1m"
GREEN="\033[32m"
YELLOW="\033[33m"
CYAN="\033[36m"
RED="\033[31m"
RESET="\033[0m"

divider() { echo -e "${CYAN}======================================================================${RESET}"; }
header()  { echo -e "\n${BOLD}${GREEN}$1${RESET}\n"; }
info()    { echo -e "${CYAN}ℹ${RESET}  $1"; }
prompt()  { echo -e "${YELLOW}➤${RESET} $1"; }
success() { echo -e "${GREEN}✅${RESET} $1"; }
warn()    { echo -e "${RED}⚠${RESET}  $1"; }

divider
echo -e "${BOLD}${GREEN}       frigate-telegram — Interactive Deployment${RESET}"
echo ""
echo -e "  Sends Frigate NVR camera events to Telegram with thumbnails,"
echo -e "  clips, and previews. Configure once, run forever."
divider

# ---- STEP 1: Telegram Bot Token -------------------------------------------
header "Step 1 of 6: Telegram Bot Token"

echo -e "You need a Telegram bot to send messages. Here's how to create one:"
echo ""
echo -e "  ${BOLD}1.${RESET} Open Telegram and search for ${CYAN}@BotFather${RESET}"
echo -e "  ${BOLD}2.${RESET} Send the command: ${CYAN}/newbot${RESET}"
echo -e "  ${BOLD}3.${RESET} Choose a name (e.g., \"Frigate Events\")"
echo -e "  ${BOLD}4.${RESET} Choose a username ending in \"bot\" (e.g., myhouse_frigate_bot)"
echo -e "  ${BOLD}5.${RESET} BotFather will give you a token like:"
echo -e "     ${CYAN}1234567890:ABCdefGHIjklMNOpqrsTUVwxyz${RESET}"
echo ""
echo -e "  ${YELLOW}Already have a bot? Just paste the token below.${RESET}"
echo ""

while true; do
    prompt "Paste your bot token (from @BotFather):"
    read -r TELEGRAM_BOT_TOKEN
    if [ -n "$TELEGRAM_BOT_TOKEN" ]; then
        break
    fi
    warn "Token cannot be empty. Please paste your bot token."
done
success "Bot token saved."

# ---- STEP 2: Telegram Chat ID ----------------------------------------------
header "Step 2 of 6: Telegram Chat ID"

echo -e "Events will be sent to a Telegram chat. You need the chat's ID."
echo ""
echo -e "  ${BOLD}Option A — Personal chat (just you):${RESET}"
echo -e "    1. Send any message to your bot on Telegram"
echo -e "    2. Visit: ${CYAN}https://api.telegram.org/bot<YOUR_TOKEN>/getUpdates${RESET}"
echo -e "    3. Look for ${CYAN}\"chat\":{\"id\":123456789}${RESET}"
echo -e "    4. Copy that number"
echo ""
echo -e "  ${BOLD}Option B — Group chat:${RESET}"
echo -e "    1. Add your bot to the group"
echo -e "    2. Send a message in the group"
echo -e "    3. Visit the getUpdates URL above"
echo -e "    4. Group IDs are negative (e.g., -1003462069191)"
echo ""
echo -e "  ${BOLD}Option C — Use @RawDataBot:${RESET}"
echo -e "    1. Add ${CYAN}@RawDataBot${RESET} to your chat"
echo -e "    2. It will print the chat ID immediately"
echo ""

while true; do
    prompt "Enter your Telegram Chat ID (negative for groups):"
    read -r TELEGRAM_CHAT_ID
    if [ -n "$TELEGRAM_CHAT_ID" ]; then
        break
    fi
    warn "Chat ID cannot be empty."
done
success "Chat ID saved."

# ---- STEP 3: Frigate Connection --------------------------------------------
header "Step 3 of 6: Frigate Connection"

echo -e "Where is your Frigate NVR running?"
echo ""
echo -e "  ${BOLD}Same machine (Docker host networking):${RESET}"
echo -e "    Use: ${CYAN}http://localhost:5000${RESET}"
echo ""
echo -e "  ${BOLD}Different machine on your network:${RESET}"
echo -e "    Use: ${CYAN}http://192.168.X.X:5000${RESET}"
echo ""

prompt "Internal Frigate URL [http://localhost:5000]:"
read -r FRIGATE_URL
FRIGATE_URL="${FRIGATE_URL:-http://localhost:5000}"

echo ""
echo -e "External URL is used for links in Telegram messages."
echo -e "This should be reachable from your phone (e.g., Tailscale IP, public DNS)."
echo ""

prompt "External Frigate URL [http://localhost:5000]:"
read -r FRIGATE_EXTERNAL_URL
FRIGATE_EXTERNAL_URL="${FRIGATE_EXTERNAL_URL:-http://localhost:5000}"

success "Frigate URLs saved."

# ---- STEP 4: Redis Password ------------------------------------------------
header "Step 4 of 6: Redis Password"

echo -e "Redis is used to track which events have already been sent."
echo -e "It runs locally in Docker. Set a password for security."
echo ""

# Generate a random password
GENERATED_PASSWORD=$(openssl rand -hex 16 2>/dev/null || cat /dev/urandom | tr -dc 'a-zA-Z0-9' | fold -w 32 | head -1)

prompt "Redis password [auto-generated]:"
read -r REDIS_PASSWORD
REDIS_PASSWORD="${REDIS_PASSWORD:-$GENERATED_PASSWORD}"
echo -e "  Redis password: ${CYAN}$REDIS_PASSWORD${RESET}"
success "Redis password set."

# ---- STEP 5: REST API ------------------------------------------------------
header "Step 5 of 6: REST API Configuration"

echo -e "frigate-telegram has an optional REST API for remote control"
echo -e "(pause/resume/mute notifications, check status via HTTP)."
echo ""

prompt "Enable REST API? (y/n) [y]:"
read -r ENABLE_REST
ENABLE_REST="${ENABLE_REST:-y}"

if [[ "$ENABLE_REST" =~ ^[Yy] ]]; then
    REST_API_ENABLE="true"
    GENERATED_API_KEY=$(openssl rand -hex 24 2>/dev/null || cat /dev/urandom | tr -dc 'a-zA-Z0-9' | fold -w 48 | head -1)

    prompt "REST API listen port [3232]:"
    read -r REST_PORT
    REST_PORT="${REST_PORT:-3232}"

    prompt "API Key [auto-generated]:"
    read -r REST_API_KEY
    REST_API_KEY="${REST_API_KEY:-$GENERATED_API_KEY}"

    echo -e "  API Key: ${CYAN}$REST_API_KEY${RESET}"
    echo -e "  Use with header: ${CYAN}X-API-Key: $REST_API_KEY${RESET}"
    echo -e "  Status URL:  ${CYAN}http://YOUR_IP:$REST_PORT/api/v1/status${RESET}"
    echo -e "  Swagger UI:  ${CYAN}http://YOUR_IP:$REST_PORT/docs/index.html${RESET}"

    # Get the host's primary IP for Swagger host
    HOST_IP=$(ip route get 1 2>/dev/null | awk '{print $7; exit}' || echo "localhost")
    SWAGGER_HOST="${HOST_IP}:${REST_PORT}"
else
    REST_API_ENABLE="false"
    REST_API_KEY=""
    REST_PORT="3232"
    SWAGGER_HOST="localhost:3232"
fi
success "REST API configured."

# ---- STEP 6: Optional Settings ---------------------------------------------
header "Step 6 of 6: Optional Settings"

prompt "Timezone [UTC]:"
read -r TZ
TZ="${TZ:-UTC}"

prompt "Poll interval in seconds [30]:"
read -r SLEEP_TIME
SLEEP_TIME="${SLEEP_TIME:-30}"

prompt "Wait time for clip to be ready, in seconds [30]:"
read -r TIME_WAIT_SAVE
TIME_WAIT_SAVE="${TIME_WAIT_SAVE:-30}"

prompt "Include thumbnail images? (y/n) [y]:"
read -r INCLUDE_THUMB
INCLUDE_THUMB="${INCLUDE_THUMB:-y}"
if [[ "$INCLUDE_THUMB" =~ ^[Yy] ]]; then
    INCLUDE_THUMBNAIL_EVENT="true"
else
    INCLUDE_THUMBNAIL_EVENT="false"
fi

prompt "Include video clips? (y/n) [y]:"
read -r INCLUDE_CLIP
INCLUDE_CLIP="${INCLUDE_CLIP:-y}"
if [[ "$INCLUDE_CLIP" =~ ^[Yy] ]]; then
    INCLUDE_CLIP_EVENT="true"
else
    INCLUDE_CLIP_EVENT="false"
fi

prompt "Include preview video snippets? (y/n) [y]:"
read -r INCLUDE_PREV
INCLUDE_PREV="${INCLUDE_PREV:-y}"
if [[ "$INCLUDE_PREV" =~ ^[Yy] ]]; then
    INCLUDE_PREVIEW_EVENT="true"
else
    INCLUDE_PREVIEW_EVENT="false"
fi

success "Settings saved."

# ---- WRITE .ENV FILE -------------------------------------------------------
divider
header "Writing .env file..."

cat > .env <<EOF
# frigate-telegram configuration
# Generated: $(date)
TELEGRAM_BOT_TOKEN=$TELEGRAM_BOT_TOKEN
TELEGRAM_CHAT_ID=$TELEGRAM_CHAT_ID
FRIGATE_URL=$FRIGATE_URL
FRIGATE_EXTERNAL_URL=$FRIGATE_EXTERNAL_URL
REDIS_PASSWORD=$REDIS_PASSWORD
REST_API_ENABLE=$REST_API_ENABLE
REST_API_KEY=$REST_API_KEY
REST_API_LISTEN_ADDR=:$REST_PORT
SWAGGER_HOST=$SWAGGER_HOST
TZ=$TZ
SLEEP_TIME=$SLEEP_TIME
TIME_WAIT_SAVE=$TIME_WAIT_SAVE
EVENT_BEFORE_SECONDS=300
INCLUDE_THUMBNAIL_EVENT=$INCLUDE_THUMBNAIL_EVENT
INCLUDE_CLIP_EVENT=$INCLUDE_CLIP_EVENT
INCLUDE_PREVIEW_EVENT=$INCLUDE_PREVIEW_EVENT
DEBUG=false
EOF

chmod 600 .env
success ".env file written (permissions: 600, owner-only)."

# ---- BUILD & START ----------------------------------------------------------
divider
header "Ready to deploy!"

echo -e "Configuration summary:"
echo -e "  Bot token:     ${CYAN}${TELEGRAM_BOT_TOKEN:0:12}...${RESET}"
echo -e "  Chat ID:       ${CYAN}$TELEGRAM_CHAT_ID${RESET}"
echo -e "  Frigate URL:   ${CYAN}$FRIGATE_URL${RESET}"
echo -e "  External URL:  ${CYAN}$FRIGATE_EXTERNAL_URL${RESET}"
echo -e "  REST API:      ${CYAN}$REST_API_ENABLE${RESET}"
echo -e "  Poll interval: ${CYAN}${SLEEP_TIME}s${RESET}"
echo ""

prompt "Build and start the Docker containers now? (y/n) [y]:"
read -r START_NOW
START_NOW="${START_NOW:-y}"

if [[ "$START_NOW" =~ ^[Yy] ]]; then
    echo ""
    info "Building Docker image (this may take a minute)..."
    docker compose build

    echo ""
    info "Starting containers..."
    docker compose up -d

    echo ""
    sleep 3

    if docker ps --format '{{.Names}}' | grep -q frigate-telegram; then
        success "Deployment successful!"
        echo ""
        echo -e "  Check logs:   ${CYAN}docker compose logs -f${RESET}"
        echo -e "  Check status: ${CYAN}docker ps | grep frigate-telegram${RESET}"
        if [ "$REST_API_ENABLE" = "true" ]; then
            echo -e "  REST API:     ${CYAN}curl -H 'X-API-Key: $REST_API_KEY' http://localhost:$REST_PORT/api/v1/status${RESET}"
        fi
        echo ""
        echo -e "  ${GREEN}Your bot should send a startup message to Telegram shortly.${RESET}"
    else
        warn "Something went wrong. Check logs: docker compose logs"
    fi
else
    echo ""
    info "To start later, run:"
    echo -e "  ${CYAN}docker compose up -d${RESET}"
fi

divider
echo -e "${GREEN}${BOLD}  Setup complete!${RESET}"
echo ""
echo -e "  To reconfigure:   re-run this script"
echo -e "  To stop:          docker compose down"
echo -e "  To update:        git pull && docker compose up -d --build"
echo -e "  Telegram commands: /stop /resume /mute /unmute /status /help"
divider

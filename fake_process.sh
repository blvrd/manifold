#!/bin/bash

# Define colors
COLORS=(
    "\033[0;31m"    # Red
    "\033[0;32m"    # Green
    "\033[0;33m"    # Yellow
    "\033[0;34m"    # Blue
    "\033[0;35m"    # Purple
    "\033[0;36m"    # Cyan
)
RESET="\033[0m"     # Reset color

trap 'echo "Received SIGTERM, shutting down..."; exit 0' SIGTERM
trap 'echo "Received SIGINT, shutting down..."; exit 0' SIGINT

# Array of random messages
messages=(
    "System running smoothly"
    "Processing data"
    "Network connection stable"
    "Memory usage normal"
    "Cache updated"
    "Background tasks running"
    "Service health check passed"
    "Queue processing"
    "Monitoring active"
    "Resources optimized"
)

# Infinite loop
while true; do
    # Get random message and color
    random_msg_index=$((RANDOM % ${#messages[@]}))
    random_color_index=$((RANDOM % ${#COLORS[@]}))

    message="${messages[$random_msg_index]}"
    color="${COLORS[$random_color_index]}"

    # Print timestamp and colored message
    echo -e "${color}$(date '+%Y-%m-%d %H:%M:%S') - $message${RESET}"

    # Sleep for 1 second
    sleep 0.5
done


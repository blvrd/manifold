#!/bin/bash

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
    # Get random message from array
    random_index=$((RANDOM % ${#messages[@]}))
    message="${messages[$random_index]}"

    # Print timestamp and message
    echo "$(date '+%Y-%m-%d %H:%M:%S') - $message"

    # Sleep for 1 second
    sleep 0.1
done

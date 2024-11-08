#!/bin/bash

# Function to handle cleanup
cleanup() {
    echo "Caught signal - cleaning up..."
    exit 0
}

# Trap SIGINT and SIGTERM
trap cleanup SIGINT SIGTERM

echo "Starting script..."
echo "This script will exit with an error in 5 seconds"

# Sleep for 5 seconds
sleep 5

echo "Time's up! Exiting with error..."
exit 1

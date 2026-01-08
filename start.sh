#!/bin/bash
# PR Review Server Startup Script
# Automatically loads environment variables from .env

# Load environment variables from .env file
if [ -f .env ]; then
    export $(cat .env | grep -v '^#' | xargs)
fi

# Start the server
./pr-review-server

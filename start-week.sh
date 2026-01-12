#!/bin/bash
set -e

echo "🚀 Starting PR Review Server for the week..."
echo ""

# Check if Docker is running
if ! docker info > /dev/null 2>&1; then
    echo "❌ Docker is not running!"
    echo ""
    echo "Please start Docker Desktop and try again."
    echo "Or set Docker Desktop to start automatically:"
    echo "  Docker Desktop > Settings > General > Start Docker Desktop when you log in"
    exit 1
fi

# Check if .env exists
if [ ! -f .env ]; then
    echo "❌ .env file not found!"
    echo ""
    echo "Please create .env from .env.docker:"
    echo "  cp .env.docker .env"
    echo "  # Then edit .env with your GitHub token"
    exit 1
fi

# Start the server
docker-compose up -d

echo ""
echo "✅ Server started and will run in the background!"
echo ""
echo "📊 Dashboard: http://localhost:7769"
echo ""
echo "Useful commands:"
echo "  make logs    - View live logs"
echo "  make status  - Check server status"
echo "  make stop    - Stop server (Friday afternoon)"
echo ""
echo "The server will keep running even if you:"
echo "  - Close your terminal"
echo "  - Restart Docker Desktop"
echo "  - Reboot your laptop (if Docker auto-starts)"
echo ""
echo "Happy reviewing! 🎉"

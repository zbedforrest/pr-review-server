.PHONY: help build start stop restart logs status clean rebuild

help:
	@echo "PR Review Server - Docker Management"
	@echo ""
	@echo "Monday morning workflow:"
	@echo "  make start          - Start the server (runs all week)"
	@echo ""
	@echo "Daily operations:"
	@echo "  make logs           - View live logs"
	@echo "  make status         - Check if server is running"
	@echo ""
	@echo "Maintenance:"
	@echo "  make restart        - Restart the server"
	@echo "  make stop           - Stop the server (Friday afternoon)"
	@echo "  make rebuild        - Rebuild and restart (after code changes)"
	@echo ""
	@echo "Troubleshooting:"
	@echo "  make clean          - Stop and remove everything"

build:
	@echo "Building Docker image..."
	docker-compose build

start:
	@echo "Starting PR Review Server..."
	@echo "This will run in the background until you run 'make stop'"
	docker-compose up -d
	@echo ""
	@echo "✅ Server started!"
	@echo "📊 Dashboard: http://localhost:7769"
	@echo "📝 View logs: make logs"

stop:
	@echo "Stopping PR Review Server..."
	docker-compose down
	@echo "✅ Server stopped"

restart:
	@echo "Restarting server..."
	docker-compose restart
	@echo "✅ Server restarted"

logs:
	@echo "Showing live logs (Ctrl+C to exit)..."
	docker-compose logs -f --tail=100

status:
	@docker-compose ps
	@echo ""
	@if docker-compose ps | grep -q "Up"; then \
		echo "✅ Server is running"; \
		echo "📊 Dashboard: http://localhost:7769"; \
	else \
		echo "❌ Server is not running"; \
		echo "💡 Run 'make start' to start it"; \
	fi

clean:
	@echo "⚠️  This will stop the server and remove containers"
	@read -p "Continue? (y/N) " -n 1 -r; \
	echo; \
	if [[ $$REPLY =~ ^[Yy]$$ ]]; then \
		docker-compose down -v; \
		echo "✅ Cleaned up"; \
	fi

rebuild:
	@echo "Rebuilding and restarting..."
	docker-compose down
	docker-compose build --no-cache
	docker-compose up -d
	@echo "✅ Rebuild complete"
	@echo "📝 View logs: make logs"

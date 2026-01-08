# Docker Setup Guide

## One-Time Setup (Do This Once)

### 1. Install Docker Desktop
Download from: https://www.docker.com/products/docker-desktop

### 2. Configure Docker to Auto-Start
Open Docker Desktop:
- Go to Settings → General
- ✅ Check "Start Docker Desktop when you log in"
- ✅ Check "Open Docker Dashboard at startup" (optional)

### 3. Set Up Environment Variables
```bash
# Copy the template
cp .env.docker .env

# Edit with your credentials
nano .env  # or use your favorite editor

# Required values:
# GITHUB_TOKEN=ghp_your_token_here
# GITHUB_USERNAME=your_username
```

### 4. Locate Your cbpr Binary
```bash
# Find where cbpr is installed
which cbpr

# If it outputs /usr/local/bin/cbpr, you're good!
# Otherwise, update CBPR_PATH in .env with the correct path
```

### 5. Build the Docker Image (First Time Only)
```bash
make build
# or
docker-compose build
```

---

## Monday Morning Workflow

### Option A: Using the Script (Recommended)
```bash
cd ~/pr-review-server
./start-week.sh
```

### Option B: Using Make
```bash
cd ~/pr-review-server
make start
```

### Option C: Direct Docker Compose
```bash
cd ~/pr-review-server
docker-compose up -d
```

That's it! The server will now run all week.

---

## Daily Operations

### View the Dashboard
Open in browser: http://localhost:7769

### Check Server Status
```bash
make status
```

### View Live Logs
```bash
make logs
# Press Ctrl+C to exit (server keeps running)
```

### Restart Server (if needed)
```bash
make restart
```

---

## Friday Afternoon (Optional)

### Stop the Server for the Weekend
```bash
make stop
```

The server will automatically stop when you:
- Shut down your laptop
- Quit Docker Desktop

---

## After Code Changes

If you modify the Go code:

```bash
make rebuild
```

This will:
1. Stop the old container
2. Rebuild the Docker image with new code
3. Start the new container

---

## Troubleshooting

### Server Won't Start
```bash
# Check Docker is running
docker info

# Check logs for errors
make logs

# Try rebuilding
make rebuild
```

### Can't Access Dashboard
```bash
# Check if container is running
make status

# Check port isn't in use
lsof -i :7769

# Try restarting
make restart
```

### cbpr Not Found in Container
The issue is likely that cbpr isn't mounted correctly.

```bash
# Find cbpr on your host
which cbpr

# Update CBPR_PATH in .env
echo "CBPR_PATH=/path/to/your/cbpr" >> .env

# Restart
make restart
```

### Want to Start Fresh
```bash
make clean    # Removes containers but keeps data
rm -rf data/  # If you want to delete the database too
make start
```

---

## Understanding the Setup

### What Runs Automatically?
1. **Docker Desktop** - Auto-starts on login (if configured)
2. **Your Container** - Auto-starts when Docker starts (thanks to `restart: unless-stopped`)

### What's Persistent?
These directories survive container restarts:
- `./reviews/` - Generated HTML reviews
- `./data/` - SQLite database (tracked PRs)

### Network Access
- Host: `http://localhost:7769`
- Inside container: The server runs on port 8080 (mapped to 7769 on host)

### Resource Usage
- **Memory**: ~100-500MB (depends on cbpr)
- **CPU**: Low (only spikes during cbpr reviews)
- **Disk**: Grows with reviews (~1-5MB per review)

---

## Advanced: Fully Automatic Startup

If you want the server to start automatically when you login without running any commands:

### Create a LaunchAgent for Docker Compose

```bash
# Create the plist file
cat > ~/Library/LaunchAgents/com.user.pr-review-docker.plist << 'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.user.pr-review-docker</string>

    <key>ProgramArguments</key>
    <array>
        <string>/usr/local/bin/docker-compose</string>
        <string>-f</string>
        <string>/Users/zbedmm/pr-review-server/docker-compose.yml</string>
        <string>up</string>
        <string>-d</string>
    </array>

    <key>WorkingDirectory</key>
    <string>/Users/zbedmm/pr-review-server</string>

    <key>RunAtLoad</key>
    <true/>

    <key>StandardOutPath</key>
    <string>/Users/zbedmm/pr-review-server/logs/launchd-stdout.log</string>

    <key>StandardErrorPath</key>
    <string>/Users/zbedmm/pr-review-server/logs/launchd-stderr.log</string>
</dict>
</plist>
EOF

# Create logs directory
mkdir -p ~/pr-review-server/logs

# Load the service
launchctl load ~/Library/LaunchAgents/com.user.pr-review-docker.plist
```

Now it will start automatically on login!

To disable:
```bash
launchctl unload ~/Library/LaunchAgents/com.user.pr-review-docker.plist
```

---

## Questions?

Run `make help` for a quick reference of available commands.

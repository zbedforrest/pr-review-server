# Audio Setup for Docker on macOS

This guide explains how to enable voice notifications from the Docker container on your Mac.

## How It Works

The server uses platform-specific text-to-speech:
- **macOS (local)**: Uses the native `say` command
- **Linux (Docker)**: Uses `espeak-ng` with audio routed through PulseAudio

When running in Docker on Mac, the Linux container uses PulseAudio to send audio to your Mac's speakers.

## Prerequisites

You need PulseAudio installed and configured on your Mac to receive audio from the container.

## Setup Steps

### 1. Install PulseAudio on your Mac

```bash
brew install pulseaudio
```

### 2. Configure PulseAudio to Accept Network Connections

Create or edit the PulseAudio configuration file:

```bash
mkdir -p ~/.config/pulse
cat > ~/.config/pulse/default.pa << 'EOF'
# Load the native protocol module for network access
load-module module-native-protocol-tcp auth-ip-acl=127.0.0.1;172.16.0.0/12 auth-anonymous=1

# Load the rest of the default configuration
.include /opt/homebrew/etc/pulse/default.pa
EOF
```

**Note**: If you installed PulseAudio via Intel Homebrew (not Apple Silicon), use `/usr/local/etc/pulse/default.pa` instead.

### 3. Start PulseAudio

```bash
# Kill any existing PulseAudio instance
pulseaudio --kill

# Start PulseAudio as a daemon
pulseaudio --start --load="module-native-protocol-tcp auth-ip-acl=127.0.0.1;172.16.0.0/12 auth-anonymous=1" --exit-idle-time=-1
```

**Tip**: Add this to your shell profile (`.zshrc` or `.bash_profile`) to start automatically:

```bash
# Auto-start PulseAudio if not running
if ! pgrep -x "pulseaudio" > /dev/null; then
    pulseaudio --start --load="module-native-protocol-tcp auth-ip-acl=127.0.0.1;172.16.0.0/12 auth-anonymous=1" --exit-idle-time=-1
fi
```

### 4. Verify PulseAudio is Running

```bash
pulseaudio --check
echo $?  # Should output 0 if running
```

### 5. Test the Setup

Start your Docker container:

```bash
docker compose up --build
```

Wait for a PR event that triggers a voice notification, or manually test:

```bash
# Test from within the container
docker exec -it pr-review-server espeak-ng -s 175 "Testing audio from container"
```

You should hear the message through your Mac's speakers.

## Troubleshooting

### No Audio Heard

1. **Check PulseAudio is running on Mac**:
   ```bash
   pulseaudio --check && echo "Running" || echo "Not running"
   ```

2. **Check container can reach PulseAudio**:
   ```bash
   docker exec -it pr-review-server ping -c 1 host.docker.internal
   ```

3. **Check PulseAudio logs on Mac**:
   ```bash
   tail -f ~/.config/pulse/*.log
   ```

4. **Test direct espeak-ng in container** (should fail silently without PulseAudio):
   ```bash
   docker exec -it pr-review-server espeak-ng "test"
   ```

### Permission Issues

Ensure the PulseAudio cookie is readable:
```bash
ls -la ~/.config/pulse/cookie
chmod 644 ~/.config/pulse/cookie
```

### Container Can't Connect

If you see connection errors, verify the network configuration:
```bash
# On Mac
netstat -an | grep 4713  # PulseAudio default port

# Check container environment
docker exec -it pr-review-server env | grep PULSE
```

## Disabling Voice Notifications

If you don't want voice notifications at all, set this environment variable:

```bash
# In docker-compose.yml or .env file
ENABLE_VOICE_NOTIFICATIONS=false
```

## Alternative: File-Based Audio (Fallback)

If PulseAudio is too complex, you can modify the code to write WAV files to a shared volume and play them on the host, though this is less elegant.

## Platform Detection

The code automatically detects the platform:
- `darwin` (macOS): Uses `say` command
- `linux`: Uses `espeak-ng` with PulseAudio

No configuration needed - it just works on both platforms when properly set up.

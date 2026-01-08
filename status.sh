#!/bin/bash
# PR Review Server Status Checker

echo "=== PR Review Server Status ==="
echo ""

# Check if server process is running
SERVER_PID=$(ps aux | grep "[p]r-review-server$" | awk '{print $2}')
if [ -n "$SERVER_PID" ]; then
    echo "✅ Server is RUNNING (PID: $SERVER_PID)"
else
    echo "❌ Server is NOT running"
    exit 1
fi

# Check if web server is responding
HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" http://localhost:7769 2>&1)
if [ "$HTTP_CODE" == "200" ]; then
    echo "✅ Web interface responding at http://localhost:7769"
else
    echo "⚠️  Web interface not responding (HTTP $HTTP_CODE)"
fi

# Check database stats
if [ -f pr-review.db ]; then
    echo ""
    echo "=== PR Status ==="
    sqlite3 pr-review.db "SELECT status, COUNT(*) as count FROM prs GROUP BY status ORDER BY CASE status WHEN 'completed' THEN 1 WHEN 'generating' THEN 2 WHEN 'pending' THEN 3 ELSE 4 END" 2>/dev/null | while IFS='|' read status count; do
        case $status in
            completed) echo "✅ Completed: $count" ;;
            generating) echo "🔄 Generating: $count" ;;
            pending) echo "⏳ Pending: $count" ;;
            error) echo "❌ Error: $count" ;;
        esac
    done
fi

# Check for running cbpr process
CBPR_PID=$(ps aux | grep "cbpr review" | grep -v grep | awk '{print $2}' | head -1)
if [ -n "$CBPR_PID" ]; then
    PR_NUM=$(ps aux | grep "cbpr review" | grep -v grep | sed -n 's/.*-p \([0-9]*\).*/\1/p' | head -1)
    echo ""
    echo "🔄 Currently reviewing PR #$PR_NUM (PID: $CBPR_PID)"
fi

echo ""
echo "Recent activity:"
tail -n 10 server.log | grep "Marked PR" | tail -3

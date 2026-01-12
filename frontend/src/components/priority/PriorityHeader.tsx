import { formatTime } from '@/utils/formatDate';

interface PriorityHeaderProps {
  count: number;
  timestamp: string | null;
  isCollapsed: boolean;
  onToggle: () => void;
}

export function PriorityHeader({ count, timestamp, isCollapsed, onToggle }: PriorityHeaderProps) {
  return (
    <div className="priority-section__header">
      <div>
        <h2>Priority Queue</h2>
        <span className="priority-section__count">
          {count} {count === 1 ? 'PR' : 'PRs'} to review
        </span>
        {timestamp && (
          <span className="priority-section__timestamp">
            {' '}(updated {formatTime(timestamp)})
          </span>
        )}
      </div>
      <button
        className="priority-section__toggle"
        onClick={onToggle}
        aria-expanded={!isCollapsed}
        aria-label={isCollapsed ? 'Expand priority queue' : 'Collapse priority queue'}
      >
        {isCollapsed ? 'Expand ▼' : 'Collapse ▲'}
      </button>
    </div>
  );
}

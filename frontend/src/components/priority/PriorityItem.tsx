import { memo } from 'react';
import type { PrioritizedPR } from '@/types/priority';

interface PriorityItemProps {
  item: PrioritizedPR;
}

export const PriorityItem = memo(function PriorityItem({ item }: PriorityItemProps) {
  const priorityLower = item.priority.toLowerCase();

  const priorityClass = priorityLower === 'high'
    ? 'priority-item--high'
    : priorityLower === 'medium'
    ? 'priority-item--medium'
    : 'priority-item--low';

  const badgeClass = priorityLower === 'high'
    ? 'priority-item__badge--high'
    : priorityLower === 'medium'
    ? 'priority-item__badge--medium'
    : 'priority-item__badge--low';

  const priorityEmoji = item.priority_emoji;

  return (
    <div className={`priority-item ${priorityClass}`}>
      <div className="priority-item__header">
        <div className="priority-item__title">
          <a href={item.github_url}>
            {item.owner}/{item.repo} #{item.number}
          </a>
        </div>
        <span className={`priority-item__badge ${badgeClass}`}>
          {priorityEmoji} {item.priority} (Score: {item.score})
        </span>
      </div>

      <div className="priority-item__subtitle">
        {item.title}
      </div>

      <div className="priority-item__meta">
        <span>👤 {item.author}</span>
        <span>✅ {item.approval_count} approvals</span>
        <span>📊 +{item.additions} -{item.deletions}</span>
        <span>📁 {item.changed_files} files</span>
      </div>

      {item.reasons && item.reasons.length > 0 && (
        <div className="priority-item__reasons">
          <strong>Why prioritized:</strong> {item.reasons.join(', ')}
        </div>
      )}

      <div className="priority-item__links">
        <a href={item.github_url}>
          View PR
        </a>
        {' | '}
        <a href={item.review_url}>
          View Review
        </a>
      </div>
    </div>
  );
});

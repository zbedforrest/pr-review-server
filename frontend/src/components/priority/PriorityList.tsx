import { PriorityItem } from './PriorityItem';
import type { PrioritizedPR } from '@/types/priority';

interface PriorityListProps {
  items: PrioritizedPR[];
  isCollapsed: boolean;
}

export function PriorityList({ items, isCollapsed }: PriorityListProps) {
  if (isCollapsed) return null;

  if (items.length === 0) {
    return (
      <div className="priority-section__list">
        <p className="priority-section__empty">No PRs to prioritize at the moment.</p>
      </div>
    );
  }

  return (
    <div className="priority-section__list">
      {items.map((item) => (
        <PriorityItem
          key={`${item.owner}/${item.repo}/${item.number}`}
          item={item}
        />
      ))}
    </div>
  );
}

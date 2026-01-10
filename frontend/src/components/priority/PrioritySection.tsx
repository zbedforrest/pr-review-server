import { usePriorities } from '@/hooks/usePriorities';
import { useUIStore } from '@/store';
import { PriorityHeader } from './PriorityHeader';
import { PriorityList } from './PriorityList';
import { LoadingSpinner } from '@/components/common';

export function PrioritySection() {
  const { data: result, isLoading, error } = usePriorities();
  const { priorityQueueCollapsed, togglePriorityQueue } = useUIStore();

  if (isLoading) {
    return (
      <section className="priority-section">
        <PriorityHeader
          count={0}
          timestamp={null}
          isCollapsed={priorityQueueCollapsed}
          onToggle={togglePriorityQueue}
        />
        {!priorityQueueCollapsed && <LoadingSpinner />}
      </section>
    );
  }

  if (error) {
    return (
      <section className="priority-section">
        <PriorityHeader
          count={0}
          timestamp={null}
          isCollapsed={priorityQueueCollapsed}
          onToggle={togglePriorityQueue}
        />
        {!priorityQueueCollapsed && (
          <div className="priority-section__error">
            Error loading priorities: {error instanceof Error ? error.message : 'Unknown error'}
          </div>
        )}
      </section>
    );
  }

  if (!result) return null;

  const sectionClass = priorityQueueCollapsed ? 'priority-section collapsed' : 'priority-section';

  return (
    <section className={sectionClass}>
      <PriorityHeader
        count={result.top_prs.length}
        timestamp={result.timestamp}
        isCollapsed={priorityQueueCollapsed}
        onToggle={togglePriorityQueue}
      />
      <PriorityList items={result.top_prs} isCollapsed={priorityQueueCollapsed} />
    </section>
  );
}

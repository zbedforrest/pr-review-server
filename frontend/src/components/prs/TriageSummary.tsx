import { useMemo, useState } from 'react';
import type { PR } from '../../types/pr';
import { categorizePR, type TriageFilter } from './triageUtils';
import { useTelemetry } from '@/hooks/useTelemetry';
import './TriageSummary.scss';

interface TriageSummaryProps {
  prs: PR[];
  onFilterChange?: (filter: TriageFilter | null) => void;
}

interface TriageCategory {
  id: TriageFilter;
  label: string;
  emoji: string;
  description: string;
  prs: PR[];
  className: string;
}

export function TriageSummary({ prs, onFilterChange }: TriageSummaryProps) {
  const [activeFilter, setActiveFilter] = useState<TriageFilter | null>(null);
  const { track } = useTelemetry();

  const categories = useMemo((): TriageCategory[] => {
    // Only consider PRs that need review (not mine, completed status)
    const reviewPRs = prs.filter(pr => !pr.is_mine && pr.status === 'completed');

    const critical: PR[] = [];
    const medium: PR[] = [];
    const quickApprove: PR[] = [];

    for (const pr of reviewPRs) {
      const category = categorizePR(pr);
      switch (category) {
        case 'critical':
          critical.push(pr);
          break;
        case 'medium':
          medium.push(pr);
          break;
        case 'quick-approve':
          quickApprove.push(pr);
          break;
      }
    }

    // Sort by severity within each category
    const sortBySeverity = (a: PR, b: PR) => {
      const aScore = (a.critical_count * 100) + (a.medium_count * 10) + a.low_count;
      const bScore = (b.critical_count * 100) + (b.medium_count * 10) + b.low_count;
      return bScore - aScore;
    };

    critical.sort(sortBySeverity);
    medium.sort(sortBySeverity);
    quickApprove.sort(sortBySeverity);

    return [
      {
        id: 'critical',
        label: 'Needs Close Inspection',
        emoji: '\u{1F534}', // red circle
        description: 'Has critical issues',
        prs: critical,
        className: 'critical',
      },
      {
        id: 'medium',
        label: 'Multiple Issues',
        emoji: '\u{1F7E0}', // orange circle
        description: '2+ medium issues',
        prs: medium,
        className: 'medium',
      },
      {
        id: 'quick-approve',
        label: 'Quick Approve',
        emoji: '\u{1F7E2}', // green circle
        description: 'Minor or no issues',
        prs: quickApprove,
        className: 'quick-approve',
      },
    ];
  }, [prs]);

  const totalReviewPRs = categories.reduce((sum, cat) => sum + cat.prs.length, 0);

  const handleCategoryClick = (categoryId: TriageFilter) => {
    const newFilter = activeFilter === categoryId ? null : categoryId;
    setActiveFilter(newFilter);
    onFilterChange?.(newFilter);
    track('triage_filter', { label: newFilter ?? 'clear' });
  };

  if (totalReviewPRs === 0) {
    return null;
  }

  return (
    <div className="triage-summary">
      <div className="triage-header">
        <h3>PR Triage</h3>
        <span className="triage-total">{totalReviewPRs} PRs to review</span>
      </div>
      <div className="triage-categories">
        {categories.map(category => (
          <button
            key={category.id}
            className={`triage-category ${category.className} ${activeFilter === category.id ? 'active' : ''} ${category.prs.length === 0 ? 'empty' : ''}`}
            onClick={() => handleCategoryClick(category.id)}
            disabled={category.prs.length === 0}
            title={category.description}
          >
            <span className="category-emoji">{category.emoji}</span>
            <span className="category-count">{category.prs.length}</span>
            <span className="category-label">{category.label}</span>
          </button>
        ))}
      </div>
      {activeFilter && (
        <div className="triage-filter-hint">
          Filtering: {categories.find(c => c.id === activeFilter)?.label}
          <button className="clear-filter" onClick={() => handleCategoryClick(activeFilter)}>
            Clear
          </button>
        </div>
      )}
    </div>
  );
}

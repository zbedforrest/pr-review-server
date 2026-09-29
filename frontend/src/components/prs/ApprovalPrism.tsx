import type { CSSProperties } from 'react';

export function ApprovalPrism({ finished, total, summary, running, reconnecting }: {
  finished: number; total: number; summary: string; running: number; reconnecting: boolean;
}) {
  const percent = total ? Math.min(100, Math.max(0, finished / total * 100)) : 0;
  return <div className="approval-prism-status" style={{ '--approval-progress': `${percent}%` } as CSSProperties}>
    <div className="approval-prism-fill" aria-hidden="true" />
    <svg className="approval-prism-art" viewBox="0 0 240 80" aria-hidden="true">
      <path className="approval-prism-beam" d="M0 47H91" />
      <g className="approval-prism-spectrum">
        <path d="M120 40 240 10" stroke="#f28ea4" />
        <path d="M120 43 240 25" stroke="#f3b879" />
        <path d="M120 46 240 40" stroke="#e6d983" />
        <path d="M120 49 240 55" stroke="#82d5b5" />
        <path d="M120 52 240 70" stroke="#82b8f3" />
        <path d="M120 55 240 85" stroke="#b29aee" />
      </g>
      <path className="approval-prism-glass" d="m112 9 29 59H78Z" />
      <path className="approval-prism-facet" d="m112 9-5 42 34 17M78 68l29-17" />
      <path className="approval-prism-inner" d="m91 47 29-4" />
    </svg>
    <div className="approval-prism-copy">
      <span className="approval-prism-eyebrow"><span className="approval-prism-dot" />{reconnecting ? 'Reconnecting' : 'Investigating'}{running > 1 ? ` · ${running} agents` : ''}</span>
      <span className="approval-prism-summary" role="status" aria-live="polite" aria-atomic="true">{summary}</span>
    </div>
    <div className="approval-prism-meter" role="progressbar" aria-label="Pull requests investigated" aria-valuemin={0} aria-valuemax={total || 1} aria-valuenow={Math.min(finished, total)} aria-valuetext={`${finished} of ${total} pull requests finished`}>
      <strong>{finished}<span> / {total}</span></strong>
      <span>PRs finished</span>
    </div>
  </div>;
}

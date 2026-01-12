export interface PR {
  owner: string;
  repo: string;
  number: number;
  commit_sha: string;
  last_reviewed_at: string | null;
  review_html_path: string;
  github_url: string;
  review_url: string;
  status: 'pending' | 'generating' | 'completed' | 'error';
  title: string;
  author: string;
  generating_since: string | null;
  is_mine: boolean;
  my_review_status: 'APPROVED' | 'CHANGES_REQUESTED' | 'COMMENTED' | '';
  approval_count: number;
  draft: boolean;
  notes: string;
  ci_state: 'success' | 'failure' | 'pending' | 'unknown';
  ci_failed_checks: string[];
}

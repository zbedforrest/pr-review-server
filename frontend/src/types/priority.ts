export interface PrioritizedPR {
  owner: string;
  repo: string;
  number: number;
  title: string;
  author: string;
  score: number;
  priority: 'HIGH' | 'MEDIUM' | 'LOW' | 'SKIP';
  priority_emoji: string;
  reasons: string[];
  age_days: number;
  additions: number;
  deletions: number;
  changed_files: number;
  review_count: number;
  approval_count: number;
  my_review_status: string;
  github_url: string;
  review_url: string;
  created_at: string;
}

export interface PriorityResult {
  timestamp: string;
  top_prs: PrioritizedPR[];
  total_prs_scored: number;
  high_priority_count: number;
  medium_priority_count: number;
  low_priority_count: number;
}

import { useState, useRef, useEffect } from 'react';
import { usePRs } from '@/hooks/usePRs';
import { useCurrentUser } from '@/hooks/useCurrentUser';
import { useTelemetry } from '@/hooks/useTelemetry';
import { VIA_TEAMS_PERSONAL } from '@/constants';
import './FilterBar.scss';

interface FilterBarProps {
  className?: string;
  layout?: 'stacked' | 'inline';
  selectedTeams: string[];
  onTeamsChange: (teams: string[]) => void;
  selectedRepos: string[];
  onReposChange: (repos: string[]) => void;
}

function getTeamFilterName(team: string, username?: string): string {
  if (team === VIA_TEAMS_PERSONAL) {
    return username ? `@${username}` : '@you';
  }

  const suffixIndex = team.lastIndexOf(':');
  return suffixIndex === -1 ? team : team.slice(0, suffixIndex);
}

export function FilterBar({
  className = '',
  layout = 'stacked',
  selectedTeams,
  onTeamsChange,
  selectedRepos,
  onReposChange,
}: FilterBarProps) {
  const { data: prs } = usePRs();
  const { data: currentUser } = useCurrentUser();
  const { track } = useTelemetry();
  const [collapsed, setCollapsed] = useState(true);
  const [repoDropdownOpen, setRepoDropdownOpen] = useState(false);
  const dropdownRef = useRef<HTMLDivElement>(null);

  const allTeams = Array.from(
    new Set((prs || []).flatMap(pr => pr.via_teams.map(team => getTeamFilterName(team, currentUser?.github_username))))
  ).sort();
  const allRepos = Array.from(new Set((prs || []).map(pr => `${pr.owner}/${pr.repo}`))).sort();
  const availableRepos = allRepos.filter(r => !selectedRepos.includes(r));

  const activeCount = selectedTeams.length + selectedRepos.length;

  useEffect(() => {
    function handleClickOutside(e: MouseEvent) {
      if (dropdownRef.current && !dropdownRef.current.contains(e.target as Node)) {
        setRepoDropdownOpen(false);
      }
    }
    document.addEventListener('mousedown', handleClickOutside);
    return () => document.removeEventListener('mousedown', handleClickOutside);
  }, []);

  const toggleTeam = (team: string) => {
    if (selectedTeams.includes(team)) {
      track('filter_team', { label: `remove:${team}` });
      onTeamsChange(selectedTeams.filter(t => t !== team));
    } else {
      track('filter_team', { label: `add:${team}` });
      onTeamsChange([...selectedTeams, team]);
    }
  };

  const removeRepo = (repo: string) => {
    track('filter_repo', { label: `remove:${repo}` });
    onReposChange(selectedRepos.filter(r => r !== repo));
  };

  const addRepo = (repo: string) => {
    track('filter_repo', { label: `add:${repo}` });
    onReposChange([...selectedRepos, repo]);
    setRepoDropdownOpen(false);
  };

  const clearAll = () => {
    track('clear_filters', { label: `teams:${selectedTeams.length},repos:${selectedRepos.length}` });
    onTeamsChange([]);
    onReposChange([]);
  };

  if (allTeams.length === 0 && allRepos.length === 0) return null;

  return (
    <div className={`filter-bar filter-bar--${layout} ${className}`.trim()}>
      <button
        className="filter-bar__toggle"
        onClick={() => {
          const next = !collapsed;
          track('toggle_filters', { label: next ? 'open' : 'close' });
          setCollapsed(next);
        }}
      >
        <svg className={`filter-bar__chevron ${collapsed ? '' : 'filter-bar__chevron--open'}`} width="12" height="12" viewBox="0 0 12 12">
          <path d="M4 2l4 4-4 4" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" />
        </svg>
        Filters
        {activeCount > 0 && <span className="filter-bar__count">{activeCount}</span>}
      </button>

      {!collapsed && (
        <div className="filter-bar__content">
          {allTeams.length > 0 && (
            <div className="filter-bar__section">
              <span className="filter-bar__label">Teams</span>
              <div className="filter-bar__chips">
                {allTeams.map(team => (
                  <button
                    key={team}
                    className={`filter-chip ${selectedTeams.includes(team) ? 'filter-chip--active' : ''}`}
                    onClick={() => toggleTeam(team)}
                  >
                    <span className="filter-chip__label">{team}</span>
                    {selectedTeams.includes(team) && (
                      <span className="filter-chip__remove" onClick={(e) => { e.stopPropagation(); toggleTeam(team); }}>
                        <svg width="10" height="10" viewBox="0 0 10 10">
                          <path d="M2 2l6 6M8 2l-6 6" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" />
                        </svg>
                      </span>
                    )}
                  </button>
                ))}
              </div>
            </div>
          )}

          {allRepos.length > 0 && (
            <div className="filter-bar__section">
              <span className="filter-bar__label">Repos</span>
              <div className="filter-bar__repo-row">
                {selectedRepos.map(repo => (
                  <button key={repo} className="filter-chip filter-chip--active" onClick={() => removeRepo(repo)}>
                    <span className="filter-chip__label">{repo}</span>
                    <span className="filter-chip__remove">
                      <svg width="10" height="10" viewBox="0 0 10 10">
                        <path d="M2 2l6 6M8 2l-6 6" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" />
                      </svg>
                    </span>
                  </button>
                ))}

                {availableRepos.length > 0 && (
                  <div className="filter-bar__dropdown" ref={dropdownRef}>
                    <button
                      className="filter-bar__add-btn"
                      onClick={() => {
                        const next = !repoDropdownOpen;
                        track('toggle_repo_filter_dropdown', { label: next ? 'open' : 'close' });
                        setRepoDropdownOpen(next);
                      }}
                    >
                      + Add repo
                    </button>
                    {repoDropdownOpen && (
                      <ul className="filter-bar__dropdown-menu">
                        {availableRepos.map(repo => (
                          <li key={repo}>
                            <button onClick={() => addRepo(repo)}>{repo}</button>
                          </li>
                        ))}
                      </ul>
                    )}
                  </div>
                )}
              </div>
            </div>
          )}

          {activeCount > 0 && (
            <button className="filter-bar__clear" onClick={clearAll}>
              Clear all filters
            </button>
          )}
        </div>
      )}
    </div>
  );
}

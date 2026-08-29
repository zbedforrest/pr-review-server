import { Component, ErrorInfo, ReactNode, useCallback, useState, useEffect } from 'react';
import { QueryClient, QueryClientProvider, useQueryClient } from '@tanstack/react-query';
import { ReactQueryDevtools } from '@tanstack/react-query-devtools';
import { Header, StatusBar } from '@/components/layout';
import { FilterBar } from '@/components/filters';
import { ReviewPRsSection, StatusPRPanel } from '@/components/prs';
import { useTelemetry } from '@/hooks/useTelemetry';
import { useUrlFilters } from '@/hooks/useUrlFilters';
import { UsageStatsPage } from '@/components/telemetry/UsageStatsPage';
import { PR } from '@/types/pr';
import { ServerStatus, StatusPanelFilter } from '@/types/status';
import { ConnectionStatus, subscribeToWebSocketMessages, subscribeToWebSocketStatus } from '@/utils/websocket';
import { applyPRWebSocketMessage, applyStatusWebSocketMessage } from '@/utils/websocketCacheUpdates';
import '@/styles/main.scss';

// Create query client
const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      retry: 1,
      refetchOnWindowFocus: false,
    },
  },
});

class ErrorBoundary extends Component<
  { children: ReactNode },
  { hasError: boolean; error: Error | null }
> {
  constructor(props: { children: ReactNode }) {
    super(props);
    this.state = { hasError: false, error: null };
  }

  static getDerivedStateFromError(error: Error) {
    return { hasError: true, error };
  }

  componentDidCatch(error: Error, errorInfo: ErrorInfo) {
    console.error('React Error:', error, errorInfo);
  }

  render() {
    if (this.state.hasError) {
      return (
        <div className="app-error-boundary">
          <h1>Something went wrong</h1>
          <pre className="app-error-boundary__stack">
            {this.state.error?.toString()}
            {'\n'}
            {this.state.error?.stack}
          </pre>
        </div>
      );
    }

    return this.props.children;
  }
}

function AppContent() {
  const queryClient = useQueryClient();
  const { track, trackSearch } = useTelemetry();

  // Connection status state
  const [connectionStatus, setConnectionStatus] = useState<ConnectionStatus>('connecting');

  // Status-bar count the user drilled into, with the count they clicked (the
  // status bar's counts are server-wide, the panel's table is user-scoped).
  const [statusPanel, setStatusPanel] = useState<{ status: StatusPanelFilter; count: number } | null>(null);
  const closeStatusPanel = useCallback(() => setStatusPanel(null), []);

  // Search + filter state, mirrored into URL query params for back/forward nav
  const {
    searchTerm,
    setSearchTerm,
    selectedTeams,
    setSelectedTeams,
    selectedRepos,
    setSelectedRepos,
    selectedStates,
    setSelectedStates,
  } = useUrlFilters();

  // Shared WebSocket connection
  useEffect(() => {
    const unsubscribeStatus = subscribeToWebSocketStatus((status) => {
      setConnectionStatus(status);
      if (status === 'connected') {
        queryClient.invalidateQueries({ queryKey: ['prs'] });
      }
    });

    const unsubscribeMessages = subscribeToWebSocketMessages((message) => {
      queryClient.setQueryData<PR[]>(['prs'], (oldData) => applyPRWebSocketMessage(oldData, message));
      queryClient.setQueryData<ServerStatus | undefined>(['status'], (oldData) => applyStatusWebSocketMessage(oldData, message));
    });

    return () => {
      unsubscribeMessages();
      unsubscribeStatus();
    };
  }, [queryClient]);

  return (
    <div className="app-container">
      <Header />
      <StatusBar
        connectionStatus={connectionStatus}
        activeStatusCount={statusPanel?.status ?? null}
        onStatusCountClick={(status, count) => {
          // Clicking the count that's already open closes the panel.
          const closing = statusPanel?.status === status;
          track('status_count_panel', { label: `${closing ? 'close' : 'open'}:${status}` });
          setStatusPanel(closing ? null : { status, count });
        }}
      />

      {statusPanel && (
        <StatusPRPanel
          status={statusPanel.status}
          serverCount={statusPanel.count}
          onClose={closeStatusPanel}
        />
      )}

      <div className="search-controls">
        <FilterBar
          className="search-controls__filter"
          layout="inline"
          selectedTeams={selectedTeams}
          onTeamsChange={setSelectedTeams}
          selectedRepos={selectedRepos}
          onReposChange={setSelectedRepos}
          selectedStates={selectedStates}
          onStatesChange={setSelectedStates}
        />
        <div className="search-container">
          <input
            type="text"
            placeholder="Search PRs (title, repo, author, number)..."
            value={searchTerm}
            onChange={(e) => {
              setSearchTerm(e.target.value);
              trackSearch(e.target.value);
            }}
          />
        </div>
      </div>
      <ReviewPRsSection
        searchTerm={searchTerm}
        selectedTeams={selectedTeams}
        selectedRepos={selectedRepos}
        selectedStates={selectedStates}
      />
    </div>
  );
}

function AppRouter() {
  const path = window.location.pathname;

  if (path === '/usage-stats') {
    return <UsageStatsPage />;
  }

  return <AppContent />;
}

function App() {
  return (
    <ErrorBoundary>
      <QueryClientProvider client={queryClient}>
        <AppRouter />
        <ReactQueryDevtools initialIsOpen={false} />
      </QueryClientProvider>
    </ErrorBoundary>
  );
}

export default App;

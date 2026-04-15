import { Component, ErrorInfo, ReactNode, useState, useEffect } from 'react';
import { QueryClient, QueryClientProvider, useQueryClient } from '@tanstack/react-query';
import { ReactQueryDevtools } from '@tanstack/react-query-devtools';
import { Header, StatusBar } from '@/components/layout';
import { ReviewPRsSection } from '@/components/prs';
import { useReviewerHealth } from '@/hooks/useReviewerHealth';
import { useTelemetry } from '@/hooks/useTelemetry';
import { UsageStatsPage } from '@/components/telemetry/UsageStatsPage';
import { PR } from '@/types/pr';
import { ServerStatus } from '@/types/status';
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
        <div style={{ padding: '20px', color: '#ff6b6b', backgroundColor: '#161b22' }}>
          <h1>Something went wrong</h1>
          <pre style={{ color: '#8b949e', fontSize: '12px', overflow: 'auto' }}>
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
  const { data: reviewerHealth } = useReviewerHealth();
  const queryClient = useQueryClient();
  const { track, trackSearch } = useTelemetry();

  // Column visibility state - default based on reviewer health
  // We use an internal state to track if we've already performed the initial setup
  const [hasInitializedColumns, setHasInitializedColumns] = useState(false);
  const [showReviewColumns, setShowReviewColumns] = useState(true);

  // Auto-hide columns if reviewer is unhealthy (only on first load when data arrives)
  if (!hasInitializedColumns && reviewerHealth) {
    setShowReviewColumns(reviewerHealth.recommendation === 'show_columns');
    setHasInitializedColumns(true);
  }

  // Connection status state
  const [connectionStatus, setConnectionStatus] = useState<ConnectionStatus>('connecting');

  // Search state
  const [searchTerm, setSearchTerm] = useState('');

  // Filter state
  const [selectedTeams, setSelectedTeams] = useState<string[]>([]);
  const [selectedRepos, setSelectedRepos] = useState<string[]>([]);

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

  const handleToggleColumns = () => {
    const next = !showReviewColumns;
    setShowReviewColumns(next);
    track('toggle_columns', { label: next ? 'show' : 'hide' });
  };

  return (
    <div className="app-container">
      <Header />
      <StatusBar connectionStatus={connectionStatus} />

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



      <ReviewPRsSection
        showReviewColumns={showReviewColumns}
        onToggleColumns={handleToggleColumns}
        searchTerm={searchTerm}
        selectedTeams={selectedTeams}
        selectedRepos={selectedRepos}
        onTeamsChange={setSelectedTeams}
        onReposChange={setSelectedRepos}
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

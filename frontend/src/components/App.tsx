import { Component, ErrorInfo, ReactNode, useState, useEffect } from 'react';
import { QueryClient, QueryClientProvider, useQueryClient } from '@tanstack/react-query';
import { ReactQueryDevtools } from '@tanstack/react-query-devtools';
import { Header, StatusBar } from '@/components/layout';
import { ReviewPRsSection, TriageSummary, TriageFilter } from '@/components/prs';
import { useReviewerHealth } from '@/hooks/useReviewerHealth';
import { usePRs } from '@/hooks/usePRs';
import { useTelemetry } from '@/hooks/useTelemetry';
import { UsageStatsPage } from '@/components/telemetry/UsageStatsPage';
import { PR } from '@/types/pr';
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
  const { data: prs } = usePRs();
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
  const [connectionStatus, setConnectionStatus] = useState<'connected' | 'disconnected' | 'connecting'>('connecting');

  // Search state
  const [searchTerm, setSearchTerm] = useState('');

  // Triage filter state
  const [triageFilter, setTriageFilter] = useState<TriageFilter | null>(null);

  // WebSocket connection
  useEffect(() => {
    let ws: WebSocket | null = null;
    let reconnectTimeout: ReturnType<typeof setTimeout>;

    const connect = () => {
      setConnectionStatus('connecting');
      const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
      ws = new WebSocket(`${protocol}//${window.location.host}/ws`);

      ws.onopen = () => {
        console.log('WebSocket connected');
        setConnectionStatus('connected');
        // Refetch queries on connect to sync state (catch-up)
        queryClient.invalidateQueries({ queryKey: ['prs'] });
      };

      ws.onmessage = (event) => {
        try {
          const message = JSON.parse(event.data);
          const { type, payload } = message;

          queryClient.setQueryData<PR[]>(['prs'], (oldData) => {
            if (!oldData) return oldData;
            if (!payload) return oldData; // Guard against null payload

            switch (type) {
              case 'pr_created': {
                const newPR = payload as PR;
                if (!newPR) return oldData; // Extra safety
                // Avoid duplicates
                if (oldData.some(pr => pr.number === newPR.number && pr.repo === newPR.repo && pr.owner === newPR.owner)) {
                  return oldData.map(pr => 
                    pr.number === newPR.number && pr.repo === newPR.repo && pr.owner === newPR.owner ? newPR : pr
                  );
                }
                return [newPR, ...oldData];
              }

              case 'pr_updated': {
                const updatedPR = payload as PR;
                return oldData.map((pr) =>
                  pr.number === updatedPR.number && pr.repo === updatedPR.repo && pr.owner === updatedPR.owner
                    ? updatedPR
                    : pr
                );
              }

              case 'pr_deleted': {
                const { owner, repo, number } = payload;
                return oldData.filter((pr) => 
                  !(pr.number === number && pr.repo === repo && pr.owner === owner)
                );
              }

              default:
                return oldData;
            }
          });
        } catch (error) {
          console.error('Error processing WebSocket message:', error);
        }
      };

      ws.onerror = (error) => {
        console.error('WebSocket error:', error);
      };

      ws.onclose = () => {
        console.log('WebSocket disconnected, attempting to reconnect...');
        setConnectionStatus('disconnected');
        reconnectTimeout = setTimeout(connect, 3000); // Reconnect after 3 seconds
      };
    };

    connect();

    return () => {
      if (ws) ws.close();
      clearTimeout(reconnectTimeout);
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

      <div className="search-container" style={{ marginBottom: '20px' }}>
        <input
          type="text"
          placeholder="Search PRs (title, repo, author, number)..."
          value={searchTerm}
          onChange={(e) => {
            setSearchTerm(e.target.value);
            trackSearch(e.target.value);
          }}
          style={{
            width: '100%',
            padding: '12px 16px',
            fontSize: '16px',
            borderRadius: '6px',
            border: '1px solid #30363d',
            backgroundColor: '#0d1117',
            color: '#c9d1d9',
            outline: 'none',
          }}
        />
      </div>

      <TriageSummary prs={prs || []} onFilterChange={setTriageFilter} />

      <ReviewPRsSection
        showReviewColumns={showReviewColumns}
        onToggleColumns={handleToggleColumns}
        searchTerm={searchTerm}
        triageFilter={triageFilter}
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

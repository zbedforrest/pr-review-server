import { Component, ErrorInfo, ReactNode, useState, useEffect } from 'react';
import { QueryClient, QueryClientProvider, useQueryClient } from '@tanstack/react-query';
import { ReactQueryDevtools } from '@tanstack/react-query-devtools';
import { Header, StatusBar } from '@/components/layout';
import { FilterBar } from '@/components/filters';
import { ReviewPRsSection } from '@/components/prs';
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
  const { trackSearch } = useTelemetry();

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

  return (
    <div className="app-container">
      <Header />
      <StatusBar connectionStatus={connectionStatus} />

      <div className="search-controls">
        <FilterBar
          className="search-controls__filter"
          layout="inline"
          selectedTeams={selectedTeams}
          onTeamsChange={setSelectedTeams}
          selectedRepos={selectedRepos}
          onReposChange={setSelectedRepos}
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

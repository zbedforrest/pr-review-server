import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
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

function App() {
  return (
    <QueryClientProvider client={queryClient}>
      <div className="app-container">
        <h1>Testing React App</h1>
        <p>If you can see this, React is working!</p>
      </div>
    </QueryClientProvider>
  );
}

export default App;

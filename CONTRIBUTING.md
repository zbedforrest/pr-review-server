# Contributing to PR Review Server

Thank you for your interest in contributing to PR Review Server!

## Development Setup

### Prerequisites

- Go 1.24 or later
- Node.js 18+ and npm
- `cbpr` command-line tool (for testing review generation)
- GitHub personal access token with `repo` scope
- Docker Desktop (optional, for testing Docker builds)

### Initial Setup

1. **Clone the repository**:
   ```bash
   git clone <repository-url>
   cd pr-review-server
   ```

2. **Set up environment variables**:
   ```bash
   cp .env.example .env
   ```

   Edit `.env` and add your credentials:
   ```bash
   GITHUB_TOKEN=ghp_your_token_here
   GITHUB_USERNAME=your_github_username
   ```

3. **Install frontend dependencies**:
   ```bash
   cd frontend
   npm install
   cd ..
   ```

## Development Workflow

### Running in Development Mode

For the best development experience, run the backend and frontend separately:

1. **Start the Go backend**:
   ```bash
   ./dev.sh
   ```

   This runs the backend on port 7769 (or your configured SERVER_PORT) with `DEV_MODE=true`, which disables the embedded frontend and expects a separate frontend dev server.

2. **In a separate terminal, start the React frontend**:
   ```bash
   cd frontend
   npm run dev
   ```

   This runs the Vite dev server on port 3000 with hot module reloading.

3. **Access the app**:
   - Frontend: http://localhost:3000 (with hot reload)
   - Backend API: http://localhost:7769/api/

The frontend dev server proxies API requests to the backend automatically.

### Testing Production Build Locally

To test the full production build with embedded frontend:

```bash
./prod-local.sh
```

This:
1. Builds the React frontend (`npm run build`)
2. Embeds it in the Go binary
3. Runs the server in production mode

Access at: http://localhost:7769

### Testing Docker Build

```bash
# Build and run
docker-compose up --build

# View logs
docker-compose logs -f

# Stop
docker-compose down
```

## Project Structure

```
.
├── config/              # Configuration management
│   └── config.go
├── db/                  # Database layer (SQLite)
│   ├── db.go           # Core database operations
│   └── migrations.go   # Schema migrations
├── github/              # GitHub API client
│   └── client.go
├── poller/              # PR polling and review generation
│   └── poller.go
├── prioritization/      # PR prioritization logic
│   ├── prioritizer.go
│   └── prioritizer_test.go
├── server/              # HTTP server
│   ├── server.go       # API endpoints and static file serving
│   └── dist/           # Embedded React build (generated)
├── frontend/            # React dashboard
│   ├── src/
│   │   ├── components/ # React components
│   │   ├── hooks/      # Custom React hooks
│   │   ├── store/      # Zustand state management
│   │   ├── types/      # TypeScript type definitions
│   │   └── App.tsx     # Main application
│   ├── package.json
│   └── vite.config.ts
├── main.go              # Application entry point
└── scripts/             # Development and deployment scripts
```

## Code Style

### Go

- Follow standard Go conventions (`gofmt`, `golint`)
- Use meaningful variable names
- Add comments for exported functions and complex logic
- Keep functions focused and reasonably sized

### TypeScript/React

- Follow the existing ESLint configuration
- Use functional components with hooks
- Use TypeScript for type safety
- Keep components small and focused
- Use the existing state management patterns (Zustand)

Run linter:
```bash
cd frontend
npm run lint
```

## Making Changes

### Backend Changes

1. Make your changes in the appropriate Go package
2. Test manually with `./dev.sh`
3. Ensure the build works: `go build`
4. Test the full production build: `./prod-local.sh`

### Frontend Changes

1. Make your changes in `frontend/src/`
2. Test with hot reload: `cd frontend && npm run dev`
3. Check TypeScript: `npm run type-check`
4. Run linter: `npm run lint`
5. Build for production: `npm run build`
6. Test with backend: `./prod-local.sh`

### Database Schema Changes

If you need to modify the database schema:

1. Update the schema in `db/migrations.go`
2. Increment the schema version
3. Add migration logic to handle existing databases
4. Test with a fresh database and with existing data
5. Document the change in your commit message

### Adding Dependencies

**Go dependencies**:
```bash
go get <package>
go mod tidy
```

**Frontend dependencies**:
```bash
cd frontend
npm install <package>
```

## Testing

### Manual Testing Checklist

Before submitting a PR:

- [ ] Server starts successfully (both dev and prod modes)
- [ ] Frontend loads and displays correctly
- [ ] API endpoints return expected data
- [ ] Database operations work correctly
- [ ] PR polling and review generation work
- [ ] Docker build succeeds
- [ ] No console errors in browser
- [ ] No compilation warnings

### Automated Tests

Add tests for new features:

**Go tests**:
```bash
go test ./...
```

**Frontend tests** (when implemented):
```bash
cd frontend
npm test
```

## Common Development Tasks

### Add a New API Endpoint

1. Add the handler function in `server/server.go`
2. Register the route in the `Start()` function
3. Update types in `frontend/src/types/` if needed
4. Add frontend API call in the appropriate component or hook

### Add a New React Component

1. Create component in `frontend/src/components/`
2. Use TypeScript for props
3. Follow existing patterns for styling (inline styles or Sass)
4. Import and use in parent component

### Modify Database Schema

1. Update `db/migrations.go`:
   - Increment `currentSchemaVersion`
   - Add new migration in `migrateSchema()`
2. Update `db/db.go` if needed (add new query functions)
3. Update Go types to match new schema
4. Test migration from previous version

### Debug cbpr Integration

To test cbpr locally without the full server:

```bash
cbpr review --repo-name=owner/repo -p 123 --output=/tmp/test.html
ls -lh /tmp/test.html
open /tmp/test.html
```

## Documentation

When adding new features, update:

- `README.md` - User-facing changes
- `CONTRIBUTING.md` - Development-related changes
- Code comments - Complex logic
- Commit messages - Clear description of changes

## Submitting Changes

1. **Create a feature branch**:
   ```bash
   git checkout -b feature/your-feature-name
   ```

2. **Make your changes** following the guidelines above

3. **Commit with clear messages**:
   ```bash
   git add -A
   git commit -m "Add feature: description of what you added"
   ```

4. **Test thoroughly** (see Manual Testing Checklist)

5. **Push and create a pull request**:
   ```bash
   git push origin feature/your-feature-name
   ```

6. **Describe your changes** in the PR description:
   - What does this change?
   - Why is this change needed?
   - How was it tested?

## Need Help?

- Check existing documentation in `/docs` and markdown files
- Review existing code for patterns and examples
- Open an issue for questions or bugs
- Reach out to maintainers for guidance

## License

By contributing, you agree that your contributions will be licensed under the same license as the project.

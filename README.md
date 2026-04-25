# immaiwin (its my money and i want it now)

`immaiwin` is a high-performance market monitoring and analysis platform built with Go and React. It tracks unusual trading activity across prediction markets (Polymarket) and traditional options/futures (Schwab), while aggregating relevant financial news from multiple sources.

## Features

- **Unusual Trade Detection**: Monitors Polymarket and Schwab (Options & Futures) for large or anomalous trades using rolling averages and custom expressions.
- **Real-time Streaming**: Utilizes Redis Pub/Sub and WebSockets to stream trades and news updates instantly to the UI.
- **News Aggregator**: Scrapes and aggregates financial news from Al Jazeera, Bloomberg RSS, and Investing.com.
- **Unified UI**: A modern TanStack Start dashboard built with React and Tailwind CSS for visualizing market data and news.
- **Worker Architecture**: Scalable registry of background workers for scraping and watching different market segments.

## Prerequisites

- **Go**: 1.21 or later
- **Node.js**: 20.x or later (with `pnpm`)
- **Docker**: For running MongoDB and Redis
- **Make**: For running project commands

## Installation & Setup

### 1. Clone the Repository
```bash
git clone https://github.com/bRRRITSCOLD/immaiwin-go.git
cd immaiwin-go
```

### 2. Environment Configuration
Copy the example environment file and fill in your credentials (especially Schwab API keys if using Schwab features):
```bash
cp .env.example .env
```

### 3. Start Dependencies (Docker)
The app requires MongoDB and Redis. You can start them using Docker Compose:
```bash
docker compose up -d
```
- **MongoDB**: Stores trades, news articles, and Schwab OAuth tokens.
- **Redis**: Handles real-time message broadcasting between workers and the API.

### 4. Setup Backend
Install the necessary Go tools and git hooks:
```bash
make setup
```

### 5. Setup Frontend
Navigate to the UI directory and install dependencies:
```bash
cd internal/ui
pnpm install
cd ../..
```

## Running the Application

To fully run the system, you need to start the API, the UI, and one or more workers.

### Start the API Server
```bash
make run-api
```
The API server will be available at `http://localhost:8080`.

### Start the UI (Development Mode)
```bash
make run-dev-ui
```
The dashboard will be available at `http://localhost:3000`.

### Start Background Workers
You can run specific workers to start collecting data.

**List available workers:**
```bash
make list-workers
```

**Commonly used workers:**
- **Polymarket Watcher**: `make run-worker NAME=polymarket-watcher`
- **MongoDB Writer**: `make run-worker NAME=mongodb-writer` (Required to persist trades from Redis to MongoDB)
- **Al Jazeera Scraper**: `make run-worker NAME=aljazeera-scraper`
- **Bloomberg RSS**: `make run-worker NAME=bloomberg-rss`

## Development

- **Linting**: `make lint` (uses `golangci-lint`)
- **Testing**: `make test`
- **Clean builds**: `make clean`

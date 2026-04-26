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

### 2. Schwab Developer Setup
To use Schwab features, you must have a Schwab Developer account.

1. **Create Account**: Register at [Schwab Developer Portal](https://developer.schwab.com/).
2. **Request API Access**: Ensure your account has access to the **Market Data API** and **Trader API**.
3. **Create an App**: 
   - Go to "Dashboard" -> "Create New App".
   - Name your app (e.g., `immaiwin-dev`).
   - **Register Callback URL**: Set this to `https://127.0.0.1:8080/auth/schwab/callback`. This *must* match your `SCHWAB_CALLBACK_URL` in `.env`.
4. **Retrieve Credentials**: Once approved, copy your **App Key** (`SCHWAB_CLIENT_ID`) and **App Secret** (`SCHWAB_CLIENT_SECRET`) into your `.env`.

### 3. Environment Configuration
Copy the example environment file and fill in your credentials (especially Schwab API keys if using Schwab features):
```bash
cp .env.example .env
```

### 3. Start Dependencies (Docker)
The app requires MongoDB and Redis. You can start them using Docker Compose:
```bash
make docker-compose-up
```
- **MongoDB**: Stores trades, news articles, and Schwab OAuth tokens.
- **Redis**: Handles real-time message broadcasting between workers and the API.

### 4. Setup TLS (Required for Schwab OAuth)
The Schwab API requires an HTTPS callback URL. We use `mkcert` to generate locally-trusted certificates for `127.0.0.1`.

1. **Install mkcert**: Follow the [official instructions](https://github.com/FiloSottile/mkcert#installation).
2. **Install local CA and Generate certificates**:
   ```bash
   make certs
   ```
3. **Configure .env**:
   Update your `.env` to point to the certs and use an `https` callback:
   ```env
   API_TLS_CERT=./.private/certs/localhost.pem
   API_TLS_KEY=./.private/certs/localhost-key.pem
   SCHWAB_CALLBACK_URL=https://127.0.0.1:8080/auth/schwab/callback
   ```

### 5. Setup Frontend and Backend
Install the necessary Go tools and git hooks:
```bash
make setup
```

## Running the Application

To fully run the system, you need to start the API, the UI, and one or more workers.

### Start the API Server
```bash
make api
```
The API server will be available at `https://localhost:8080`.

### Start the UI (Development Mode)
```bash
make dev-ui
```
The dashboard will be available at `http://localhost:3000`.

### Start Background Workers
You can run specific workers to start collecting data.

**List available workers:**
```bash
make list-workers
```

**Commonly used workers:**
- **Polymarket Watcher**: `make worker NAME=polymarket-watcher`
- **MongoDB Writer**: `make worker NAME=mongodb-writer` (Required to persist data from different feeds to MongoDB)
- **Al Jazeera Scraper**: `make worker NAME=aljazeera-scraper`
- **Bloomberg RSS**: `make worker NAME=bloomberg-rss`
- **Schwab Options Watcher**: `make worker NAME=schwab-watcher`
- **Schwab Futures Watcher**: `make worker NAME=schwab-futures-watcher`

## Stopping Application
### Stop Workers
In each worker terminal press ctrl + c.

### Stop Dependencies (UI)
In ui terminal press ctrl + c.

### Stop Dependencies (API)
In api terminal press ctrl + c.

### Stop Dependencies (Docker)
```bash
make docker-compose-down
```

## Development

- **Linting**: `make lint` (uses `golangci-lint`)
- **Testing**: `make test`
- **Clean builds**: `make clean`

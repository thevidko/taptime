# Role and Goal

You are an expert Golang developer. Your task is to build a lightweight, personal Time-Tracking (Attendance) web application. The app must be designed to receive NFC "tap" requests from an iOS Shortcut and provide a responsive web dashboard for a single user.

# Tech Stack Requirements

- **Backend:** Go (Golang 1.22+). Use the standard library `net/http` (utilizing the new routing features in 1.22) or a very lightweight router like `chi`.
- **Database:** SQLite. **CRITICAL:** Use a pure Go implementation of SQLite (e.g., `modernc.org/sqlite` or `github.com/glebarez/go-sqlite`) to ensure `CGO_ENABLED=0` works perfectly and requires no C compiler.
- **Frontend:** Go standard `html/template`, Tailwind CSS (via CDN), and HTMX (via CDN) for dynamic interactions without writing custom JavaScript.
- **Deployment:** Docker (Multi-stage build) and ready for GitHub.

# Database Schema

Create a table `records` with the following columns:

- `id` (INTEGER PRIMARY KEY)
- `date` (TEXT, format YYYY-MM-DD)
- `day_type` (TEXT, e.g., 'work', 'home_office', 'vacation', 'sick')
- `arrival_time` (DATETIME, nullable)
- `departure_time` (DATETIME, nullable)
- `total_minutes` (INTEGER, default 0)

# Core Features & Endpoints

1.  **NFC Tap Endpoint (POST `/api/tap`)**
    - Accepts a simple POST request (JSON or form data) with the current timestamp.
    - **Logic:** Look up today's record.
      - If no record exists for today: Create a new row with `day_type`='work' and set `arrival_time`.
      - If a record exists with an `arrival_time` but NO `departure_time`: Set the `departure_time` and calculate the difference in minutes, saving it to `total_minutes`.
    - Returns a simple HTTP 200 OK.

2.  **Web Dashboard (GET `/`)**
    - Displays records for the current month.
    - Shows a summary card at the top: "Total hours worked this month" (sum of `total_minutes`).
    - Displays a list/table of daily records.
    - **UI/UX:** Use Tailwind CSS to make it responsive. On desktop, display as a standard table. On mobile, display as a stack of cards for readability.

3.  **Manual Entry (POST `/api/manual`)**
    - A form on the dashboard to manually add a day (e.g., Vacation, Home Office).
    - Use HTMX to submit the form and update the table/summary without a full page reload.
    - **Logic:** If `day_type` is 'vacation' or 'sick', automatically set `total_minutes` to 480 (8 hours) so the monthly total remains accurate. But keep it possible to change this value.

# Project Deliverables & Structure

Please generate the following files with production-ready, clean code:

1.  `main.go`: The core server, routing, and database initialization.
2.  `templates/`: Folder containing HTML templates (e.g., `index.html`).
3.  `go.mod` and `go.sum`: Initialize the module (e.g., `module github.com/yourusername/tap-time`).
4.  `Dockerfile`: A multi-stage build. Stage 1: `golang:1.22-alpine` to build the binary (CGO_ENABLED=0). Stage 2: A minimal `alpine` image. **Crucial:** Define a volume (e.g., `/data`) where the SQLite `.db` file will reside so data persists container restarts.
5.  `.gitignore`: Standard Go and SQLite ignores (e.g., `*.db`, `*.db-journal`, vendor/, etc.).
6.  `README.md`: Short instructions on how to build and run the Docker container.

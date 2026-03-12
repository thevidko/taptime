# Tap Time Tracker

A lightweight, personal Time-Tracking (Attendance) web application written in Go.

## Features

- **NFC Tap endpoint:** Send a POST request to `/api/tap` to start or stop a work session.
- **Web Dashboard:** Managed via HTMX and Tailwind CSS.
- **Manual Entry:** Add vacation, sick days, or past work logs from the dashboard.

## Running in Docker

```bash
docker build -t tap-time .

# Run the container with a volume mounted for data persistence
docker run -d \
  -p 8080:8080 \
  -v ./data:/data \
  --name tap-time \
  tap-time
```

## NFC Setup (iOS)

1. Create a new personal automation triggered by an NFC tag.
2. Action: "Get contents of URL".
3. Link: `https://your-server.com/api/tap`.
4. Method: `POST`.

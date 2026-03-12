package main

import (
	"database/sql"
	"html/template"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/glebarez/go-sqlite"
)

type Record struct {
	ID            int
	Date          string
	DayType       string
	ArrivalTime   *string
	DepartureTime *string
	TotalMinutes  int
}

var db *sql.DB
var tmpl *template.Template

func main() {
	var err error
	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "data.db"
	}

	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		log.Fatalf("failed to connect database: %v", err)
	}
	defer db.Close()

	initDB()

	funcs := template.FuncMap{
		"deref": func(s *string) string {
			if s == nil {
				return ""
			}
			return *s
		},
		"div": func(a, b float64) float64 {
			if b == 0 {
				return 0
			}
			return a / b
		},
		"float64": func(i int) float64 {
			return float64(i)
		},
	}

	tmpl, err = template.New("index.html").Funcs(funcs).ParseGlob("templates/*.html")
	if err != nil {
		log.Fatalf("failed to parse templates: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", handleDashboard)
	mux.HandleFunc("POST /api/tap", handleTap)
	mux.HandleFunc("POST /api/manual", handleManual)
	mux.HandleFunc("POST /api/settings", handleSettings)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("Server listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

func initDB() {
	query := `
	CREATE TABLE IF NOT EXISTS records (
		id INTEGER PRIMARY KEY,
		date TEXT,
		day_type TEXT,
		arrival_time DATETIME,
		departure_time DATETIME,
		total_minutes INTEGER DEFAULT 0
	);
	CREATE TABLE IF NOT EXISTS settings (
		key TEXT PRIMARY KEY,
		value TEXT
	);
	`
	_, err := db.Exec(query)
	if err != nil {
		log.Fatalf("failed to initialize db schema: %v", err)
	}
}

func getWorkingDays(year, month int) int {
	t := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC)
	daysInMonth := time.Date(year, time.Month(month+1), 0, 0, 0, 0, 0, time.UTC).Day()
	workingDays := 0
	for i := 0; i < daysInMonth; i++ {
		wd := t.Weekday()
		if wd != time.Saturday && wd != time.Sunday {
			workingDays++
		}
		t = t.AddDate(0, 0, 1)
	}
	return workingDays
}

func handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	monthQuery := r.URL.Query().Get("month")
	now := time.Now()
	var queryTime time.Time
	var err error

	if monthQuery != "" {
		queryTime, err = time.Parse("2006-01", monthQuery)
		if err != nil {
			queryTime = now
		}
	} else {
		queryTime = now
	}

	currentMonth := queryTime.Format("2006-01")
	prevMonth := queryTime.AddDate(0, -1, 0).Format("2006-01")
	nextMonth := queryTime.AddDate(0, 1, 0).Format("2006-01")

	rows, err := db.Query(`
		SELECT id, date, day_type, arrival_time, departure_time, total_minutes
		FROM records
		WHERE date LIKE ?
		ORDER BY date DESC
	`, currentMonth+"-%")

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var records []Record
	totalMonthMinutes := 0

	for rows.Next() {
		var rec Record
		if err := rows.Scan(&rec.ID, &rec.Date, &rec.DayType, &rec.ArrivalTime, &rec.DepartureTime, &rec.TotalMinutes); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		records = append(records, rec)
		totalMonthMinutes += rec.TotalMinutes
	}

	var fte float64 = 1.0
	var fteStr string
	err = db.QueryRow("SELECT value FROM settings WHERE key = 'fte'").Scan(&fteStr)
	if err == nil {
		parsedFte, errFte := strconv.ParseFloat(fteStr, 64)
		if errFte == nil {
			fte = parsedFte
		}
	}

	workingDays := getWorkingDays(queryTime.Year(), int(queryTime.Month()))
	targetHours := float64(workingDays) * 8.0 * fte
	totalHours := float64(totalMonthMinutes) / 60.0

	progressPercent := 0.0
	if targetHours > 0 {
		progressPercent = (totalHours / targetHours) * 100
		if progressPercent > 100 {
			progressPercent = 100
		}
	}

	data := struct {
		Records           []Record
		TotalHours        float64
		TotalMonthMinutes int
		CurrentMonth      string
		PrevMonth         string
		NextMonth         string
		Today             string
		FTE               float64
		TargetHours       float64
		ProgressPercent   float64
	}{
		Records:           records,
		TotalHours:        totalHours,
		TotalMonthMinutes: totalMonthMinutes,
		CurrentMonth:      currentMonth,
		PrevMonth:         prevMonth,
		NextMonth:         nextMonth,
		Today:             now.Format("2006-01-02"),
		FTE:               fte,
		TargetHours:       targetHours,
		ProgressPercent:   progressPercent,
	}

	if err := tmpl.ExecuteTemplate(w, "index.html", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func handleTap(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	today := now.Format("2006-01-02")
	timestampStr := now.Format("2006-01-02 15:04:05")

	var id int
	var arrivalTime *string
	var departureTime *string
	var totalMinutes int

	err := db.QueryRow(`
		SELECT id, arrival_time, departure_time, total_minutes 
		FROM records 
		WHERE date = ?
	`, today).Scan(&id, &arrivalTime, &departureTime, &totalMinutes)

	if err == sql.ErrNoRows {
		_, err := db.Exec(`
			INSERT INTO records (date, day_type, arrival_time) 
			VALUES (?, 'work', ?)
		`, today, timestampStr)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	} else if err == nil {
		if arrivalTime != nil && departureTime == nil {
			// The go-sqlite driver converts DATETIME columns to RFC3339 with a "Z" suffix,
			// treating stored local-time strings as UTC. Normalize to plain "YYYY-MM-DD HH:MM:SS"
			// and parse as local time to get the correct diff.
			arrStr := strings.ReplaceAll(*arrivalTime, "T", " ")
			if strings.HasSuffix(arrStr, "Z") {
				arrStr = arrStr[:len(arrStr)-1]
			}
			if len(arrStr) > 19 {
				arrStr = arrStr[:19]
			}
			arrTime, parseErr := time.ParseInLocation("2006-01-02 15:04:05", arrStr, time.Local)
			if parseErr != nil {
				http.Error(w, "neplatný čas příchodu v db: "+parseErr.Error(), http.StatusInternalServerError)
				return
			}
			diff := now.Sub(arrTime)
			diffMinutes := int(diff.Minutes())
			if diffMinutes < 0 {
				diffMinutes = 0
			} else if diffMinutes == 0 && diff.Seconds() > 0 {
				diffMinutes = 1
			}
			total := totalMinutes + diffMinutes

			_, err := db.Exec(`
				UPDATE records 
				SET departure_time = ?, total_minutes = ? 
				WHERE id = ?
			`, timestampStr, total, id)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		} else if arrivalTime == nil {
			// No arrival yet for today's record — set arrival
			_, err := db.Exec(`
				UPDATE records
				SET arrival_time = ?, departure_time = NULL
				WHERE id = ?
			`, timestampStr, id)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		// else: both arrival and departure are set — shift already complete, ignore tap
	} else {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Úspěšně zaznamenáno (Tap)"))
}

func handleManual(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	date := r.FormValue("date")
	dayType := r.FormValue("day_type")
	minStr := strings.TrimSpace(r.FormValue("total_minutes"))

	if date == "" || dayType == "" {
		http.Error(w, "datum a typ dne jsou povinné", http.StatusBadRequest)
		return
	}

	totalMinutes := 0
	if minStr != "" {
		parsed, err := strconv.Atoi(minStr)
		if err == nil {
			totalMinutes = parsed
		}
	} else {
		if dayType == "vacation" || dayType == "sick" {
			totalMinutes = 480
		}
	}

	var id int
	var existingDeparture *string
	err := db.QueryRow("SELECT id, departure_time FROM records WHERE date = ?", date).Scan(&id, &existingDeparture)
	if err == sql.ErrNoRows {
		_, err = db.Exec(`
			INSERT INTO records (date, day_type, total_minutes)
			VALUES (?, ?, ?)
		`, date, dayType, totalMinutes)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	} else if err == nil {
		// If the record already has a completed tap cycle (departure set), preserve total_minutes from tapping
		if existingDeparture != nil && minStr == "" {
			_, err = db.Exec(`
				UPDATE records
				SET day_type = ?
				WHERE id = ?
			`, dayType, id)
		} else {
			_, err = db.Exec(`
				UPDATE records
				SET day_type = ?, total_minutes = ?
				WHERE id = ?
			`, dayType, totalMinutes, id)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("HX-Redirect", "/")
	w.WriteHeader(http.StatusOK)
}

func handleSettings(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	fte := strings.TrimSpace(r.FormValue("fte"))
	if fte != "" {
		_, err := strconv.ParseFloat(fte, 64)
		if err != nil {
			http.Error(w, "FTE musí být číslo", http.StatusBadRequest)
			return
		}

		_, err = db.Exec(`
			INSERT INTO settings (key, value) VALUES ('fte', ?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value
		`, fte)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("HX-Redirect", "/")
	w.WriteHeader(http.StatusOK)
}

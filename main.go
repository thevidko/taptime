package main

import (
	"database/sql"
	"encoding/csv"
	"fmt"
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
	IsActive      bool
	Note          string
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
	migrateToUTC()
	migrateAddNoteColumn()

	funcs := template.FuncMap{
		"div": func(a, b float64) float64 {
			if b == 0 {
				return 0
			}
			return a / b
		},
		"float64": func(i int) float64 {
			return float64(i)
		},
		// localTime converts a UTC RFC3339 timestamp (as returned by the sqlite driver)
		// to the server's local timezone and returns "HH:MM".
		"localTime": func(s *string) string {
			if s == nil || *s == "" {
				return ""
			}
			t, err := time.Parse(time.RFC3339, *s)
			if err != nil {
				return ""
			}
			return t.In(time.Local).Format("15:04")
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
	mux.HandleFunc("DELETE /api/records/{id}", handleDeleteRecord)
	mux.HandleFunc("POST /api/records/edit", handleEditRecord)
	mux.HandleFunc("GET /api/export/csv", handleExportCSV)

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

// migrateToUTC converts any existing locally-stored timestamps to UTC.
// The go-sqlite driver returns DATETIME values as RFC3339 with "Z" suffix,
// treating whatever is stored as UTC. Before this fix, times were stored as
// local time without timezone info. This migration parses them as local and
// re-saves as actual UTC so the driver can round-trip them correctly.
func migrateToUTC() {
	var done string
	err := db.QueryRow("SELECT value FROM settings WHERE key = 'utc_migration_done'").Scan(&done)
	if err == nil {
		return // already migrated
	}

	rows, err := db.Query("SELECT id, arrival_time, departure_time FROM records")
	if err != nil {
		log.Printf("UTC migration query error: %v", err)
		return
	}

	type row struct {
		id  int
		arr *string
		dep *string
	}
	var records []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.arr, &r.dep); err == nil {
			records = append(records, r)
		}
	}
	rows.Close()

	// The driver returns stored "YYYY-MM-DD HH:MM:SS" (local) as "YYYY-MM-DDTHH:MM:SSZ".
	// Strip T and Z to recover the original local time string, then convert to UTC.
	toUTC := func(s string) string {
		s = strings.ReplaceAll(s, "T", " ")
		s = strings.TrimSuffix(s, "Z")
		if len(s) > 19 {
			s = s[:19]
		}
		t, err := time.ParseInLocation("2006-01-02 15:04:05", s, time.Local)
		if err != nil {
			return ""
		}
		return t.UTC().Format("2006-01-02 15:04:05")
	}

	for _, r := range records {
		var newArr, newDep interface{}
		if r.arr != nil && *r.arr != "" {
			if utc := toUTC(*r.arr); utc != "" {
				newArr = utc
			}
		}
		if r.dep != nil && *r.dep != "" {
			if utc := toUTC(*r.dep); utc != "" {
				newDep = utc
			}
		}
		db.Exec("UPDATE records SET arrival_time = ?, departure_time = ? WHERE id = ?", newArr, newDep, r.id)
	}

	db.Exec("INSERT OR REPLACE INTO settings (key, value) VALUES ('utc_migration_done', '1')")
	log.Printf("UTC migration done: converted %d records", len(records))
}

// applyBreakDeduction subtracts 30 minutes for shifts of 6 hours or more.
func applyBreakDeduction(minutes int) int {
	if minutes >= 360 {
		return minutes - 30
	}
	return minutes
}

// applyCommuteDeduction subtracts the configured commute minutes for 'work' day type.
func applyCommuteDeduction(minutes int, dayType string, deductionMinutes int) int {
	if dayType != "work" || deductionMinutes <= 0 || minutes <= 0 {
		return minutes
	}
	result := minutes - deductionMinutes
	if result < 0 {
		return 0
	}
	return result
}

type AppSettings struct {
	FTE              float64
	WorkDayMinutes   int
	CommuteDeduction int
}

func loadSettings() AppSettings {
	s := AppSettings{
		FTE:              1.0,
		WorkDayMinutes:   480,
		CommuteDeduction: 0,
	}
	rows, err := db.Query("SELECT key, value FROM settings WHERE key IN ('fte', 'work_day_minutes', 'commute_deduction')")
	if err != nil {
		return s
	}
	defer rows.Close()
	for rows.Next() {
		var key, val string
		if err := rows.Scan(&key, &val); err != nil {
			continue
		}
		switch key {
		case "fte":
			if f, err := strconv.ParseFloat(val, 64); err == nil {
				s.FTE = f
			}
		case "work_day_minutes":
			if i, err := strconv.Atoi(val); err == nil {
				s.WorkDayMinutes = i
			}
		case "commute_deduction":
			if i, err := strconv.Atoi(val); err == nil {
				s.CommuteDeduction = i
			}
		}
	}
	return s
}

func migrateAddNoteColumn() {
	var done string
	err := db.QueryRow("SELECT value FROM settings WHERE key = 'note_column_migration_done'").Scan(&done)
	if err == nil {
		return
	}
	db.Exec("ALTER TABLE records ADD COLUMN note TEXT NOT NULL DEFAULT ''")
	db.Exec("INSERT OR REPLACE INTO settings (key, value) VALUES ('note_column_migration_done', '1')")
	log.Printf("Note column migration done")
}

type Holiday struct {
	Date time.Time
	Name string
}

// easterSunday returns Easter Sunday for the given year using the Anonymous Gregorian algorithm.
func easterSunday(year int) time.Time {
	a := year % 19
	b := year / 100
	c := year % 100
	d := b / 4
	e := b % 4
	f := (b + 8) / 25
	g := (b - f + 1) / 3
	h := (19*a + b - d - g + 15) % 30
	i := c / 4
	k := c % 4
	l := (32 + 2*e + 2*i - h - k) % 7
	m := (a + 11*h + 22*l) / 451
	month := (h + l - 7*m + 114) / 31
	day := ((h + l - 7*m + 114) % 31) + 1
	return time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
}

// czechHolidays returns all 13 Czech public holidays for the given year.
func czechHolidays(year int) []Holiday {
	easter := easterSunday(year)
	return []Holiday{
		{time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC), "Nový rok"},
		{easter.AddDate(0, 0, -2), "Velký pátek"},
		{easter.AddDate(0, 0, 1), "Velikonoční pondělí"},
		{time.Date(year, 5, 1, 0, 0, 0, 0, time.UTC), "Svátek práce"},
		{time.Date(year, 5, 8, 0, 0, 0, 0, time.UTC), "Den vítězství"},
		{time.Date(year, 7, 5, 0, 0, 0, 0, time.UTC), "Den slovanských věrozvěstů Cyrila a Metoděje"},
		{time.Date(year, 7, 6, 0, 0, 0, 0, time.UTC), "Den upálení mistra Jana Husa"},
		{time.Date(year, 9, 28, 0, 0, 0, 0, time.UTC), "Den české státnosti"},
		{time.Date(year, 10, 28, 0, 0, 0, 0, time.UTC), "Den vzniku samostatného čs. státu"},
		{time.Date(year, 11, 17, 0, 0, 0, 0, time.UTC), "Den boje za svobodu a demokracii"},
		{time.Date(year, 12, 24, 0, 0, 0, 0, time.UTC), "Štědrý den"},
		{time.Date(year, 12, 25, 0, 0, 0, 0, time.UTC), "1. svátek vánoční"},
		{time.Date(year, 12, 26, 0, 0, 0, 0, time.UTC), "2. svátek vánoční"},
	}
}

// ensureHolidays auto-inserts public holiday records for weekdays in the given month.
// Existing records are never overwritten.
func ensureHolidays(year, month int) {
	holidays := czechHolidays(year)
	s := loadSettings()
	for _, h := range holidays {
		if int(h.Date.Month()) != month {
			continue
		}
		wd := h.Date.Weekday()
		if wd == time.Saturday || wd == time.Sunday {
			continue
		}
		dateStr := h.Date.Format("2006-01-02")
		var id int
		err := db.QueryRow("SELECT id FROM records WHERE date = ?", dateStr).Scan(&id)
		if err == sql.ErrNoRows {
			db.Exec(
				`INSERT INTO records (date, day_type, total_minutes, note) VALUES (?, 'holiday', ?, ?)`,
				dateStr, int(float64(s.WorkDayMinutes)*s.FTE), h.Name,
			)
		}
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

	ensureHolidays(queryTime.Year(), int(queryTime.Month()))

	rows, err := db.Query(`
		SELECT id, date, day_type, arrival_time, departure_time, total_minutes, note
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
		if err := rows.Scan(&rec.ID, &rec.Date, &rec.DayType, &rec.ArrivalTime, &rec.DepartureTime, &rec.TotalMinutes, &rec.Note); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		records = append(records, rec)
		totalMonthMinutes += rec.TotalMinutes
	}

	// Detect active shift (today's record with arrival but no departure)
	today := now.Format("2006-01-02")
	var activeShiftSince string
	var activeShiftArrivalUnix int64
	for i := range records {
		if records[i].Date == today && records[i].ArrivalTime != nil && records[i].DepartureTime == nil {
			records[i].IsActive = true
			t, err := time.Parse(time.RFC3339, *records[i].ArrivalTime)
			if err == nil {
				activeShiftSince = t.In(time.Local).Format("15:04")
				activeShiftArrivalUnix = t.Unix()
			}
			break
		}
	}

	settings := loadSettings()

	workingDays := getWorkingDays(queryTime.Year(), int(queryTime.Month()))
	targetHours := float64(workingDays) * float64(settings.WorkDayMinutes) / 60.0 * settings.FTE
	totalHours := float64(totalMonthMinutes) / 60.0

	progressPercent := 0.0
	if targetHours > 0 {
		progressPercent = (totalHours / targetHours) * 100
		if progressPercent > 100 {
			progressPercent = 100
		}
	}

	data := struct {
		Records                []Record
		TotalHours             float64
		TotalMonthMinutes      int
		CurrentMonth           string
		PrevMonth              string
		NextMonth              string
		Today                  string
		FTE                    float64
		TargetHours            float64
		ProgressPercent        float64
		ActiveShiftSince       string
		ActiveShiftArrivalUnix int64
		WorkDayHours           float64
		CommuteDeduction       int
	}{
		Records:                records,
		TotalHours:             totalHours,
		TotalMonthMinutes:      totalMonthMinutes,
		CurrentMonth:           currentMonth,
		PrevMonth:              prevMonth,
		NextMonth:              nextMonth,
		Today:                  now.Format("2006-01-02"),
		FTE:                    settings.FTE,
		TargetHours:            targetHours,
		ProgressPercent:        progressPercent,
		ActiveShiftSince:       activeShiftSince,
		ActiveShiftArrivalUnix: activeShiftArrivalUnix,
		WorkDayHours:           float64(settings.WorkDayMinutes) / 60.0,
		CommuteDeduction:       settings.CommuteDeduction,
	}

	if err := tmpl.ExecuteTemplate(w, "index.html", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func handleTap(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	today := now.Format("2006-01-02")
	// Store timestamps as UTC so the sqlite driver round-trips them correctly.
	timestampStr := now.UTC().Format("2006-01-02 15:04:05")

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
			// Parse the UTC RFC3339 timestamp returned by the driver.
			arrTime, parseErr := time.Parse(time.RFC3339, *arrivalTime)
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
			s := loadSettings()
			total := applyCommuteDeduction(applyBreakDeduction(totalMinutes+diffMinutes), "work", s.CommuteDeduction)

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
		// else: both set — shift complete, ignore tap
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

	s := loadSettings()
	totalMinutes := 0
	if minStr != "" {
		parsed, err := strconv.Atoi(minStr)
		if err == nil {
			totalMinutes = applyCommuteDeduction(applyBreakDeduction(parsed), dayType, s.CommuteDeduction)
		}
	} else {
		if dayType == "vacation" || dayType == "sick" || dayType == "holiday" {
			totalMinutes = int(float64(s.WorkDayMinutes) * s.FTE)
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
		// Preserve total_minutes from a completed tap cycle unless explicitly overridden.
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

	workDayHoursStr := strings.TrimSpace(r.FormValue("work_day_hours"))
	if workDayHoursStr != "" {
		h, err := strconv.ParseFloat(workDayHoursStr, 64)
		if err != nil || h <= 0 || h > 24 {
			http.Error(w, "Délka pracovní doby musí být číslo (0–24)", http.StatusBadRequest)
			return
		}
		workDayMinutes := int(h * 60)
		_, err = db.Exec(`
			INSERT INTO settings (key, value) VALUES ('work_day_minutes', ?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value
		`, strconv.Itoa(workDayMinutes))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	commuteStr := strings.TrimSpace(r.FormValue("commute_deduction"))
	if commuteStr != "" {
		c, err := strconv.Atoi(commuteStr)
		if err != nil || c < 0 || c > 60 {
			http.Error(w, "Odečet docházky musí být celé číslo (0–60 minut)", http.StatusBadRequest)
			return
		}
		_, err = db.Exec(`
			INSERT INTO settings (key, value) VALUES ('commute_deduction', ?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value
		`, strconv.Itoa(c))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("HX-Redirect", "/")
	w.WriteHeader(http.StatusOK)
}

func handleDeleteRecord(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "neplatné ID", http.StatusBadRequest)
		return
	}

	_, err = db.Exec("DELETE FROM records WHERE id = ?", id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("HX-Redirect", "/")
	w.WriteHeader(http.StatusOK)
}

func handleEditRecord(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	id, err := strconv.Atoi(r.FormValue("id"))
	if err != nil {
		http.Error(w, "neplatné ID", http.StatusBadRequest)
		return
	}

	var date string
	err = db.QueryRow("SELECT date FROM records WHERE id = ?", id).Scan(&date)
	if err != nil {
		http.Error(w, "záznam nenalezen", http.StatusNotFound)
		return
	}

	dayType := r.FormValue("day_type")
	arrivalHHMM := r.FormValue("arrival_time")
	departureHHMM := r.FormValue("departure_time")
	minStr := strings.TrimSpace(r.FormValue("total_minutes"))

	// Build UTC timestamps from the local HH:MM values entered by the user.
	var arrivalUTC, departureUTC interface{}
	var arrT, depT time.Time

	if arrivalHHMM != "" {
		t, err := time.ParseInLocation("2006-01-02 15:04", date+" "+arrivalHHMM, time.Local)
		if err != nil {
			http.Error(w, "neplatný čas příchodu", http.StatusBadRequest)
			return
		}
		arrT = t
		arrivalUTC = t.UTC().Format("2006-01-02 15:04:05")
	}
	if departureHHMM != "" {
		t, err := time.ParseInLocation("2006-01-02 15:04", date+" "+departureHHMM, time.Local)
		if err != nil {
			http.Error(w, "neplatný čas odchodu", http.StatusBadRequest)
			return
		}
		depT = t
		departureUTC = t.UTC().Format("2006-01-02 15:04:05")
	}

	s := loadSettings()
	totalMinutes := 0
	if minStr != "" {
		parsed, _ := strconv.Atoi(minStr)
		totalMinutes = applyCommuteDeduction(applyBreakDeduction(parsed), dayType, s.CommuteDeduction)
	} else if arrivalHHMM != "" && departureHHMM != "" {
		diff := depT.Sub(arrT)
		totalMinutes = int(diff.Minutes())
		if totalMinutes < 0 {
			totalMinutes = 0
		}
		totalMinutes = applyCommuteDeduction(applyBreakDeduction(totalMinutes), dayType, s.CommuteDeduction)
	} else if dayType == "vacation" || dayType == "sick" || dayType == "holiday" {
		totalMinutes = int(float64(s.WorkDayMinutes) * s.FTE)
	}

	_, err = db.Exec(`
		UPDATE records
		SET day_type = ?, arrival_time = ?, departure_time = ?, total_minutes = ?
		WHERE id = ?
	`, dayType, arrivalUTC, departureUTC, totalMinutes, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("HX-Redirect", "/")
	w.WriteHeader(http.StatusOK)
}

func handleExportCSV(w http.ResponseWriter, r *http.Request) {
	monthQuery := r.URL.Query().Get("month")
	now := time.Now()
	var queryTime time.Time

	if monthQuery != "" {
		t, err := time.Parse("2006-01", monthQuery)
		if err != nil {
			queryTime = now
		} else {
			queryTime = t
		}
	} else {
		queryTime = now
	}

	currentMonth := queryTime.Format("2006-01")

	rows, err := db.Query(`
		SELECT date, day_type, arrival_time, departure_time, total_minutes
		FROM records
		WHERE date LIKE ?
		ORDER BY date ASC
	`, currentMonth+"-%")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="taptime-%s.csv"`, currentMonth))

	writer := csv.NewWriter(w)
	writer.Write([]string{"Datum", "Typ", "Příchod", "Odchod", "Hodiny", "Minuty"})

	for rows.Next() {
		var date, dayType string
		var arrivalTime, departureTime *string
		var totalMinutes int

		if err := rows.Scan(&date, &dayType, &arrivalTime, &departureTime, &totalMinutes); err != nil {
			continue
		}

		arrStr := ""
		if arrivalTime != nil {
			if t, err := time.Parse(time.RFC3339, *arrivalTime); err == nil {
				arrStr = t.In(time.Local).Format("15:04")
			}
		}
		depStr := ""
		if departureTime != nil {
			if t, err := time.Parse(time.RFC3339, *departureTime); err == nil {
				depStr = t.In(time.Local).Format("15:04")
			}
		}

		hours := fmt.Sprintf("%.2f", float64(totalMinutes)/60.0)
		writer.Write([]string{date, dayType, arrStr, depStr, hours, strconv.Itoa(totalMinutes)})
	}

	writer.Flush()
}

package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// ─────────────────────────────────────────
// DATA STRUCTURES
// ─────────────────────────────────────────

type StoryRequest struct {
	Story string `json:"story"`
}

type VFT struct {
	Entity      string `json:"entity"`
	Agreement   string `json:"agreement"`
	PaymentMade string `json:"payment_made"`
	Performance string `json:"performance"`
	HasReceipt  string `json:"has_receipt"`
	NoticeSent  string `json:"notice_sent"`
}

type LawoneNode struct {
	Node        string  `json:"node"`
	Requirement string  `json:"requirement"`
	Status      string  `json:"status"`
	Score       float64 `json:"score"`
}

type LawoneResponse struct {
	LawoneScore     float64      `json:"lawone_score"`
	Strength        string       `json:"strength"`
	Nodes           []LawoneNode `json:"nodes"`
	MissingQuestion string       `json:"missing_question"`
	VFT             *VFT         `json:"vft"`
}

// ─────────────────────────────────────────
// FIX 1 — GLOBAL CORS MIDDLEWARE
// Wraps the *entire* mux so headers are
// written before ANY handler code runs.
// A panicking handler can no longer strip them.
// ─────────────────────────────────────────

func globalCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Always write CORS headers first, unconditionally.
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		// Pre-flight — browser sends this before the real POST.
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// ─────────────────────────────────────────
// FIX 2 — PANIC RECOVERY MIDDLEWARE
// If any handler panics the goroutine still
// returns a proper JSON error (not a bare
// TCP close which the browser blames on CORS).
// ─────────────────────────────────────────

func recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("PANIC recovered: %v", rec)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(`{"error":"internal server error"}`))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ─────────────────────────────────────────
// GEMINI CALL
// FIX 3 — safe nil checks so a bad Gemini
// response can never cause a nil-pointer panic.
// ─────────────────────────────────────────

func callGemini(story string) (*VFT, error) {
	apiKey := os.Getenv("AIzaSyCb_UE-qzF2ZCDHtynGOkxGQiYUWeldp14")
	if apiKey == "" {
		return nil, fmt.Errorf("GEMINI_API_KEY not set")
	}

	url := fmt.Sprintf(
		"https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent?key=%s",
		apiKey,
	)

	prompt := `Extract legal facts from the story below and return ONLY a valid JSON object with these exact keys:
{
  "entity": "name of the other party or Unknown",
  "agreement": "brief description of the agreement or None",
  "payment_made": "amount paid or None",
  "performance": "Completed / Partial / Not started",
  "has_receipt": "Yes / No / Unknown",
  "notice_sent": "Yes / No"
}
Return ONLY the JSON object. No markdown, no explanation.

Story:
` + story

	body := map[string]interface{}{
		"contents": []map[string]interface{}{
			{
				"parts": []map[string]string{
					{"text": prompt},
				},
			},
		},
	}

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	for attempt := 1; attempt <= 3; attempt++ {
		resp, err := http.Post(url, "application/json", bytes.NewBuffer(bodyBytes))
		if err != nil {
			log.Printf("Gemini attempt %d — HTTP error: %v", attempt, err)
			time.Sleep(time.Duration(attempt) * time.Second)
			continue
		}

		respBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			log.Printf("Gemini attempt %d — status %d: %s", attempt, resp.StatusCode, string(respBytes))
			time.Sleep(time.Duration(attempt) * time.Second)
			continue
		}

		// ── Safe navigation (no type assertions that can panic) ──
		var geminiResp map[string]interface{}
		if err := json.Unmarshal(respBytes, &geminiResp); err != nil {
			log.Printf("Gemini attempt %d — unmarshal error: %v", attempt, err)
			time.Sleep(time.Duration(attempt) * time.Second)
			continue
		}

		rawText, err := extractGeminiText(geminiResp)
		if err != nil {
			log.Printf("Gemini attempt %d — extract error: %v", attempt, err)
			time.Sleep(time.Duration(attempt) * time.Second)
			continue
		}

		// Strip markdown fences if Gemini added them anyway.
		rawText = strings.TrimSpace(rawText)
		rawText = strings.TrimPrefix(rawText, "```json")
		rawText = strings.TrimPrefix(rawText, "```")
		rawText = strings.TrimSuffix(rawText, "```")
		rawText = strings.TrimSpace(rawText)

		var vft VFT
		if err := json.Unmarshal([]byte(rawText), &vft); err != nil {
			log.Printf("Gemini attempt %d — VFT parse error: %v\nRaw: %s", attempt, err, rawText)
			time.Sleep(time.Duration(attempt) * time.Second)
			continue
		}

		return &vft, nil
	}

	return nil, fmt.Errorf("Gemini failed after 3 attempts")
}

// extractGeminiText safely walks the Gemini response without panicking.
func extractGeminiText(resp map[string]interface{}) (string, error) {
	candidates, ok := resp["candidates"].([]interface{})
	if !ok || len(candidates) == 0 {
		return "", fmt.Errorf("no candidates in response")
	}

	candidate, ok := candidates[0].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("invalid candidate format")
	}

	content, ok := candidate["content"].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("no content in candidate")
	}

	parts, ok := content["parts"].([]interface{})
	if !ok || len(parts) == 0 {
		return "", fmt.Errorf("no parts in content")
	}

	part, ok := parts[0].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("invalid part format")
	}

	text, ok := part["text"].(string)
	if !ok {
		return "", fmt.Errorf("no text in part")
	}

	return text, nil
}

// ─────────────────────────────────────────
// SCORING LOGIC
// ─────────────────────────────────────────

func buildLawoneScore(vft *VFT) LawoneResponse {
	var nodes []LawoneNode
	total := 0.0
	missingQ := ""

	if vft.Entity != "" && vft.Entity != "Unknown" {
		total += 0.25
		nodes = append(nodes, LawoneNode{"Node A", "Identity", "Verified", 0.25})
	} else {
		missingQ = "Who is the other party?"
		nodes = append(nodes, LawoneNode{"Node A", "Identity", "Pending", 0})
	}

	if vft.PaymentMade != "" && vft.PaymentMade != "None" {
		total += 0.25
		nodes = append(nodes, LawoneNode{"Node B", "Payment", "Verified", 0.25})
	} else {
		nodes = append(nodes, LawoneNode{"Node B", "Payment", "Pending", 0})
	}

	if vft.Performance == "Partial" || vft.Performance == "Not started" {
		total += 0.25
		nodes = append(nodes, LawoneNode{"Node C", "Breach", "Verified", 0.25})
	} else {
		nodes = append(nodes, LawoneNode{"Node C", "Breach", "Pending", 0})
	}

	if vft.NoticeSent == "Yes" {
		total += 0.25
		nodes = append(nodes, LawoneNode{"Node D", "Notice", "Verified", 0.25})
	} else {
		nodes = append(nodes, LawoneNode{"Node D", "Notice", "Pending", 0})
	}

	strength := "Weak"
	if total >= 0.75 {
		strength = "Strong"
	} else if total >= 0.5 {
		strength = "Medium"
	}

	return LawoneResponse{
		LawoneScore:     total,
		Strength:        strength,
		Nodes:           nodes,
		MissingQuestion: missingQ,
		VFT:             vft,
	}
}

// ─────────────────────────────────────────
// DB
// ─────────────────────────────────────────

func initDB(db *sql.DB) {
	db.Exec(`CREATE TABLE IF NOT EXISTS cases (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		story TEXT,
		score REAL
	)`)
}

// ─────────────────────────────────────────
// HANDLERS
// ─────────────────────────────────────────

func analyzeHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		var req StoryRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Story) == "" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"invalid or empty request"}`))
			return
		}

		vft, err := callGemini(req.Story)
		if err != nil {
			log.Printf("Gemini error: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":"AI service failed — check GEMINI_API_KEY on Render"}`))
			return
		}

		result := buildLawoneScore(vft)
		db.Exec(`INSERT INTO cases (story, score) VALUES (?, ?)`, req.Story, result.LawoneScore)
		json.NewEncoder(w).Encode(result)
	}
}

// ─────────────────────────────────────────
// MAIN
// ─────────────────────────────────────────

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	db, err := sql.Open("sqlite", "./lawone.db")
	if err != nil {
		log.Fatal(err)
	}
	initDB(db)

	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("LAWONE API Running 🚀"))
	})

	mux.Handle("/analyze", analyzeHandler(db))

	// Stack: globalCORS → recoveryMiddleware → mux
	// CORS headers are set first, before any handler code can fail.
	handler := globalCORS(recoveryMiddleware(mux))

	log.Printf("🚀 Running on port %s", port)
	log.Fatal(http.ListenAndServe(":"+port, handler))
}
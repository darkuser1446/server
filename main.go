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
// CORS MIDDLEWARE (FIXED)
// ─────────────────────────────────────────

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// ─────────────────────────────────────────
// GEMINI CALL
// ─────────────────────────────────────────

func callGemini(story string) (*VFT, error) {
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		apiKey = "AIzaSyCb_UE-qzF2ZCDHtynGOkxGQiYUWeldp14"
	}

	url := fmt.Sprintf(
		"https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent?key=%s",
		apiKey,
	)

	body := map[string]interface{}{
		"contents": []map[string]interface{}{
			{
				"parts": []map[string]string{
					{"text": "Extract legal facts as JSON.\n\nStory:\n" + story},
				},
			},
		},
	}

	bodyBytes, _ := json.Marshal(body)

	for attempt := 1; attempt <= 2; attempt++ {
		resp, err := http.Post(url, "application/json", bytes.NewBuffer(bodyBytes))
		if err != nil {
			return nil, err
		}

		respBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var geminiResp map[string]interface{}
		json.Unmarshal(respBytes, &geminiResp)

		candidates := geminiResp["candidates"].([]interface{})
		content := candidates[0].(map[string]interface{})["content"].(map[string]interface{})
		parts := content["parts"].([]interface{})
		rawText := parts[0].(map[string]interface{})["text"].(string)

		rawText = strings.Trim(rawText, "` \n")

		var vft VFT
		err = json.Unmarshal([]byte(rawText), &vft)
		if err == nil {
			return &vft, nil
		}

		time.Sleep(1 * time.Second)
	}

	return nil, fmt.Errorf("Gemini failed")
}

// ─────────────────────────────────────────
// LOGIC
// ─────────────────────────────────────────

func buildLawoneScore(vft *VFT) LawoneResponse {
	nodes := []LawoneNode{}
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
// HANDLER
// ─────────────────────────────────────────

func analyze(db *sql.DB) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		w.Header().Set("Content-Type", "application/json")

		var req StoryRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid request"}`, 400)
			return
		}

		vft, err := callGemini(req.Story)
		if err != nil {
			http.Error(w, `{"error":"AI failed"}`, 500)
			return
		}

		result := buildLawoneScore(vft)

		db.Exec(`INSERT INTO cases (story, score) VALUES (?, ?)`, req.Story, result.LawoneScore)

		json.NewEncoder(w).Encode(result)
	})
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

	http.Handle("/", corsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("API Running 🚀"))
	})))

	http.Handle("/analyze", corsMiddleware(analyze(db)))

	log.Println("🚀 Running on port", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
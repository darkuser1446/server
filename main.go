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

	systemPrompt := `You are a legal fact extractor.
Return ONLY valid JSON.`

	body := map[string]interface{}{
		"contents": []map[string]interface{}{
			{
				"parts": []map[string]string{
					{"text": systemPrompt + "\n\nStory:\n" + story},
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

	if vft.Entity != "" && vft.Entity != "Unknown" {
		total += 0.25
		nodes = append(nodes, LawoneNode{"Node A", "Identity", "Verified", 0.25})
	}

	if vft.PaymentMade != "" && vft.PaymentMade != "None" {
		total += 0.25
		nodes = append(nodes, LawoneNode{"Node B", "Payment", "Verified", 0.25})
	}

	if vft.Performance == "Partial" || vft.Performance == "Not started" {
		total += 0.25
		nodes = append(nodes, LawoneNode{"Node C", "Breach", "Verified", 0.25})
	}

	if vft.NoticeSent == "Yes" {
		total += 0.25
		nodes = append(nodes, LawoneNode{"Node D", "Notice", "Verified", 0.25})
	}

	strength := "Weak"
	if total >= 0.75 {
		strength = "Strong"
	} else if total >= 0.5 {
		strength = "Medium"
	}

	return LawoneResponse{
		LawoneScore: total,
		Strength:    strength,
		Nodes:       nodes,
		VFT:         vft,
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

func cors(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")

		if r.Method == "OPTIONS" {
			return
		}
		h(w, r)
	}
}

func analyze(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		var req StoryRequest
		json.NewDecoder(r.Body).Decode(&req)

		vft, err := callGemini(req.Story)
		if err != nil {
			http.Error(w, err.Error(), 500)
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

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("API Running 🚀"))
	})

	http.HandleFunc("/analyze", cors(analyze(db)))

	log.Println("Running on port", port)
	http.ListenAndServe(":"+port, nil)
}
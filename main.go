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

	"github.com/joho/godotenv"
	_ "modernc.org/sqlite"
	tele "gopkg.in/telebot.v3"
)


const DAILY_BUDGET =  2200


var db *sql.DB
var groqKey string


type ExpenseData struct {
	Amount      float64 `json:"amount"`
	Category    string  `json:"category"`
	Description string  `json:"description"`
}

func main() {

	err := godotenv.Load()
	if err != nil {
		log.Fatal("Error loading .env file")
	}
	groqKey = os.Getenv("GROQ_API_KEY")
	token := os.Getenv("TELEGRAM_TOKEN")


	db, err = sql.Open("sqlite", "finance.db")
	if err != nil {
		log.Fatal("Database Error:", err)
	}

	db.Exec(`CREATE TABLE IF NOT EXISTS expenses (
		id INTEGER PRIMARY KEY AUTOINCREMENT, 
		amount REAL, 
		category TEXT, 
		description TEXT, 
		date TEXT
	)`)
	db.Exec(`CREATE TABLE IF NOT EXISTS savings (
		id INTEGER PRIMARY KEY AUTOINCREMENT, 
		amount REAL, 
		date TEXT
	)`)


	pref := tele.Settings{
		Token:  token,
		Poller: &tele.LongPoller{Timeout: 10 * time.Second},
	}
	b, err := tele.NewBot(pref)
	if err != nil {
		log.Fatal("Telegram Bot Error:", err)
	}



	b.Handle("/start", func(c tele.Context) error {
		return c.Send(fmt.Sprintf("🤖 AI Finance Bot ready!\n\nYour daily budget is set to ₦%.2f.\nSend me an expense like '15 lunch' or use /daily to calculate savings.", DAILY_BUDGET))
	})


	b.Handle("/daily", func(c tele.Context) error {
		var totalSpent float64

		err := db.QueryRow("SELECT COALESCE(SUM(amount), 0) FROM expenses WHERE date(date) = date('now')").Scan(&totalSpent)
		if err != nil {
			return c.Send("❌ Database error calculating daily total.")
		}

		savings := DAILY_BUDGET - totalSpent
		if savings > 0 {

			_, err = db.Exec("INSERT INTO savings (amount, date) VALUES (?, datetime('now'))", savings)
			if err != nil {
				return c.Send("❌ Database error saving funds.")
			}
			return c.Send(fmt.Sprintf("🎉 **Day Complete!**\nSpent: ₦%.2f\nSaved: ₦%.2f\n\nI've moved the ₦%.2f to your savings ledger!", totalSpent, savings, savings))
		} else {
			return c.Send(fmt.Sprintf("📉 **Day Complete!**\nYou spent ₦%.2f, which is over your ₦%.2f budget. No savings today, but tomorrow is a new day!", totalSpent, DAILY_BUDGET))
		}
	})


	b.Handle(tele.OnText, func(c tele.Context) error {
		msg := c.Text()
		if strings.HasPrefix(msg, "/") {
			return nil
		}

		// 1. Send message to AI
		data, err := parseExpenseWithAI(msg)
		if err != nil {
			return c.Send("❌ Couldn't parse that. Please format like: '20 groceries' or '15 lunch'.")
		}

		// 2. Save to Database
		_, err = db.Exec("INSERT INTO expenses (amount, category, description, date) VALUES (?, ?, ?, datetime('now'))",
			data.Amount, data.Category, data.Description)
		if err != nil {
			return c.Send("❌ Database error saving expense.")
		}

		// 3. Reply to user
		return c.Send(fmt.Sprintf("✅ Logged: ₦%.2f for %s\n(%s)", data.Amount, data.Category, data.Description))
	})

	fmt.Println("🚀 Go Finance Bot is running...")
	b.Start()
}

// --- AI INTEGRATION ---
func parseExpenseWithAI(message string) (ExpenseData, error) {
	// Construct the payload for Groq API
	payload := map[string]interface{}{
		"model": "qwen/qwen3.8-27b",
		"messages": []map[string]string{
			{
				"role":    "user",
				"content": fmt.Sprintf("Extract expense details from: \"%s\". Return ONLY valid JSON: {\"amount\": float, \"category\": \"string\", \"description\": \"string\"}.", message),
			},
		},
		"response_format": map[string]string{"type": "json_object"},
		"temperature":     0.1,
	}

	jsonPayload, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", "https://api.groq.com/openai/v1/chat/completions", bytes.NewBuffer(jsonPayload))
	req.Header.Set("Authorization", "Bearer "+groqKey)
	req.Header.Set("Content-Type", "application/json")

	// Execute HTTP Request
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return ExpenseData{}, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	// Parse Groq API response structure
	var groqResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &groqResp); err != nil {
		return ExpenseData{}, err
	}

	// Clean up the AI's JSON (remove markdown blocks if it adds them)
	cleanJson := strings.ReplaceAll(groqResp.Choices[0].Message.Content, "```json", "")
	cleanJson = strings.ReplaceAll(cleanJson, "```", "")
	cleanJson = strings.TrimSpace(cleanJson)

	// Unmarshal into our Go Struct
	var data ExpenseData
	err = json.Unmarshal([]byte(cleanJson), &data)
	return data, err
}
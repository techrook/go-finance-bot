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
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
	_ "modernc.org/sqlite"
	tele "gopkg.in/telebot.v3"
)


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

	db.Exec(`CREATE TABLE IF NOT EXISTS earnings (
		id INTEGER PRIMARY KEY AUTOINCREMENT, 
		amount REAL,
		period Text, 
		date TEXT
	)`)

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

		db.Exec(`CREATE TABLE IF NOT EXISTS bot_state (
			key TEXT PRIMARY KEY,
			value TEXT,
			updated_at TEXT
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
			var incomeAmount float64
			var incomePeriod string
			

			err := db.QueryRow("SELECT amount, period FROM earnings ORDER BY id DESC LIMIT 1").Scan(&incomeAmount, &incomePeriod)
			
			if err != nil || incomeAmount == 0 {

				return c.Send("🤖 **Welcome to your AI Finance Bot!**\n\nTo get started, please set your income and how often you receive it.\n\n👉 Example: `/setincome 100000 monthly` or `/setincome 25000 weekly`\n\nOnce set, you can start logging expenses like '1500 lunch'.")
			}
			

			msg := fmt.Sprintf("🤖 **Welcome back!**\n\n💰 Income: ₦%.2f (%s)\n📉 Send me an expense like '1500 lunch' or use /daily to calculate savings.", incomeAmount, incomePeriod)
			return c.Send(msg)
		})
	
		b.Handle("/setincome", func(c tele.Context) error {
			args := c.Args() 
			
			if len(args) < 2 {
				return c.Send("❌ Please provide both amount and period.\n👉 Example: `/setincome 100000 monthly`")
			}
			
			period := strings.ToLower(args[1])
			amount, err := strconv.ParseFloat(args[0], 64)
			if err !=nil {
				return c.Send("❌ Please provide a valid number for the amout")
			}
			validPeriods := []string{"daily",  "weekly", "monthly", "yearly"}
			isValid := false
			for _, p := range validPeriods {
				if period == p {
					isValid = true
					break
				}
			}
			
			if !isValid {
				return c.Send("❌ Invalid period. Please use: daily, weekly, monthly, or yearly.")
			}
			
			_, err = db.Exec("INSERT INTO earnings (amount, period, date) VALUES (?, ?, datetime('now'))", amount, period)
			if err != nil {
				return c.Send("❌ Database error saving income.")
			}
			
			return c.Send(fmt.Sprintf("✅ Income successfully set to ₦%.2f %s!\n\nYou can now start logging expenses.", amount, period))
		})


	


	b.Handle(tele.OnText, func(c tele.Context) error {
		msg := c.Text()
		if strings.HasPrefix(msg, "/") {
			return nil
		}


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


func parseExpenseWithAI(message string) (ExpenseData, error) {

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


	cleanJson := strings.ReplaceAll(groqResp.Choices[0].Message.Content, "```json", "")
	cleanJson = strings.ReplaceAll(cleanJson, "```", "")
	cleanJson = strings.TrimSpace(cleanJson)


	var data ExpenseData
	err = json.Unmarshal([]byte(cleanJson), &data)
	return data, err
}
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
	// 1. Load Environment Variables
	err := godotenv.Load()
	if err != nil {
		log.Fatal("Error loading .env file")
	}
	groqKey = os.Getenv("GROQ_API_KEY")
	token := os.Getenv("TELEGRAM_TOKEN")

	// 2. Initialize SQLite Database
	db, err = sql.Open("sqlite", "finance.db")
	if err != nil {
		log.Fatal("Database Error:", err)
	}

	// 3. Create Tables (Multi-user ready)
	db.Exec(`CREATE TABLE IF NOT EXISTS users (
		user_id INTEGER PRIMARY KEY,
		username TEXT,
		first_name TEXT,
		first_seen TEXT
	)`)

	db.Exec(`CREATE TABLE IF NOT EXISTS earnings (
		id INTEGER PRIMARY KEY AUTOINCREMENT, 
		user_id INTEGER,
		amount REAL,
		period TEXT, 
		date TEXT
	)`)

	db.Exec(`CREATE TABLE IF NOT EXISTS expenses (
		id INTEGER PRIMARY KEY AUTOINCREMENT, 
		user_id INTEGER,
		amount REAL, 
		category TEXT, 
		description TEXT, 
		date TEXT
	)`)

	db.Exec(`CREATE TABLE IF NOT EXISTS savings (
		id INTEGER PRIMARY KEY AUTOINCREMENT, 
		user_id INTEGER,
		amount REAL, 
		date TEXT
	)`)

	db.Exec(`CREATE TABLE IF NOT EXISTS bot_state (
		user_id INTEGER,
		key TEXT,
		value TEXT,
		updated_at TEXT,
		PRIMARY KEY (user_id, key)
	)`)

	// 4. Initialize Telegram Bot
	pref := tele.Settings{
		Token:  token,
		Poller: &tele.LongPoller{Timeout: 10 * time.Second},
	}
	b, err := tele.NewBot(pref)
	if err != nil {
		log.Fatal("Telegram Bot Error:", err)
	}

	// 5. BACKGROUND GOROUTINE: Multi-User Periodic Summary Checker
	go func() {
		loc, _ := time.LoadLocation("Africa/Lagos")
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()

		for range ticker.C {
			now := time.Now().In(loc)
			today := now.Format("2006-01-02")
			thisMonth := now.Format("2006-01")

			// Check if it's midnight (00:00)
			if now.Hour() == 0 && now.Minute() == 0 {
				rows, err := db.Query("SELECT user_id FROM users")
				if err == nil {
					for rows.Next() {
						var userID int64
						rows.Scan(&userID)
						
						lastDaily := getLastSummaryDate(userID, "last_daily_summary")
						if lastDaily != today {
							sendDailySummary(b, userID)
						}
					}
					rows.Close()
				}
			}

			// Check if it's 1st of the month at midnight
			if now.Day() == 1 && now.Hour() == 0 && now.Minute() == 0 {
				rows, err := db.Query("SELECT user_id FROM users")
				if err == nil {
					for rows.Next() {
						var userID int64
						rows.Scan(&userID)
						
						lastMonthly := getLastSummaryDate(userID, "last_monthly_summary")
						if lastMonthly != thisMonth {
							sendMonthlySummary(b, userID)
						}
					}
					rows.Close()
				}
			}
		}
	}()

	// 6. BOT HANDLERS

	// /start command
	b.Handle("/start", func(c tele.Context) error {
		user := c.Sender()
		userID := user.ID

		// Register or update the user in our database
		db.Exec(`INSERT OR IGNORE INTO users (user_id, username, first_name, first_seen) 
		         VALUES (?, ?, ?, datetime('now'))`, userID, user.Username, user.FirstName)

		// Lazy evaluation: Check if summaries need to be sent for THIS user
		checkAndSendSummaries(b, userID)

		var incomeAmount float64
		var incomePeriod string

		// Query ONLY this user's income
		err := db.QueryRow("SELECT amount, period FROM earnings WHERE user_id = ? ORDER BY id DESC LIMIT 1", userID).Scan(&incomeAmount, &incomePeriod)
		
		if err != nil || incomeAmount == 0 {
			return c.Send("🤖 **Welcome to your AI Finance Bot!**\n\nTo get started, please set your income and how often you receive it.\n\n👉 Example: `/setincome 100000 monthly`\n\nOnce set, you can start logging expenses like '1500 lunch'.")
		}

		msg := fmt.Sprintf("🤖 **Welcome back, %s!**\n\n💰 Income: ₦%.2f (%s)\n\nSend me an expense like '1500 lunch' or use /daily to calculate savings.", user.FirstName, incomeAmount, incomePeriod)
		return c.Send(msg)
	})

	// /setincome command
	b.Handle("/setincome", func(c tele.Context) error {
		userID := c.Sender().ID
		args := c.Args() 
		
		if len(args) < 2 {
			return c.Send("❌ Please provide both amount and period.\n👉 Example: `/setincome 100000 monthly`")
		}
		
		period := strings.ToLower(args[1])
		amount, err := strconv.ParseFloat(args[0], 64)
		if err != nil {
			return c.Send("❌ Please provide a valid number for the amount.")
		}
		
		validPeriods := []string{"daily", "weekly", "monthly", "yearly"}
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
		
		// Save with user_id
		_, err = db.Exec("INSERT INTO earnings (user_id, amount, period, date) VALUES (?, ?, ?, datetime('now'))", userID, amount, period)
		if err != nil {
			return c.Send("❌ Database error saving income.")
		}
		
		return c.Send(fmt.Sprintf("✅ Income successfully set to ₦%.2f (%s)!\n\nYou can now start logging expenses.", amount, period))
	})

	// Handle all regular text messages (The AI Logic)
	b.Handle(tele.OnText, func(c tele.Context) error {
		userID := c.Sender().ID
		msg := c.Text()
		if strings.HasPrefix(msg, "/") {
			return nil
		}

		data, err := parseExpenseWithAI(msg)
		if err != nil {
			return c.Send("❌ Couldn't parse that. Please format like: '500 groceries' or '1500 lunch'.")
		}

		// Save to Database with user_id
		_, err = db.Exec("INSERT INTO expenses (user_id, amount, category, description, date) VALUES (?, ?, ?, ?, datetime('now'))",
			userID, data.Amount, data.Category, data.Description)
		if err != nil {
			return c.Send("❌ Database error saving expense.")
		}

		return c.Send(fmt.Sprintf("✅ Logged: ₦%.2f for %s\n(%s)", data.Amount, data.Category, data.Description))
	})

	fmt.Println("🚀 Go Finance Bot is running...")
	b.Start()
}

// --- AI INTEGRATION ---
func parseExpenseWithAI(message string) (ExpenseData, error) {
	payload := map[string]interface{}{
		"model": "qwen/qwen3.8-27b",
		"messages": []map[string]string{
			{
				"role":    "user",
				"content": fmt.Sprintf("Extract expense details from: \"%s\". The currency is Nigerian Naira. Return ONLY valid JSON: {\"amount\": float, \"category\": \"string\", \"description\": \"string\"}.", message),
			},
		},
		"response_format": map[string]string{"type": "json_object"},
		"temperature":     0.1,
	}

	jsonPayload, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", "https://api.groq.com/openai/v1/chat/completions", bytes.NewBuffer(jsonPayload))
	req.Header.Set("Authorization", "Bearer "+groqKey)
	req.Header.Set("Content-Type", "application/json")

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

// --- HELPER: Get last summary date from database ---
func getLastSummaryDate(userID int64, key string) string {
	var lastDate string
	err := db.QueryRow("SELECT value FROM bot_state WHERE user_id = ? AND key = ?", userID, key).Scan(&lastDate)
	if err != nil {
		return "" // Not found
	}
	return lastDate
}

// --- HELPER: Update last summary date in database ---
func updateLastSummaryDate(userID int64, key string, date string) {
	db.Exec("INSERT OR REPLACE INTO bot_state (user_id, key, value, updated_at) VALUES (?, ?, ?, datetime('now'))", userID, key, date)
}

// --- HELPER: Generate and send daily summary ---
func sendDailySummary(b *tele.Bot, userID int64) {
	loc, _ := time.LoadLocation("Africa/Lagos")
	today := time.Now().In(loc).Format("2006-01-02")

	// Query ONLY this user's expenses
	rows, err := db.Query(`
		SELECT amount, category, description 
		FROM expenses 
		WHERE user_id = ? AND date(date) = ?
		ORDER BY date DESC
	`, userID, today)
	if err != nil {
		return
	}
	defer rows.Close()

	var totalSpent float64
	var expenses []string
	for rows.Next() {
		var amount float64
		var category, description string
		rows.Scan(&amount, &category, &description)
		totalSpent += amount
		expenses = append(expenses, fmt.Sprintf("• ₦%.2f - %s (%s)", amount, category, description))
	}

	message := fmt.Sprintf("📊 **Daily Summary for %s**\n\n", today)
	if len(expenses) == 0 {
		message += "No expenses logged today! 🎉"
	} else {
		message += fmt.Sprintf("**Total Spent:** ₦%.2f\n\n", totalSpent)
		message += "**Breakdown:**\n" + strings.Join(expenses, "\n")
	}

	// Send to user (Ignore error if user blocked the bot)
	b.Send(&tele.User{ID: userID}, message)
	updateLastSummaryDate(userID, "last_daily_summary", today)
}

// --- HELPER: Generate and send monthly summary ---
func sendMonthlySummary(b *tele.Bot, userID int64) {
	loc, _ := time.LoadLocation("Africa/Lagos")
	now := time.Now().In(loc)
	month := now.Format("2006-01")
	monthName := now.Format("January 2006")

	// Query ONLY this user's expenses
	rows, err := db.Query(`
		SELECT amount, category, description, date
		FROM expenses 
		WHERE user_id = ? AND strftime('%Y-%m', date) = ?
		ORDER BY date DESC
	`, userID, month)
	if err != nil {
		return
	}
	defer rows.Close()

	var totalSpent float64
	categoryTotals := make(map[string]float64)
	var expenseCount int

	for rows.Next() {
		var amount float64
		var category, description, date string
		rows.Scan(&amount, &category, &description, &date)
		totalSpent += amount
		categoryTotals[category] += amount
		expenseCount++
	}

	message := fmt.Sprintf("📊 **Monthly Summary for %s**\n\n", monthName)
	message += fmt.Sprintf("**Total Spent:** ₦%.2f\n", totalSpent)
	message += fmt.Sprintf("**Number of Transactions:** %d\n\n", expenseCount)

	if len(categoryTotals) > 0 {
		message += "**Breakdown by Category:**\n"
		for category, total := range categoryTotals {
			percentage := (total / totalSpent) * 100
			message += fmt.Sprintf("• %s: ₦%.2f (%.1f%%)\n", category, total, percentage)
		}
	} else {
		message += "No expenses logged this month!"
	}

	b.Send(&tele.User{ID: userID}, message)
	updateLastSummaryDate(userID, "last_monthly_summary", month)
}

// --- HELPER: Check if summary should be sent (lazy evaluation) ---
func checkAndSendSummaries(b *tele.Bot, userID int64) {
	loc, _ := time.LoadLocation("Africa/Lagos")
	now := time.Now().In(loc)
	today := now.Format("2006-01-02")
	thisMonth := now.Format("2006-01")

	lastDaily := getLastSummaryDate(userID, "last_daily_summary")
	if lastDaily != today {
		sendDailySummary(b, userID)
	}

	if now.Day() == 1 {
		lastMonthly := getLastSummaryDate(userID, "last_monthly_summary")
		if lastMonthly != thisMonth {
			sendMonthlySummary(b, userID)
		}
	}
}
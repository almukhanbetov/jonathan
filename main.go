package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

// --- MODELS ---
type Game struct {
	GameID     string
	Sport      string
	Bookmaker  string
	Source     string
	League     string
	Home       string
	Away       string
	Scores     string
	TimeStatus string
	StartsAt   *time.Time
}

type LiveOdd struct {
	GameID        string
	Sport         string
	Bookmaker     string
	MarketID      string
	MarketName    string
	SelectionID   string
	SelectionName string
	Line          string
	PriceDec      string
	PriceFrac     string
	FetchedAt     time.Time
	Raw           string
}

type APIResponse struct {
	Success int                `json:"success"`
	Results [][]map[string]any `json:"results"`
}

// --- ENV / DB ---
func loadEnv() {
	_ = godotenv.Load()
}

func getEnv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}

func connectDB() (*pgxpool.Pool, error) {
	dbURL := getEnv("DATABASE_URL", "")
	return pgxpool.New(context.Background(), dbURL)
}

// --- MAIN ---
func main() {
	loadEnv()

	db, err := connectDB()
	if err != nil {
		log.Fatalf("❌ DB connection failed: %v", err)
	}
	defer db.Close()

	r := gin.Default()
	r.Use(cors.New(cors.Config{
		AllowOrigins:     []string{"http://127.0.0.1:5173"},
		AllowMethods:     []string{"GET", "POST"},
		AllowHeaders:     []string{"Origin", "Content-Type"},
		AllowCredentials: true,
	}))
	// 1. Загрузка матчей (pre + live)
	r.GET("/sync-games", func(c *gin.Context) {
		all, err := fetchAllGames()
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		if err := upsertGames(db, all); err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"status": "✅ Games synced", "count": len(all)})
	})

	// 2. Загрузка коэффициентов для live матчей
	r.GET("/update-liveodds", func(c *gin.Context) {
		gameIDs, err := fetchLiveGameIDs(db)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}

		inserted := 0
		for _, id := range gameIDs {
			sport, _ := getGameSport(db, id)
			odds, err := fetchLiveOdds(id, sport)
			if err != nil {
				log.Printf("❌ Fetch odds error for %s: %v", id, err)
				continue
			}
			if err := insertLiveOdds(db, odds); err != nil {
				log.Printf("❌ Insert odds error for %s: %v", id, err)
				continue
			}
			inserted += len(odds)
		}
		c.JSON(200, gin.H{"status": "✅ Odds updated", "inserted": inserted})
	})
	r.GET("/api/games", func(c *gin.Context) {
		rows, err := db.Query(context.Background(), `
		SELECT game_id, league, home_team, away_team, time_status, starts_at
		FROM games
		WHERE time_status IN ('0','1')
		ORDER BY starts_at NULLS LAST
		LIMIT 100
	`)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()

		type G struct {
			GameID   string     `json:"game_id"`
			League   string     `json:"league"`
			Home     string     `json:"home_team"`
			Away     string     `json:"away_team"`
			Time     string     `json:"time_status"`
			StartsAt *time.Time `json:"starts_at"`
			Odds     any        `json:"odds"`
		}

		var out []G

		for rows.Next() {
			var g G
			if err := rows.Scan(&g.GameID, &g.League, &g.Home, &g.Away, &g.Time, &g.StartsAt); err == nil {

				// Загружаем коэффициенты
				oddsRows, err := db.Query(context.Background(),
					`SELECT selection_name, price_dec FROM odds WHERE game_id = $1`,
					g.GameID,
				)
				if err == nil {
					var odds []map[string]string
					for oddsRows.Next() {
						var name, price string
						if err := oddsRows.Scan(&name, &price); err == nil {
							odds = append(odds, map[string]string{
								"selection_name": name,
								"price_dec":      price,
							})
						}
					}
					oddsRows.Close()
					g.Odds = odds
				} else {
					g.Odds = []map[string]string{}
				}

				out = append(out, g)
			}
		}

		c.JSON(200, gin.H{"games": out})
	})

	r.Run(":" + getEnv("PORT", "9090"))
}

// --- GAME FETCHING ---

func fetchAllGames() ([]Game, error) {
	var all []Game
	if g, err := fetchPreGames("soccer"); err == nil {
		all = append(all, g...)
	}
	if g, err := fetchPreGames("tennis"); err == nil {
		all = append(all, g...)
	}
	if g, err := fetchLiveGames("soccer"); err == nil {
		all = append(all, g...)
	}
	if g, err := fetchLiveGames("tennis"); err == nil {
		all = append(all, g...)
	}
	return all, nil
}

func fetchPreGames(sport string) ([]Game, error) {
	login := getEnv("API_LOGIN", "")
	token := getEnv("API_TOKEN", "")
	url := fmt.Sprintf("https://bookiesapi.com/api/get.php?login=%s&token=%s&task=pre&bookmaker=bet365&sport=%s",
		login, token, sport)

	var resp struct {
		Games []struct {
			GameID     string `json:"game_id"`
			Time       string `json:"time"`
			TimeStatus string `json:"time_status"`
			League     string `json:"league"`
			Home       string `json:"home"`
			Away       string `json:"away"`
			Scores     string `json:"scores"`
		} `json:"games_pre"`
	}

	httpResp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()
	if err := json.NewDecoder(httpResp.Body).Decode(&resp); err != nil {
		return nil, err
	}

	var out []Game
	for _, g := range resp.Games {
		out = append(out, Game{
			GameID:     g.GameID,
			Sport:      sport,
			Bookmaker:  "bet365",
			Source:     "pre",
			League:     g.League,
			Home:       g.Home,
			Away:       g.Away,
			Scores:     g.Scores,
			TimeStatus: g.TimeStatus,
			StartsAt:   parseUnixMaybe(g.Time),
		})
	}
	return out, nil
}

func fetchLiveGames(sport string) ([]Game, error) {
	login := getEnv("API_LOGIN", "")
	token := getEnv("API_TOKEN", "")
	url := fmt.Sprintf("https://bookiesapi.com/api/get.php?login=%s&token=%s&task=live&bookmaker=bet365&sport=%s",
		login, token, sport)

	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var root map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&root); err != nil {
		return nil, err
	}

	arrRaw, ok := root["games"].([]any)
	if !ok {
		return nil, nil
	}

	var out []Game
	for _, item := range arrRaw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		gameID := fmt.Sprintf("%v", m["game_id"])
		out = append(out, Game{
			GameID:     gameID,
			Sport:      sport,
			Bookmaker:  "bet365",
			Source:     "live",
			League:     fmt.Sprintf("%v", m["league"]),
			Home:       fmt.Sprintf("%v", m["home"]),
			Away:       fmt.Sprintf("%v", m["away"]),
			Scores:     fmt.Sprintf("%v", m["scores"]),
			TimeStatus: fmt.Sprintf("%v", m["time_status"]),
			StartsAt:   parseUnixMaybe(fmt.Sprintf("%v", m["time"])),
		})
	}
	return out, nil
}

// --- LIVE ODDS FETCHING ---

func fetchLiveGameIDs(pool *pgxpool.Pool) ([]string, error) {
	rows, err := pool.Query(context.Background(), "SELECT game_id FROM games WHERE source='live' AND time_status='1'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func getGameSport(pool *pgxpool.Pool, gameID string) (string, error) {
	var sport string
	err := pool.QueryRow(context.Background(), "SELECT sport FROM games WHERE game_id=$1", gameID).Scan(&sport)
	if err != nil {
		return "", err
	}
	return sport, nil
}

func fetchLiveOdds(gameID, sport string) ([]LiveOdd, error) {
	login := getEnv("API_LOGIN", "")
	token := getEnv("API_TOKEN", "")
	url := fmt.Sprintf("https://bookiesapi.com/api/get.php?login=%s&token=%s&task=liveodds&bookmaker=bet365&game_id=%s",
		login, token, gameID)

	res, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	var apiResp APIResponse
	if err := json.NewDecoder(res.Body).Decode(&apiResp); err != nil {
		return nil, err
	}

	var odds []LiveOdd
	now := time.Now()

	var currentMarketID, currentMarketName string
	for _, group := range apiResp.Results {
		for _, item := range group {
			switch fmt.Sprintf("%v", item["type"]) {
			case "MG":
				currentMarketID = fmt.Sprintf("%v", item["ID"])
				currentMarketName = fmt.Sprintf("%v", item["NA"])
			case "PA":
				oddsStr, ok := getOddsField(item)
				if !ok {
					continue
				}
				priceDec, priceFrac, _ := fracToDecimal(oddsStr)
				selectionID := fmt.Sprintf("%v", item["ID"])
				selectionName := fmt.Sprintf("%v", item["NA"])
				line := fmt.Sprintf("%v", item["HA"])
				rawJSON, _ := json.Marshal(item)

				odds = append(odds, LiveOdd{
					GameID:        gameID,
					Sport:         sport,
					Bookmaker:     "bet365",
					MarketID:      currentMarketID,
					MarketName:    currentMarketName,
					SelectionID:   selectionID,
					SelectionName: selectionName,
					Line:          line,
					PriceDec:      priceDec,
					PriceFrac:     priceFrac,
					FetchedAt:     now,
					Raw:           string(rawJSON),
				})
			}
		}
	}
	return odds, nil
}

// --- DATABASE INSERTS ---

func upsertGames(pool *pgxpool.Pool, games []Game) error {
	if len(games) == 0 {
		return nil
	}

	// Удаление матчей с прошедшей датой
	_, err := pool.Exec(context.Background(), `
		DELETE FROM games
		WHERE starts_at < CURRENT_DATE
	`)
	if err != nil {
		return fmt.Errorf("failed to delete old games: %w", err)
	}

	// Продолжение: вставка обновленных данных
	batch := &pgx.Batch{}
	for _, g := range games {
		batch.Queue(`
			INSERT INTO games
				(game_id, sport, bookmaker, source, league, home_team, away_team, scores, time_status, starts_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,now())
			ON CONFLICT (game_id)
			DO UPDATE SET
				sport=$2, bookmaker=$3, source=$4, league=$5, home_team=$6, away_team=$7, scores=$8, time_status=$9, starts_at=$10, updated_at=now()
		`, g.GameID, g.Sport, g.Bookmaker, g.Source, g.League, g.Home, g.Away, g.Scores, g.TimeStatus, g.StartsAt)
	}

	br := pool.SendBatch(context.Background(), batch)
	defer br.Close()
	for range games {
		if _, err := br.Exec(); err != nil {
			return err
		}
	}
	return nil
}

func insertLiveOdds(pool *pgxpool.Pool, odds []LiveOdd) error {
	if len(odds) == 0 {
		return nil
	}

	// Удаление устаревших коэффициентов (например, старше 1 дня)
	_, err := pool.Exec(context.Background(), `
		DELETE FROM liveodds
		WHERE fetched_at < NOW() - INTERVAL '1 day'
	`)
	if err != nil {
		return fmt.Errorf("failed to delete old live odds: %w", err)
	}

	batch := &pgx.Batch{}
	for _, o := range odds {
		batch.Queue(`
			INSERT INTO liveodds
				(game_id, sport, bookmaker, market_id, market_name,
				 selection_id, selection_name, line, price_dec, price_frac,
				 fetched_at, raw)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
			ON CONFLICT (game_id, market_id, selection_id)
			DO UPDATE SET
				sport=$2, bookmaker=$3, market_name=$5, selection_name=$7,
				line=$8, price_dec=$9, price_frac=$10, fetched_at=$11, raw=$12
		`, o.GameID, o.Sport, o.Bookmaker, o.MarketID, o.MarketName,
			o.SelectionID, o.SelectionName, o.Line, o.PriceDec, o.PriceFrac,
			o.FetchedAt, o.Raw)
	}

	br := pool.SendBatch(context.Background(), batch)
	defer br.Close()
	for range odds {
		if _, err := br.Exec(); err != nil {
			return err
		}
	}
	return nil
}

// --- HELPERS ---

func parseUnixMaybe(s string) *time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	sec, err := strconv.ParseInt(s, 10, 64)
	if err != nil || sec <= 0 {
		return nil
	}
	t := time.Unix(sec, 0).UTC()
	return &t
}

func fracToDecimal(odds string) (string, string, bool) {
	odds = strings.TrimSpace(odds)
	parts := strings.Split(odds, "/")
	if len(parts) != 2 {
		return "", odds, false
	}
	a, err1 := strconv.ParseFloat(parts[0], 64)
	b, err2 := strconv.ParseFloat(parts[1], 64)
	if err1 != nil || err2 != nil || b == 0 {
		return "", odds, false
	}
	d := 1.0 + (a / b)
	d = math.Round(d*1000) / 1000
	return strconv.FormatFloat(d, 'f', -1, 64), odds, true
}

func getOddsField(item map[string]any) (string, bool) {
	for _, key := range []string{"OD", "ODD", "ODDS"} {
		if v, ok := item[key]; ok {
			return fmt.Sprintf("%v", v), true
		}
	}
	return "", false
}

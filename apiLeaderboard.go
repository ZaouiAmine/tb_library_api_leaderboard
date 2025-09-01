package lib

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/taubyte/go-sdk/database"
	"github.com/taubyte/go-sdk/event"
	http "github.com/taubyte/go-sdk/http/event"
)

// Shared helper for errors → writes error message + returns status code
func fail(h http.Event, err error, code int) uint32 {
	h.Write([]byte(err.Error()))
	h.Return(code)
	return 1
}

// ===== Data Structures =====

// Represents a 3D vector (used for block position/scale)
type Vec3 struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

// Describes a single block/game event
type GameEvent struct {
	EventType      string `json:"event_type"`
	BlockIndex     int    `json:"block_index"`
	BlockPosition  Vec3   `json:"block_position"`
	BlockScale     Vec3   `json:"block_scale"`
	TargetPosition Vec3   `json:"target_position"`
	TargetScale    Vec3   `json:"target_scale"`
	Timestamp      int64  `json:"timestamp"`
}

// Represents a player's game submission
type GameStateReq struct {
	PlayerName      string      `json:"player_name"`
	GameEvents      []GameEvent `json:"game_events"`
	GameDuration    int64       `json:"game_duration"`
	FinalBlockCount int         `json:"final_block_count"`
}

// ===== Utility Functions =====

// Compute score from the player's final block count (kept for compatibility; not used by new set)
func computeScore(req GameStateReq) int {
	score := req.FinalBlockCount - 1
	if score < 0 {
		return 0
	}
	return score
}

// helper for absolute value of int64
func absInt64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// ===== Exported Functions (HTTP Handlers) =====

// getAll → Returns the full leaderboard as JSON
//export getAll
func getAll(e event.Event) uint32 {
	// Parse HTTP request
	h, err := e.HTTP()
	if err != nil {
		return 1
	}

	// Open leaderboard database
	db, err := database.New("/leaderboard")
	if err != nil {
		return fail(h, err, 500)
	}

	// List all player keys
	keys, err := db.List("")
	if err != nil {
		return fail(h, err, 500)
	}

	// Sort player names alphabetically
	sort.Strings(keys)

	// Collect {player_name, highest_score} entries
	entries := make([]map[string]string, 0, len(keys))
	for _, key := range keys {
		value, err := db.Get(key)
		if err != nil {
			continue // skip if record is corrupted
		}
		entries = append(entries, map[string]string{
			"player_name":   strings.Trim(key, "/"),
			"highest_score": string(value),
		})
	}

	// Encode result as JSON and send back
	jsonData, err := json.Marshal(entries)
	if err != nil {
		return fail(h, err, 500)
	}

	h.Headers().Set("Content-Type", "application/json")
	h.Write(jsonData)
	h.Return(200)
	return 0
}

// get → Returns one player’s score (via query param `player_name`)
//export get
func get(e event.Event) uint32 {
	// Parse HTTP request
	h, err := e.HTTP()
	if err != nil {
		return 1
	}

	// Extract player_name from query string
	key, err := h.Query().Get("player_name")
	if err != nil {
		return fail(h, err, 400)
	}

	// Open leaderboard database
	db, err := database.New("/leaderboard")
	if err != nil {
		return fail(h, err, 500)
	}

	// Look up player's score
	value, err := db.Get(key)
	if err != nil {
		return fail(h, err, 404) // not found
	}

	// Send score as plain response
	h.Write(value)
	h.Return(200)
	return 0
}

// set → Submits/updates a player’s score if higher than existing
// Anti-cheat: replay events, validate timestamps and speed, ignore final_block_count
//export set
func set(e event.Event) uint32 {
	// Parse HTTP request
	h, err := e.HTTP()
	if err != nil {
		return 1
	}

	// Open leaderboard database
	db, err := database.New("/leaderboard")
	if err != nil {
		return fail(h, err, 500)
	}

	// Decode request body JSON into GameStateReq
	var req GameStateReq
	dec := json.NewDecoder(h.Body())
	defer h.Body().Close()
	if err = dec.Decode(&req); err != nil {
		return fail(h, err, 400)
	}

	// Validate input
	if strings.TrimSpace(req.PlayerName) == "" {
		return fail(h, fmt.Errorf("missing player_name"), 400)
	}

	// ===== Anti-cheat scoring =====
	blockCount := 0
	var lastTimestamp int64 = -1

	for _, ev := range req.GameEvents {
		// Ensure timestamps always move forward
		if lastTimestamp >= 0 && ev.Timestamp <= lastTimestamp {
			return fail(h, fmt.Errorf("invalid event timing"), 400)
		}
		lastTimestamp = ev.Timestamp

		// Only count block placements (stacking game)
		if strings.ToLower(ev.EventType) == "block_placed" {
			blockCount++
		}
	}

	// Quick sanity: if there are events, check duration and rate
	if len(req.GameEvents) > 0 {
		first := req.GameEvents[0].Timestamp
		last := req.GameEvents[len(req.GameEvents)-1].Timestamp
		actualDuration := last - first

		// if duration is way off claimed, reject (5s slack)
		if req.GameDuration > 0 && absInt64(actualDuration-req.GameDuration) > 5000 {
			return fail(h, fmt.Errorf("duration mismatch"), 400)
		}

		// rate-limit: max 5 blocks per second
		if actualDuration > 0 {
			blocksPerSecond := float64(blockCount) / (float64(actualDuration) / 1000.0)
			if blocksPerSecond > 5.0 {
				return fail(h, fmt.Errorf("impossible block rate"), 400)
			}
		}
	}

	// Final score = block count - 1 (first block is base)
	newScore := blockCount - 1
	if newScore < 0 {
		newScore = 0
	}

	// ===== Leaderboard update =====
	existingBest := 0
	if b, err := db.Get(req.PlayerName); err == nil && len(b) > 0 {
		if v, convErr := strconv.Atoi(string(b)); convErr == nil {
			existingBest = v
		}
	}

	// Only update if new score is higher
	if newScore > existingBest {
		if err = db.Put(req.PlayerName, []byte(strconv.Itoa(newScore))); err != nil {
			return fail(h, err, 500)
		}
	}

	// Respond success
	h.Return(200)
	return 0
}

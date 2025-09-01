package lib

import (
	"encoding/json"
	"fmt"
	"math"
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

// ===== Anti-Cheat Utility Functions =====

// Helper for absolute value of float64
func absFloat64(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// Helper for absolute value of int64
func absInt64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// Calculate distance between two 3D points
func distance(p1, p2 Vec3) float64 {
	dx := p1.X - p2.X
	dy := p1.Y - p2.Y
	dz := p1.Z - p2.Z
	return math.Sqrt(dx*dx + dy*dy + dz*dz)
}

// Validate game events for anti-cheat
func validateGameEvents(events []GameEvent, gameDuration int64, finalBlockCount int) error {
	if len(events) == 0 {
		return fmt.Errorf("no game events provided")
	}

	// Sort events by timestamp to ensure chronological order
	sortedEvents := make([]GameEvent, len(events))
	copy(sortedEvents, events)
	sort.Slice(sortedEvents, func(i, j int) bool {
		return sortedEvents[i].Timestamp < sortedEvents[j].Timestamp
	})

	var lastTimestamp int64 = -1
	blockCount := 0
	var lastBlockPosition Vec3

	// Constants for anti-cheat validation
	const (
		maxBlocksPerSecond = 3.0           // Maximum blocks per second
		maxBlockDistance   = 10.0          // Maximum distance between blocks
		minTimeBetweenEvents = 50          // Minimum 50ms between events
		maxGameDuration    = 3600000       // Maximum 1 hour game duration
		minGameDuration    = 1000          // Minimum 1 second game duration
	)

	for i, ev := range sortedEvents {
		// 1. Validate timestamp progression
		if lastTimestamp >= 0 {
			timeDiff := ev.Timestamp - lastTimestamp
			if timeDiff < minTimeBetweenEvents {
				return fmt.Errorf("events too close together: %dms between events", timeDiff)
			}
		}
		lastTimestamp = ev.Timestamp

		// 2. Validate event type
		if ev.EventType == "" {
			return fmt.Errorf("empty event type at index %d", i)
		}

		// 3. Count block placement events
		if strings.ToLower(ev.EventType) == "block_placed" {
			blockCount++
			
			// 4. Validate block positions (if not first block)
			if blockCount > 1 {
				dist := distance(ev.BlockPosition, lastBlockPosition)
				if dist > maxBlockDistance {
					return fmt.Errorf("block distance too large: %.2f units", dist)
				}
			}
			lastBlockPosition = ev.BlockPosition
		}

		// 5. Validate block index progression
		if ev.BlockIndex < 0 {
			return fmt.Errorf("negative block index: %d", ev.BlockIndex)
		}
	}

	// 6. Validate game duration
	if gameDuration < minGameDuration {
		return fmt.Errorf("game duration too short: %dms", gameDuration)
	}
	if gameDuration > maxGameDuration {
		return fmt.Errorf("game duration too long: %dms", gameDuration)
	}

	// 7. Validate actual duration matches claimed duration
	if len(sortedEvents) > 1 {
		actualDuration := sortedEvents[len(sortedEvents)-1].Timestamp - sortedEvents[0].Timestamp
		durationDiff := absInt64(actualDuration - gameDuration)
		if durationDiff > 5000 { // Allow 5 second tolerance
			return fmt.Errorf("duration mismatch: claimed %dms, actual %dms", gameDuration, actualDuration)
		}
	}

	// 8. Validate block rate
	if len(sortedEvents) > 1 {
		actualDuration := sortedEvents[len(sortedEvents)-1].Timestamp - sortedEvents[0].Timestamp
		if actualDuration > 0 {
			blocksPerSecond := float64(blockCount) / (float64(actualDuration) / 1000.0)
			if blocksPerSecond > maxBlocksPerSecond {
				return fmt.Errorf("block rate too high: %.2f blocks/second", blocksPerSecond)
			}
		}
	}

	// 9. Validate final block count matches actual block placement events
	if blockCount != finalBlockCount {
		return fmt.Errorf("final block count mismatch: expected %d, got %d", finalBlockCount, blockCount)
	}

	return nil
}

// ===== Utility Functions =====

// Compute score from the player's final block count
func computeScore(req GameStateReq) int {
	score := req.FinalBlockCount - 1
	if score < 0 {
		return 0
	}
	return score
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

// get → Returns one player's score (via query param `player_name`)
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
		return fail(h, fmt.Errorf("missing player_name parameter"), 400)
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

// set → Submits/updates a player's score with anti-cheat validation
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
		return fail(h, fmt.Errorf("invalid JSON format: %v", err), 400)
	}

	// Basic input validation
	if strings.TrimSpace(req.PlayerName) == "" {
		return fail(h, fmt.Errorf("missing or empty player_name"), 400)
	}

	// Anti-cheat validation
	if err := validateGameEvents(req.GameEvents, req.GameDuration, req.FinalBlockCount); err != nil {
		return fail(h, fmt.Errorf("anti-cheat validation failed: %v", err), 400)
	}

	// Compute new score based on validated events
	newScore := computeScore(req)

	// Check existing best score for player
	existingBest := 0
	if b, err := db.Get(req.PlayerName); err == nil && len(b) > 0 {
		if v, convErr := strconv.Atoi(string(b)); convErr == nil {
			existingBest = v
		}
	}

	// Only update if new score is higher
	if newScore > existingBest {
		if err = db.Put(req.PlayerName, []byte(strconv.Itoa(newScore))); err != nil {
			return fail(h, fmt.Errorf("failed to save score: %v", err), 500)
		}
	}

	// Respond with success and validation info
	response := map[string]interface{}{
		"player_name":    req.PlayerName,
		"score":          newScore,
		"previous_best":  existingBest,
		"updated":        newScore > existingBest,
		"events_validated": len(req.GameEvents),
		"validation_passed": true,
	}

	jsonResponse, err := json.Marshal(response)
	if err != nil {
		return fail(h, err, 500)
	}

	h.Headers().Set("Content-Type", "application/json")
	h.Write(jsonResponse)
	h.Return(200)
	return 0
}
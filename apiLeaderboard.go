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

// Compute score from the player's final block count
func computeScore(req GameStateReq) int {
	// Anti-cheat validation using timestamps and event analysis
	if len(req.GameEvents) == 0 {
		return 0 // No events = no score
	}

	// Sort events by timestamp to ensure chronological order
	sortedEvents := make([]GameEvent, len(req.GameEvents))
	copy(sortedEvents, req.GameEvents)
	sort.Slice(sortedEvents, func(i, j int) bool {
		return sortedEvents[i].Timestamp < sortedEvents[j].Timestamp
	})

	var lastTimestamp int64 = -1
	blockCount := 0
	var lastBlockPosition Vec3

	// Anti-cheat constants
	const (
		maxBlocksPerSecond = 3.0           // Maximum blocks per second
		maxBlockDistance   = 15.0          // Maximum distance between blocks
		minTimeBetweenEvents = 50          // Minimum 50ms between events
		maxGameDuration    = 3600000       // Maximum 1 hour game duration
		minGameDuration    = 1000          // Minimum 1 second game duration
	)

	for i, ev := range sortedEvents {
		// Validate timestamp progression
		if lastTimestamp >= 0 {
			timeDiff := ev.Timestamp - lastTimestamp
			if timeDiff < minTimeBetweenEvents {
				// Events too close together - suspicious, reduce score
				blockCount = int(float64(blockCount) * 0.5)
				break
			}
		}
		lastTimestamp = ev.Timestamp

		// Count block placement events
		if ev.EventType == "block_placed" {
			blockCount++
			
			// Validate block positions (if not first block)
			if blockCount > 1 {
				// Calculate 3D distance between blocks
				dx := ev.BlockPosition.X - lastBlockPosition.X
				dy := ev.BlockPosition.Y - lastBlockPosition.Y
				dz := ev.BlockPosition.Z - lastBlockPosition.Z
				dist := dx*dx + dy*dy + dz*dz // Square distance for performance
				
				if dist > maxBlockDistance*maxBlockDistance {
					// Blocks too far apart - suspicious, reduce score
					blockCount = int(float64(blockCount) * 0.7)
					break
				}
			}
			lastBlockPosition = ev.BlockPosition
		}

		// Validate block index progression
		if ev.BlockIndex < 0 {
			// Invalid block index - suspicious
			blockCount = int(float64(blockCount) * 0.8)
			break
		}
	}

	// Validate game duration
	if req.GameDuration < minGameDuration || req.GameDuration > maxGameDuration {
		// Suspicious game duration - reduce score
		blockCount = int(float64(blockCount) * 0.6)
	}

	// Validate actual duration matches claimed duration
	if len(sortedEvents) > 1 {
		actualDuration := sortedEvents[len(sortedEvents)-1].Timestamp - sortedEvents[0].Timestamp
		durationDiff := actualDuration - req.GameDuration
		if durationDiff < 0 {
			durationDiff = -durationDiff
		}
		if durationDiff > 5000 { // Allow 5 second tolerance
			// Duration mismatch - suspicious, reduce score
			blockCount = int(float64(blockCount) * 0.5)
		}
	}

	// Validate block rate
	if len(sortedEvents) > 1 {
		actualDuration := sortedEvents[len(sortedEvents)-1].Timestamp - sortedEvents[0].Timestamp
		if actualDuration > 0 {
			blocksPerSecond := float64(blockCount) / (float64(actualDuration) / 1000.0)
			if blocksPerSecond > maxBlocksPerSecond {
				// Block rate too high - suspicious, reduce score
				blockCount = int(float64(blockCount) * 0.4)
			}
		}
	}

	// Validate final block count consistency
	if blockCount != req.FinalBlockCount {
		// Count mismatch - suspicious, use the lower value
		if req.FinalBlockCount < blockCount {
			blockCount = req.FinalBlockCount
		}
		// Apply penalty for inconsistency
		blockCount = int(float64(blockCount) * 0.9)
	}

	// Score = block count - 1 (first block is base)
	score := blockCount - 1
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

// set → Submits/updates a player's score if higher than existing
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
	if req.PlayerName == "" {
		return fail(h, err, 400)
	}

	// Compute new score
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
			return fail(h, err, 500)
		}
	}

	// Respond success
	h.Return(200)
	return 0
}
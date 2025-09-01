package lib

import (
	"encoding/json"
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


// computeScoreFromEvents securely recomputes score
func computeScoreFromEvents(req GameStateReq) (int, error) {
    if len(req.GameEvents) == 0 {
        return 0, fmt.Errorf("no game events provided")
    }

    blockCount := 0
    lastTimestamp := req.GameEvents[0].Timestamp
    firstTimestamp := lastTimestamp
    maxBlocksPerSec := 5 // configurable

    for i, ev := range req.GameEvents {
        // 1. Timestamps must increase
        if ev.Timestamp < lastTimestamp {
            return 0, fmt.Errorf("invalid event order at index %d", i)
        }
        elapsed := ev.Timestamp - lastTimestamp
        if elapsed < 0 {
            return 0, fmt.Errorf("negative time gap at index %d", i)
        }
        lastTimestamp = ev.Timestamp

        // 2. Handle events
        switch strings.ToLower(ev.EventType) {
        case "block_placed":
            blockCount++
        case "block_removed":
            if blockCount > 0 {
                blockCount--
            }
        default:
            // ignore unknown events
        }
    }

    // 3. Validate duration
    actualDuration := lastTimestamp - firstTimestamp
    if req.GameDuration > 0 && actualDuration > req.GameDuration+2000 {
        // Allow small tolerance (~2s)
        return 0, fmt.Errorf("duration mismatch")
    }

    // 4. Validate growth rate
    if actualDuration > 0 {
        blocksPerSec := float64(blockCount) / (float64(actualDuration) / 1000.0)
        if blocksPerSec > float64(maxBlocksPerSec) {
            return 0, fmt.Errorf("impossible block rate")
        }
    }

    // 5. Apply original scoring rule
    score := blockCount - 1
    if score < 0 {
        score = 0
    }

    return score, nil
}


// set → Submits/updates a player’s score if higher than existing
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
        return fail(h, fmt.Errorf("missing player_name"), 400)
    }

    // Compute new score from secure event replay
    newScore, err := computeScoreFromEvents(req)
    if err != nil {
        return fail(h, err, 400)
    }

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
}

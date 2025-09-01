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
		return fail(h, err, 400)
	}

	// ===== Anti-cheat scoring =====
	blockCount := 0
	var lastTimestamp int64 = -1

	for _, ev := range req.GameEvents {
		// Only count block placements
		if ev.EventType == "block_placed" {
			blockCount++
		}

		// Ensure timestamps always move forward
		if lastTimestamp >= 0 && ev.Timestamp <= lastTimestamp {
			return fail(h, 
				fmt.Errorf("invalid event timing"), 400)
		}
		lastTimestamp = ev.Timestamp
	}

	// Quick sanity: check duration matches claimed
	if len(req.GameEvents) > 0 {
		first := req.GameEvents[0].Timestamp
		last := req.GameEvents[len(req.GameEvents)-1].Timestamp
		actualDuration := last - first

		// if duration is way off claimed, reject
		if req.GameDuration > 0 && abs(actualDuration-req.GameDuration) > 5000 {
			return fail(h, 
				fmt.Errorf("duration mismatch"), 400)
		}

		// rate-limit: max 5 blocks per second
		if actualDuration > 0 {
			blocksPerSecond := float64(blockCount) / (float64(actualDuration) / 1000.0)
			if blocksPerSecond > 5.0 {
				return fail(h, 
					fmt.Errorf("impossible block rate"), 400)
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

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

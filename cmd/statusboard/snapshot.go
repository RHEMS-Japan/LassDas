package main

import (
	"encoding/json"
	"errors"
	"time"
)

const snapshotMaxAge = 3 * time.Minute

// Routing belongs to the private snapshot, never to the browser. Nothing
// writes a routing field onto a row any more, but board.json outlives the
// process that wrote it: a file left by an older engine must not reach a
// reader either.
func publicBoardRow(raw json.RawMessage) (json.RawMessage, error) {
	var row map[string]json.RawMessage
	if err := json.Unmarshal(raw, &row); err != nil || row == nil {
		return nil, errors.New("invalid board row")
	}
	delete(row, "workspace_path")
	return json.Marshal(row)
}

func publicSnapshot(raw []byte) (json.RawMessage, time.Time, error) {
	var shape struct {
		SchemaVersion int               `json:"schema_version"`
		GeneratedAt   time.Time         `json:"generated_at"`
		Runs          []json.RawMessage `json:"runs"`
	}
	if err := json.Unmarshal(raw, &shape); err != nil || shape.SchemaVersion != 1 || shape.GeneratedAt.IsZero() {
		return nil, time.Time{}, errors.New("invalid board snapshot")
	}
	var board map[string]json.RawMessage
	if err := json.Unmarshal(raw, &board); err != nil {
		return nil, time.Time{}, err
	}
	rows := make([]json.RawMessage, 0, len(shape.Runs))
	for _, row := range shape.Runs {
		public, err := publicBoardRow(row)
		if err != nil {
			return nil, time.Time{}, err
		}
		rows = append(rows, public)
	}
	board["runs"], _ = json.Marshal(rows)
	public, err := json.Marshal(board)
	return public, shape.GeneratedAt, err
}

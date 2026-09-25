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
//
// The acknowledgement is added here rather than in the snapshot: the
// attendant writes what the engine did, and whether a person has cleared a
// finished card away is the board's own fact, in the board's own file.
func publicBoardRow(raw json.RawMessage, acknowledged map[string]acknowledgement) (json.RawMessage, error) {
	var row map[string]json.RawMessage
	if err := json.Unmarshal(raw, &row); err != nil || row == nil {
		return nil, errors.New("invalid board row")
	}
	delete(row, "workspace_path")
	delete(row, "acknowledged_at")
	var identity struct {
		DeliveryID string `json:"delivery_id"`
	}
	if json.Unmarshal(raw, &identity) == nil && identity.DeliveryID != "" {
		if entry, cleared := acknowledged[identity.DeliveryID]; cleared {
			stamp, err := json.Marshal(entry.At)
			if err != nil {
				return nil, err
			}
			row["acknowledged_at"] = stamp
		}
	}
	return json.Marshal(row)
}

func publicSnapshot(raw []byte, acknowledged map[string]acknowledgement) (json.RawMessage, time.Time, error) {
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
		public, err := publicBoardRow(row, acknowledged)
		if err != nil {
			return nil, time.Time{}, err
		}
		rows = append(rows, public)
	}
	board["runs"], _ = json.Marshal(rows)
	public, err := json.Marshal(board)
	return public, shape.GeneratedAt, err
}

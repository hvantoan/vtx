package persistence

import (
	"encoding/json"
	"fmt"
)

func jsonBytes(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal evidence: %w", err)
	}
	return b, nil
}

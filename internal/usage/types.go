// Package usage models the payload returned by opencode's usage endpoint and
// parses it defensively: the endpoint is an external dependency, so every field
// is treated as optional and an unrecognized shape degrades to "unknown" rather
// than to a wrong number.
package usage

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

// Window is one quota window (rolling / weekly / monthly).
type Window struct {
	// Status is the upstream verdict, e.g. "ok" or "rate-limited".
	Status string
	// Percent is the consumed fraction in [0, 100]. Meaningful only when HasPercent.
	Percent float64
	// HasPercent reports whether the payload carried a usable percent value.
	HasPercent bool
	// ResetsAt is the window reset time, meaningful only when HasResetsAt.
	ResetsAt time.Time
	// HasResetsAt reports whether the payload carried a parseable reset time.
	HasResetsAt bool
}

// Usable reports whether the window is known and not flagged by upstream.
func (w Window) Usable() bool {
	return w.Status == "" || strings.EqualFold(w.Status, "ok")
}

// Usage is the parsed usage payload.
type Usage struct {
	Rolling Window
	Weekly  Window
	Monthly Window
	// Windows counts how many of the three windows the payload actually carried.
	Windows int
}

// Known reports whether the payload carried at least one recognized window.
func (u Usage) Known() bool { return u.Windows > 0 }

// Unavailable reports whether upstream flagged any window as not usable, along
// with a human-readable reason for the resource page.
func (u Usage) Unavailable() (bool, string) {
	if reason := windowReason("rolling", u.Rolling); reason != "" {
		return true, reason
	}
	if reason := windowReason("weekly", u.Weekly); reason != "" {
		return true, reason
	}
	if reason := windowReason("monthly", u.Monthly); reason != "" {
		return true, reason
	}
	return false, ""
}

func windowReason(name string, w Window) string {
	if w.Status == "" || strings.EqualFold(w.Status, "ok") {
		return ""
	}
	return fmt.Sprintf("%s=%s", name, w.Status)
}

// SortPercent returns the ranking key: rolling.percent.
//
// Per spec only the rolling window drives load balancing; weekly and monthly act
// purely as availability gates. A payload without a usable rolling window is
// reported as unknown so the key sorts last instead of ahead of measured keys.
func (u Usage) SortPercent() (float64, bool) {
	if !u.Rolling.HasPercent {
		return 0, false
	}
	return u.Rolling.Percent, true
}

// MaxPercent is the largest percent across the windows that reported one. It is
// used only for the all-keys-exhausted fallback ordering.
func (u Usage) MaxPercent() (float64, bool) {
	max := math.Inf(-1)
	found := false
	for _, w := range []Window{u.Rolling, u.Weekly, u.Monthly} {
		if w.HasPercent {
			found = true
			if w.Percent > max {
				max = w.Percent
			}
		}
	}
	if !found {
		return 0, false
	}
	return max, true
}

// payload mirrors the upstream JSON document.
type payload struct {
	Usage struct {
		Rolling *rawWindow `json:"rolling"`
		Weekly  *rawWindow `json:"weekly"`
		Monthly *rawWindow `json:"monthly"`
	} `json:"usage"`
}

type rawWindow struct {
	Status   string          `json:"status"`
	Percent  json.RawMessage `json:"percent"`
	ResetsAt string          `json:"resetsAt"`
}

// Parse decodes an opencode usage response body.
//
// Unknown extra windows are ignored. A missing or unparseable field simply leaves
// the corresponding Window zero-valued.
func Parse(body []byte) (Usage, error) {
	var doc payload
	if errUnmarshal := json.Unmarshal(body, &doc); errUnmarshal != nil {
		return Usage{}, fmt.Errorf("decode usage payload: %w", errUnmarshal)
	}
	var out Usage
	for _, item := range []struct {
		name string
		raw  *rawWindow
		dst  *Window
	}{
		{"rolling", doc.Usage.Rolling, &out.Rolling},
		{"weekly", doc.Usage.Weekly, &out.Weekly},
		{"monthly", doc.Usage.Monthly, &out.Monthly},
	} {
		if item.raw == nil {
			continue
		}
		out.Windows++
		if item.name == "rolling" {
			item.dst.Status = strings.TrimSpace(item.raw.Status)
		} else {
			item.dst.Status = strings.TrimSpace(item.raw.Status)
		}
		if percent, ok := decodePercent(item.raw.Percent); ok {
			item.dst.Percent = percent
			item.dst.HasPercent = true
		}
		if resetAt, ok := decodeTime(item.raw.ResetsAt); ok {
			item.dst.ResetsAt = resetAt
			item.dst.HasResetsAt = true
		}
	}
	if out.Windows == 0 {
		return out, fmt.Errorf("usage payload carried no recognized window")
	}
	return out, nil
}

func decodePercent(raw json.RawMessage) (float64, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return 0, false
	}
	var number float64
	if errNumber := json.Unmarshal(raw, &number); errNumber == nil {
		if math.IsNaN(number) || math.IsInf(number, 0) {
			return 0, false
		}
		return number, true
	}
	var text string
	if errText := json.Unmarshal(raw, &text); errText == nil {
		var parsed float64
		if _, errScan := fmt.Sscanf(strings.TrimSpace(text), "%f", &parsed); errScan != nil {
			return 0, false
		}
		if math.IsNaN(parsed) || math.IsInf(parsed, 0) {
			return 0, false
		}
		return parsed, true
	}
	return 0, false
}

func decodeTime(value string) (time.Time, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z0700"} {
		if parsed, errParse := time.Parse(layout, trimmed); errParse == nil {
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}

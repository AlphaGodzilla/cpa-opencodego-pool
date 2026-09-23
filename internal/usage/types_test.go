package usage

import "testing"

// upstreamSample is the payload the opencode usage endpoint returns, verbatim.
const upstreamSample = `{
  "usage": {
    "rolling": {
      "status": "ok",
      "percent": 0,
      "resetsAt": "2026-09-23T14:39:09.347Z"
    },
    "weekly": {
      "status": "rate-limited",
      "percent": 100,
      "resetsAt": "2026-09-28T00:00:00.000Z"
    },
    "monthly": {
      "status": "ok",
      "percent": 95,
      "resetsAt": "2026-10-11T06:39:41.000Z"
    }
  }
}`

func TestParseUpstreamSample(t *testing.T) {
	parsed, errParse := Parse([]byte(upstreamSample))
	if errParse != nil {
		t.Fatalf("Parse: %v", errParse)
	}
	if parsed.Windows != 3 {
		t.Fatalf("windows = %d, want 3", parsed.Windows)
	}
	if !parsed.Known() {
		t.Fatal("payload should be known")
	}
	if parsed.Rolling.Status != "ok" || parsed.Rolling.Percent != 0 {
		t.Errorf("rolling = %+v", parsed.Rolling)
	}
	if parsed.Weekly.Status != "rate-limited" || parsed.Weekly.Percent != 100 {
		t.Errorf("weekly = %+v", parsed.Weekly)
	}
	if parsed.Monthly.Percent != 95 {
		t.Errorf("monthly = %+v", parsed.Monthly)
	}
	if !parsed.Rolling.HasResetsAt || parsed.Rolling.ResetsAt.Year() != 2026 {
		t.Errorf("resetsAt not parsed: %+v", parsed.Rolling)
	}
}

func TestSortPercentUsesRollingOnly(t *testing.T) {
	parsed, _ := Parse([]byte(upstreamSample))
	percent, ok := parsed.SortPercent()
	if !ok || percent != 0 {
		t.Fatalf("SortPercent = %v/%v, want 0/true (weekly and monthly must not rank)", percent, ok)
	}
}

func TestUnavailableFlagsAnyWindow(t *testing.T) {
	parsed, _ := Parse([]byte(upstreamSample))
	unavailable, reason := parsed.Unavailable()
	if !unavailable {
		t.Fatal("a rate-limited weekly window must make the credential unusable")
	}
	if reason != "weekly=rate-limited" {
		t.Errorf("reason = %q", reason)
	}
}

func TestMaxPercentAcrossWindows(t *testing.T) {
	parsed, _ := Parse([]byte(upstreamSample))
	max, ok := parsed.MaxPercent()
	if !ok || max != 100 {
		t.Fatalf("MaxPercent = %v/%v, want 100/true", max, ok)
	}
}

func TestParseToleratesMissingAndOddFields(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		windows     int
		unavailable bool
		sortOK      bool
	}{
		{"empty object", `{}`, 0, false, false},
		{"unknown window only", `{"usage":{"hourly":{"status":"ok","percent":5}}}`, 0, false, false},
		{"rolling without percent", `{"usage":{"rolling":{"status":"ok"}}}`, 1, false, false},
		{"percent as string", `{"usage":{"rolling":{"status":"ok","percent":"7.5"}}}`, 1, false, true},
		{"percent null", `{"usage":{"rolling":{"status":"ok","percent":null}}}`, 1, false, false},
		{"missing status", `{"usage":{"rolling":{"percent":3}}}`, 1, false, true},
		{"unparseable resetsAt", `{"usage":{"rolling":{"status":"ok","percent":3,"resetsAt":"soon"}}}`, 1, false, true},
		{"extra window", `{"usage":{"rolling":{"status":"ok","percent":3},"yearly":{"status":"ok","percent":1}}}`, 1, false, true},
		{"non-ok status ranks but stays usable for sorting", `{"usage":{"rolling":{"status":"throttled","percent":12}}}`, 1, true, true},
	}
	for _, testCase := range cases {
		parsed, errParse := Parse([]byte(testCase.body))
		if testCase.windows == 0 {
			if errParse == nil {
				t.Errorf("%s: expected an error, got %+v", testCase.name, parsed)
			}
			continue
		}
		if errParse != nil {
			t.Errorf("%s: unexpected error %v", testCase.name, errParse)
			continue
		}
		if parsed.Windows != testCase.windows {
			t.Errorf("%s: windows = %d, want %d", testCase.name, parsed.Windows, testCase.windows)
		}
		if unavailable, _ := parsed.Unavailable(); unavailable != testCase.unavailable {
			t.Errorf("%s: unavailable = %v, want %v", testCase.name, unavailable, testCase.unavailable)
		}
		if _, ok := parsed.SortPercent(); ok != testCase.sortOK {
			t.Errorf("%s: SortPercent ok = %v, want %v", testCase.name, ok, testCase.sortOK)
		}
	}
}

func TestParseRejectsNonJSON(t *testing.T) {
	if _, errParse := Parse([]byte("<html>gateway error</html>")); errParse == nil {
		t.Fatal("expected an error for a non-JSON body")
	}
}

func TestWindowUsable(t *testing.T) {
	if !(Window{}).Usable() {
		t.Error("an absent status means unknown, not unusable")
	}
	if !(Window{Status: "OK"}).Usable() {
		t.Error("status comparison must be case-insensitive")
	}
	if (Window{Status: "rate-limited"}).Usable() {
		t.Error("rate-limited must not be usable")
	}
}

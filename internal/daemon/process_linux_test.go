//go:build linux

package daemon

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// buildStat renders a /proc/<pid>/stat line whose comm field is `prefix`
// (already wrapped in parens, e.g. "1234 (bash)") followed by 20 tokens where
// index 19 — stat field 22, starttime, once parsing resumes after the LAST ')'
// — is `startTime`. Extra trailing tokens model the real file's remaining
// fields.
func buildStat(prefix, startTime string) []byte {
	fields := make([]string, 20)
	for i := range fields {
		fields[i] = "0"
	}
	fields[19] = startTime
	return []byte(prefix + " " + strings.Join(fields, " ") + " 0 0 0\n")
}

func TestParseProcStatStartTime(t *testing.T) {
	cases := []struct {
		name   string
		data   []byte
		want   int64
		wantOK bool
	}{
		{name: "normal", data: buildStat("1234 (bash)", "8425"), want: 8425, wantOK: true},
		{name: "comm with spaces", data: buildStat("1234 (my proc)", "999"), want: 999, wantOK: true},
		{name: "comm with open paren", data: buildStat("1234 (a(b)", "111"), want: 111, wantOK: true},
		// comm literally contains a ')': the field is "(weird):)"; the LAST ')'
		// is its closing paren, so a naive first-')' parse would misalign.
		{name: "comm with close paren", data: buildStat("1234 (weird):)", "777"), want: 777, wantOK: true},
		{name: "truncated too few fields", data: []byte("1234 (bash) R 1 1 1\n"), wantOK: false},
		{name: "no closing paren", data: []byte("1234 malformed line\n"), wantOK: false},
		{name: "non-numeric starttime", data: buildStat("1234 (bash)", "abc"), wantOK: false},
		{name: "zero starttime", data: buildStat("1234 (bash)", "0"), wantOK: false},
		// MaxInt64 + 1: parses as uint64 but exceeds int64 range.
		{name: "overflow", data: buildStat("1234 (bash)", "9223372036854775808"), wantOK: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseProcStatStartTime(tc.data)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (data=%q)", ok, tc.wantOK, tc.data)
			}
			if ok && got != tc.want {
				t.Errorf("token = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestProcessStartTime_Self_Linux(t *testing.T) {
	got, ok := ProcessStartTime(os.Getpid())
	if !ok {
		t.Fatal("ProcessStartTime(self) ok = false, want true")
	}
	if got <= 0 {
		t.Errorf("ProcessStartTime(self) = %d, want > 0", got)
	}
	// The parsed value should equal what we get parsing the file directly.
	data, err := os.ReadFile("/proc/" + strconv.Itoa(os.Getpid()) + "/stat")
	if err == nil {
		if want, wok := parseProcStatStartTime(data); wok && want != got {
			t.Errorf("ProcessStartTime = %d, direct parse = %d", got, want)
		}
	}
}

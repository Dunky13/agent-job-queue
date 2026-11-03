package census

import (
	"errors"
	"reflect"
	"testing"
)

var observer = Observer{PID: 999, PGID: 900}

const observerRow = "  999   900   501 S    Mon Nov  2 18:22:25 2026    \n"

func TestParseRejectsUntrustedListings(t *testing.T) {
	for _, test := range []struct {
		name, output string
		want         error
	}{
		{"empty", "", ErrTruncated},
		{"whitespace only", " \n \n", ErrTruncated},
		{"blank line between rows", observerRow + "\n101 400 501 S Mon Nov 2 18:22:26 2026\n", ErrTruncated},
		{"row cut inside birth", observerRow + "101 400 501 S Mon Nov\n", ErrTruncated},
		{"row cut inside year", observerRow + "101 400 501 S Mon Nov 2 18:22:26 20\n", ErrInvalid},
		{"truncated before observer", "101 400 501 S Mon Nov 2 18:22:26 2026\n", ErrIncomplete},
		{"observer in another group", "999 901 501 S Mon Nov 2 18:22:25 2026\n", ErrIncomplete},
		{"zombie observer", "999 900 501 Z Mon Nov 2 18:22:25 2026\n", ErrIncomplete},
		{"zombie observer with flags", "999 900 501 Z+ Mon Nov 2 18:22:25 2026\n", ErrIncomplete},
		{"duplicate pid", observerRow + "101 400 501 S Mon Nov 2 18:22:26 2026\n101 401 501 S Mon Nov 2 18:22:27 2026\n", ErrAmbiguous},
		{"duplicate observer", observerRow + observerRow, ErrAmbiguous},
		{"garbage row", observerRow + "invalid\n", ErrTruncated},
		{"non-numeric pid", observerRow + "abc 400 501 S Mon Nov 2 18:22:26 2026\n", ErrInvalid},
		{"non-numeric pgid", observerRow + "101 abc 501 S Mon Nov 2 18:22:26 2026\n", ErrInvalid},
		{"non-numeric uid", observerRow + "101 400 abc S Mon Nov 2 18:22:26 2026\n", ErrInvalid},
		{"zero pid", observerRow + "0 400 501 S Mon Nov 2 18:22:26 2026\n", ErrInvalid},
		{"negative pgid", observerRow + "101 -1 501 S Mon Nov 2 18:22:26 2026\n", ErrInvalid},
		{"extra birth token", observerRow + "101 400 501 S Mon Nov 2 18:22:26 2026 extra\n", ErrInvalid},
		{"localized birth", observerRow + "101 400 501 S ma 2 nov. 18:22:26 2026\n", ErrInvalid},
		// The column sets this module replaced must fail closed, never shift.
		{"pid,pgid,lstart row", "999 900 Mon Nov 2 18:22:25 2026\n", ErrTruncated},
		{"pid,pgid,stat,lstart row", "999 900 S Mon Nov 2 18:22:25 2026\n", ErrTruncated},
		{"pid,uid,stat,lstart row", "999 501 S Mon Nov 2 18:22:25 2026\n", ErrTruncated},
		{"pid,command row", "999 /usr/bin/fixture --flag\n", ErrTruncated},
	} {
		t.Run(test.name, func(t *testing.T) {
			if table, err := Parse(test.output, observer); !errors.Is(err, test.want) || table != nil {
				t.Fatalf("Parse = %v, %v; want %v", table, err, test.want)
			}
		})
	}
}

func TestParseReadsRows(t *testing.T) {
	table, err := Parse(observerRow+"  101   400    -2 Z+   Sun Nov  1 07:58:26 2026\n102 0 0 Ss Sun Nov 15 07:58:27 2026\n", observer)
	if err != nil {
		t.Fatal(err)
	}
	want := Process{PID: 101, PGID: 400, UID: -2, State: "Z+", Birth: "Sun Nov 1 07:58:26 2026", RawBirth: "Sun Nov  1 07:58:26 2026"}
	if table[101] != want {
		t.Fatalf("row = %+v, want %+v", table[101], want)
	}
	if len(table) != 3 || table[102].PGID != 0 || table[999].RawBirth != "Mon Nov  2 18:22:25 2026" {
		t.Fatalf("table = %+v", table)
	}
}

func TestRawBirthIgnoresColumnsThatMatchIt(t *testing.T) {
	// The state column starts the same way as the weekday.
	table, err := Parse("999 900 501 S Sun Nov  1 07:58:26 2026\n", observer)
	if err != nil || table[999].RawBirth != "Sun Nov  1 07:58:26 2026" {
		t.Fatalf("table = %+v, %v", table, err)
	}
}

func TestGroupIsSortedAndFiltered(t *testing.T) {
	table, err := Parse(observerRow+"300 400 501 S Mon Nov 2 18:22:26 2026\n101 400 501 Z Mon Nov 2 18:22:27 2026\n102 401 501 S Mon Nov 2 18:22:28 2026\n", observer)
	if err != nil {
		t.Fatal(err)
	}
	pids := []int{}
	for _, process := range table.Group(400) {
		pids = append(pids, process.PID)
	}
	if !reflect.DeepEqual(pids, []int{101, 300}) {
		t.Fatalf("group = %v", pids)
	}
	if members := table.Group(402); members == nil || len(members) != 0 {
		t.Fatalf("missing group = %#v", members)
	}
}

func TestIdentify(t *testing.T) {
	table, err := Parse(observerRow+"101 400 501 S Mon Nov 2 18:22:26 2026\n102 400 501 Z Mon Nov 2 18:22:27 2026\n", observer)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		pid   int
		birth string
		want  Liveness
	}{
		{"alive", 101, "Mon Nov 2 18:22:26 2026", Alive},
		{"alive with recorded ps spacing", 101, "Mon Nov  2 18:22:26 2026", Alive},
		{"reused", 101, "Mon Nov 2 18:22:00 2026", Reused},
		{"empty birth is not a match", 101, "", Reused},
		{"zombie", 102, "Mon Nov 2 18:22:27 2026", Defunct},
		{"reused zombie", 102, "Mon Nov 2 18:22:00 2026", Reused},
		{"absent", 103, "Mon Nov 2 18:22:26 2026", Absent},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, got := table.Identify(test.pid, test.birth); got != test.want {
				t.Fatalf("Identify = %v, want %v", got, test.want)
			}
		})
	}
}

func TestWordingKeepsCallerMessages(t *testing.T) {
	wording := Wording{"truncated", "invalid", "ambiguous", "incomplete"}
	for sentinel, want := range map[error]string{ErrTruncated: "truncated", ErrInvalid: "invalid", ErrAmbiguous: "ambiguous", ErrIncomplete: "incomplete", errors.New("other"): "other"} {
		if got := wording.Of(sentinel); got.Error() != want {
			t.Fatalf("Of(%v) = %v, want %s", sentinel, got, want)
		}
	}
}

func TestParseBootIdentity(t *testing.T) {
	const want = "12345678-9abc-def0-1234-56789abcdef0"
	for input, valid := range map[string]bool{
		want:                                     true,
		"12345678-9ABC-DEF0-1234-56789ABCDEF0\n": true,
		"":                                       false,
		"not-a-uuid":                             false,
		"{12345678-9abc-def0-1234-56789abcdef0}": false,
		"12345678-9abc-def0-1234-56789abcdef0 trailing": false,
	} {
		got, ok := ParseBootIdentity(input)
		if ok != valid || valid && got != want {
			t.Fatalf("ParseBootIdentity(%q) = %q, %v", input, got, ok)
		}
	}
}

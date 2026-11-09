package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"smsarchive/internal/archive"
)

func TestParseListRejectsTruncationAndDuplicateSlots(t *testing.T) {
	header := "+CMGL: 1,1,,2"
	for _, lines := range [][]string{
		{header}, {header, "0001"}, {header, "000102", header, "000102"},
	} {
		if _, err := parseList("SM", lines); err == nil {
			t.Fatalf("accepted malformed listing with %d lines", len(lines))
		}
	}
	slots, err := parseList("SM", []string{header, "000102"})
	if err != nil || len(slots) != 1 || slots[0].Index != 1 {
		t.Fatal("valid listing rejected")
	}
}

func TestCompletionMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "marker.json")
	if completed(path, "a") {
		t.Fatal("missing marker allowed rotation")
	}
	if err := writeJSON(path, completion{1, "a", time.Now()}, true); err != nil {
		t.Fatal(err)
	}
	if !completed(path, "a") || completed(path, "b") {
		t.Fatal("marker was not scoped to its SIM")
	}
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if completed(path, "a") {
		t.Fatal("corrupt marker allowed rotation")
	}
}

type fakeRotationModem struct {
	id            string
	identityReads int
	switchAt      int
	slot          archive.Slot
	deletes       int
}

func (m *fakeRotationModem) identity() (string, error) {
	m.identityReads++
	if m.switchAt > 0 && m.identityReads >= m.switchAt {
		return "replacement", nil
	}
	return m.id, nil
}

func (*fakeRotationModem) selectStore(string) error            { return nil }
func (m *fakeRotationModem) readSIM(int) (archive.Slot, error) { return m.slot, nil }
func (m *fakeRotationModem) deleteSIM(index int) error {
	if index != m.slot.Index {
		return errors.New("wrong index")
	}
	m.deletes++
	return nil
}
func (m *fakeRotationModem) storage() (string, string, occupancy, error) {
	return "SM", "SM", occupancy{1 - m.deletes, 15}, nil
}

func TestRotationRequiresSameSIMAndPDU(t *testing.T) {
	const pdu = "0000039121F30000421120304050000141"
	sim := strings.Repeat("a", 64)
	slot := archive.Slot{Store: "SM", Index: 1, PDU: pdu}
	a, err := archive.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Import(sim, []archive.Slot{slot}, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name        string
		switchAt    int
		pdu         string
		wantDeletes int
	}{
		{"unchanged", 0, pdu, 1},
		{"SIM changed immediately before deletion", 4, pdu, 0},
		{"slot changed", 0, pdu + "00", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &fakeRotationModem{id: sim, switchAt: tc.switchAt, slot: slot}
			m.slot.PDU = tc.pdu
			s := service{status: status{SMUsed: 1}}
			err := s.rotate(m, a, sim, []archive.Slot{slot}, settings{})
			if m.deletes != tc.wantDeletes || (err == nil) != (tc.wantDeletes > 0) {
				t.Fatalf("deletions=%d error=%v", m.deletes, err)
			}
		})
	}
}

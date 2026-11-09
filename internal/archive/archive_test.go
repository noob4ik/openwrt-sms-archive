package archive

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

func samplePDU(senderDigit, day byte, text string, total, part int) string {
	flags := byte(0x04)
	var body []byte
	if total > 1 {
		flags |= 0x40
		body = []byte{5, 0, 3, 42, byte(total), byte(part)}
	}
	for _, word := range utf16.Encode([]rune(text)) {
		body = append(body, byte(word>>8), byte(word))
	}
	raw := []byte{0, flags, 2, 0x91, senderDigit, 0, 8, 0x62, 0x11, day, 0x10, 0x85, 0x95, 0x61, byte(len(body))}
	return strings.ToUpper(hex.EncodeToString(append(raw, body...)))
}

func TestDecodePDU(t *testing.T) {
	pdu := samplePDU(0x21, 0x20, "Привет €", 1, 1)
	d := decodePDU(pdu)
	if !d.decoded || d.sender != "+12" || d.text != "Привет €" {
		t.Fatalf("wrong decoding: %+v", d)
	}
	if got := d.timestamp.Format(time.RFC3339); got != "2026-11-01T21:58:59Z" {
		t.Fatal(got)
	}
	// GSM 7-bit alphabet with padding after the six-byte UDH.
	raw := []byte{0, 0x44, 2, 0x91, 0x21, 0, 0, 0x62, 0x11, 0x20, 0x10, 0x85, 0x95, 0x61, 8, 5, 0, 3, 42, 2, 1, 0x82}
	d = decodePDU(hex.EncodeToString(raw))
	if !d.decoded || d.text != "A" || d.total != 2 || d.sequence != 1 {
		t.Fatalf("wrong GSM decoding: %+v", d)
	}
}

func TestImportAssemblesAndDeduplicates(t *testing.T) {
	root := t.TempDir()
	a, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	sim := digest("card one")
	first := Slot{"SM", 1, samplePDU(0x21, 0x20, "Пе", 2, 1)}
	second := Slot{"SM", 2, samplePDU(0x21, 0x20, "рвое", 2, 2)}
	copy := Slot{"ME", 1, strings.ToLower(second.PDU)}
	now := time.Now()
	stats, err := a.Import(sim, []Slot{second, copy}, now)
	if err != nil || stats.Added != 1 || len(a.Messages) != 1 {
		t.Fatalf("first import: %+v %v", stats, err)
	}
	m := a.Messages[0]
	id, name := m.ID, m.filename()
	groups, err := a.VerifiedGroups(sim, []Slot{second}, nil, nil)
	if err != nil || len(groups) != 0 {
		t.Fatalf("incomplete group was selected: %+v %v", groups, err)
	}
	a, err = Load(root)
	if err != nil {
		t.Fatal(err)
	}
	stats, err = a.Import(sim, []Slot{first, second, copy}, now.Add(time.Second))
	if err != nil || stats.Added != 0 || stats.Updated != 1 || len(a.Messages) != 1 {
		t.Fatalf("second import: %+v %v", stats, err)
	}
	m = a.Messages[0]
	if m.ID != id || m.filename() != name || m.PartsReceived != 2 || m.PartsExpected != 2 || m.Text != "Первое" ||
		!slices.Equal(m.Sources, []string{"ME", "SM"}) {
		t.Fatalf("wrong multipart record: %+v", m)
	}
	stats, err = a.Import(sim, []Slot{first, second, copy}, now.Add(2*time.Second))
	if err != nil || stats != (Stats{}) {
		t.Fatalf("repeated import: %+v %v", stats, err)
	}
	groups, err = a.VerifiedGroups(sim, []Slot{first, second, copy}, nil, nil)
	if err != nil || len(groups) != 1 || len(groups[0].Slots) != 2 {
		t.Fatalf("group not kept together: %+v %v", groups, err)
	}
	if err := os.WriteFile(filepath.Join(root, "messages", name), []byte("damaged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.Verify(sim, second); err == nil {
		t.Fatal("corrupt archive was accepted")
	}
}

func TestModemAttributionAcrossSIMs(t *testing.T) {
	a, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	simA, simB := digest("card one"), digest("card two")
	pdu := samplePDU(0x21, 0x20, "same", 1, 1)
	me, sm := Slot{"ME", 1, pdu}, Slot{"SM", 1, pdu}
	now := time.Now()
	if _, err := a.Import(simA, []Slot{me}, now); err != nil || len(a.Messages) != 1 || a.Messages[0].SIMID != "" {
		t.Fatalf("ME attribution without evidence: %v", err)
	}
	if _, err := a.Import(simA, []Slot{sm, me}, now); err != nil || len(a.Messages) != 1 || a.Messages[0].SIMID != simA ||
		!slices.Equal(a.Messages[0].Sources, []string{"ME", "SM"}) {
		t.Fatalf("ME attribution with evidence: %v", err)
	}
	if _, err := a.Import(simB, []Slot{sm, me}, now); err != nil || len(a.Messages) != 3 {
		t.Fatalf("ME ambiguous attribution: %v", err)
	}
	for _, m := range a.Messages {
		if m.SIMID != "" && !slices.Equal(m.Sources, []string{"SM"}) {
			t.Fatalf("ME assigned to a SIM despite duplicate evidence: %+v", m)
		}
	}
}

func TestGroupsOrderAndAmbiguity(t *testing.T) {
	a, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sim := digest("card one")
	slots := []Slot{
		{"SM", 1, samplePDU(0x21, 0x10, "priority", 1, 1)},
		{"SM", 2, samplePDU(0x43, 0x30, "discard", 1, 1)},
		{"SM", 3, samplePDU(0x65, 0x20, "normal", 1, 1)},
		{"SM", 4, samplePDU(0x21, 0x40, "ambiguous", 2, 1)},
		{"SM", 5, samplePDU(0x21, 0x40, "conflicting", 2, 1)},
		{"SM", 6, samplePDU(0x21, 0x40, "continuation", 2, 2)},
	}
	if _, err := a.Import(sim, slots, time.Now()); err != nil {
		t.Fatal(err)
	}
	groups, err := a.VerifiedGroups(sim, slots, []string{" +12 "}, []string{"+12", "+34"})
	if err != nil || len(groups) != 3 {
		t.Fatalf("unexpected groups: %+v %v", groups, err)
	}
	for i, want := range []int{2, 3, 1} {
		if len(groups[i].Slots) != 1 || groups[i].Slots[0].Index != want {
			t.Fatalf("wrong priority: %+v", groups)
		}
	}
}

package archive

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const (
	schemaVersion = 1
	timePrefix    = "20060102T150405Z"
	concatWindow  = 10 * time.Minute
)

type Slot struct {
	Store string
	Index int
	PDU   string
}

type Group struct {
	Slots []Slot
}

type Stats struct {
	Added   int
	Updated int
}

type Part struct {
	Number  int      `yaml:"number" json:"number"`
	PDU     string   `yaml:"pdu" json:"pdu"`
	Sources []string `yaml:"sources" json:"sources"`
}

type Message struct {
	Version       int        `yaml:"version" json:"version"`
	ID            string     `yaml:"id" json:"id"`
	SIMID         string     `yaml:"sim_id" json:"sim_id"`
	Sender        string     `yaml:"sender" json:"sender"`
	ReceivedAt    *time.Time `yaml:"received_at" json:"received_at"`
	ArchivedAt    time.Time  `yaml:"archived_at" json:"archived_at"`
	DisplayAt     time.Time  `yaml:"display_at" json:"display_at"`
	Text          string     `yaml:"text" json:"text"`
	PartsReceived int        `yaml:"parts_received" json:"parts_received"`
	PartsExpected int        `yaml:"parts_expected" json:"parts_expected"`
	Sources       []string   `yaml:"sources" json:"sources"`
	Ambiguous     bool       `yaml:"ambiguous,omitempty" json:"ambiguous,omitempty"`
	Parts         []Part     `yaml:"parts" json:"parts"`
}

type Archive struct {
	dir      string
	Messages []*Message
}

func Load(root string) (*Archive, error) {
	dir := filepath.Join(root, "messages")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, errors.New("cannot create archive directory")
	}
	for _, path := range []string{root, dir} {
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() {
			return nil, errors.New("invalid archive directory")
		}
		if err := os.Chmod(path, 0700); err != nil {
			return nil, errors.New("cannot restrict archive directory permissions")
		}
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, errors.New("cannot list archive")
	}
	a := &Archive{dir: dir}
	ids := make(map[string]bool)
	for _, file := range files {
		if !strings.HasSuffix(file.Name(), ".yaml") {
			continue
		}
		m, err := readMessage(filepath.Join(dir, file.Name()))
		if err != nil || ids[m.ID] {
			return nil, errors.New("invalid or duplicate archive record")
		}
		ids[m.ID] = true
		a.Messages = append(a.Messages, m)
	}
	return a, nil
}

func (a *Archive) Import(simID string, slots []Slot, now time.Time) (Stats, error) {
	var stats Stats
	if !validID(simID) || now.IsZero() {
		return stats, errors.New("invalid SIM identity or archive time")
	}
	work := &Archive{dir: a.dir}
	previous := make(map[string]*Message, len(a.Messages))
	for _, m := range a.Messages {
		previous[m.ID] = m
		copy := *m
		copy.Parts = slices.Clone(m.Parts)
		for i := range copy.Parts {
			copy.Parts[i].Sources = slices.Clone(m.Parts[i].Sources)
		}
		work.Messages = append(work.Messages, &copy)
	}
	var sim, modem []string
	for _, slot := range slots {
		pdu, err := normalizePDU(slot.PDU)
		if err != nil {
			return stats, err
		}
		switch slot.Store {
		case "SM":
			sim = append(sim, pdu)
		case "ME":
			modem = append(modem, pdu)
		default:
			return stats, errors.New("unexpected SMS storage")
		}
	}
	for _, pdu := range sim {
		work.record(simID, pdu, "SM", now)
	}
	for _, pdu := range modem {
		work.record("", pdu, "ME", now)
	}
	work.reconcileModemSources(now)
	for _, m := range work.Messages {
		m.refresh()
		old := previous[m.ID]
		if !reflect.DeepEqual(old, m) {
			if err := work.write(m); err != nil {
				return Stats{}, err
			}
			if old == nil {
				stats.Added++
			} else {
				stats.Updated++
			}
		}
		delete(previous, m.ID)
	}
	for _, slot := range slots {
		if err := work.Verify(simID, slot); err != nil {
			return Stats{}, err
		}
	}
	// Retire an unknown record only after proving its historical ME
	// observations are still present in the confirmed SIM archive.
	for _, old := range previous {
		for _, part := range old.Parts {
			if err := work.Verify(simID, Slot{Store: "ME", PDU: part.PDU}); err != nil {
				return Stats{}, err
			}
		}
		if err := os.Remove(filepath.Join(work.dir, old.filename())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return Stats{}, errors.New("cannot retire merged archive record")
		}
	}
	if len(previous) != 0 {
		if err := syncDir(work.dir); err != nil {
			return Stats{}, errors.New("cannot sync archive directory")
		}
	}
	a.Messages = work.Messages
	return stats, nil
}

func (a *Archive) record(simID, pdu, source string, now time.Time) {
	if existing := a.find(simID, pdu); existing != nil {
		existing.addSource(pdu, source)
		return
	}
	decoded := decodePDU(pdu)
	var matching []*Message
	if decoded.total > 1 && decoded.timestamp != nil && decoded.decoded {
		for _, m := range a.Messages {
			if m.SIMID == simID && m.compatible(decoded) {
				matching = append(matching, m)
			}
		}
	}
	part := Part{Number: decoded.sequence, PDU: pdu, Sources: []string{source}}
	if len(matching) == 1 && !matching[0].Ambiguous && !matching[0].hasSequence(decoded.sequence) {
		matching[0].Parts = append(matching[0].Parts, part)
		return
	}
	for _, m := range matching {
		m.Ambiguous = true
	}
	m := &Message{
		Version: schemaVersion, ID: digest(simID + ":" + pdu), SIMID: simID,
		Sender: decoded.sender, ReceivedAt: decoded.timestamp, ArchivedAt: now.UTC(),
		DisplayAt: now.UTC(), Parts: []Part{part}, Ambiguous: len(matching) != 0,
	}
	if m.ReceivedAt != nil {
		m.DisplayAt = *m.ReceivedAt
	}
	a.Messages = append(a.Messages, m)
}

func (m *Message) compatible(next decodedPDU) bool {
	for _, part := range m.Parts {
		other := decodePDU(part.PDU)
		if !other.decoded || other.timestamp == nil || other.total != next.total ||
			other.sender != next.sender || other.ref != next.ref || other.dcs != next.dcs ||
			other.timestamp.Sub(*next.timestamp).Abs() > concatWindow {
			return false
		}
	}
	return true
}

func (m *Message) hasSequence(number int) bool {
	return slices.ContainsFunc(m.Parts, func(p Part) bool { return p.Number == number })
}

func (m *Message) refresh() {
	sort.Slice(m.Parts, func(i, j int) bool { return m.Parts[i].Number < m.Parts[j].Number })
	var body strings.Builder
	m.Sources = nil
	m.PartsExpected = 1
	m.PartsReceived = len(m.Parts)
	for _, part := range m.Parts {
		decoded := decodePDU(part.PDU)
		m.PartsExpected = max(m.PartsExpected, decoded.total)
		if decoded.decoded {
			body.WriteString(decoded.text)
		} else {
			body.WriteString("[PDU сохранён, формат текста не поддерживается]")
		}
		for _, source := range part.Sources {
			m.Sources = appendUnique(m.Sources, source)
		}
	}
	m.Text = body.String()
	slices.Sort(m.Sources)
}

func (a *Archive) find(simID, pdu string) *Message {
	for _, m := range a.Messages {
		if m.SIMID == simID && m.hasPDU(pdu) {
			return m
		}
	}
	return nil
}

func (a *Archive) confirmed(pdu string) *Message {
	var match *Message
	for _, m := range a.Messages {
		if m.SIMID != "" && m.hasSource(pdu, "SM") {
			if match != nil {
				return nil
			}
			match = m
		}
	}
	return match
}

func (a *Archive) reconcileModemSources(now time.Time) {
	// A later SIM may reveal another copy of a previously unique PDU.
	// Recover the ME observation as unknown if its attribution becomes
	// ambiguous, retaining both independently confirmed SIM copies.
	for _, m := range slices.Clone(a.Messages) {
		if m.SIMID == "" {
			continue
		}
		for i := range m.Parts {
			part := &m.Parts[i]
			if slices.Contains(part.Sources, "ME") && a.confirmed(part.PDU) != m {
				a.record("", part.PDU, "ME", now)
				part.Sources = []string{"SM"}
			}
		}
	}
	result := make([]*Message, 0, len(a.Messages))
	for _, m := range a.Messages {
		if m.SIMID == "" {
			target := a.confirmed(m.Parts[0].PDU)
			if target != nil && allPartsMatch(a, target, m.Parts) {
				for _, part := range m.Parts {
					target.addSource(part.PDU, "ME")
				}
				continue
			}
		}
		result = append(result, m)
	}
	a.Messages = result
}

func allPartsMatch(a *Archive, target *Message, parts []Part) bool {
	for _, part := range parts {
		if a.confirmed(part.PDU) != target {
			return false
		}
	}
	return true
}

func (m *Message) hasPDU(pdu string) bool {
	return slices.ContainsFunc(m.Parts, func(part Part) bool { return part.PDU == pdu })
}

func (m *Message) hasSource(pdu, source string) bool {
	return slices.ContainsFunc(m.Parts, func(part Part) bool {
		return part.PDU == pdu && slices.Contains(part.Sources, source)
	})
}

func (m *Message) addSource(pdu, source string) {
	for i := range m.Parts {
		if m.Parts[i].PDU == pdu {
			m.Parts[i].Sources = appendUnique(m.Parts[i].Sources, source)
			slices.Sort(m.Parts[i].Sources)
		}
	}
}

func appendUnique(values []string, value string) []string {
	if !slices.Contains(values, value) {
		return append(values, value)
	}
	return values
}

// Verify reads the persisted file again and proves it contains the slot's
// normalized PDU and source. ME may match an unknown or a confirmed record.
func (a *Archive) Verify(simID string, slot Slot) error {
	pdu, err := normalizePDU(slot.PDU)
	if err != nil {
		return err
	}
	var expected *Message
	switch slot.Store {
	case "SM":
		expected = a.find(simID, pdu)
	case "ME":
		expected = a.find("", pdu)
		if expected == nil {
			expected = a.confirmed(pdu)
		}
	}
	if expected == nil {
		return errors.New("SMS missing from archive")
	}
	m, err := readMessage(filepath.Join(a.dir, expected.filename()))
	if err != nil || m.ID != expected.ID || m.SIMID != expected.SIMID || !m.hasSource(pdu, slot.Store) {
		return errors.New("archived SMS does not match the modem")
	}
	return nil
}

func (a *Archive) VerifiedGroups(simID string, slots []Slot, white, black []string) ([]Group, error) {
	type candidate struct {
		message *Message
		slots   []Slot
	}
	var candidates []candidate
	indices := make(map[string]int)
	for _, slot := range slots {
		if slot.Store != "SM" {
			continue
		}
		if err := a.Verify(simID, slot); err != nil {
			return nil, err
		}
		pdu, _ := normalizePDU(slot.PDU)
		m := a.find(simID, pdu)
		if !m.canRotate() {
			continue
		}
		i, ok := indices[m.ID]
		if !ok {
			i = len(candidates)
			indices[m.ID] = i
			candidates = append(candidates, candidate{message: m})
		}
		candidates[i].slots = append(candidates[i].slots, slot)
	}
	rank := func(sender string) int {
		sender = normalizeSender(sender)
		matches := func(value string) bool { return normalizeSender(value) == sender }
		if slices.ContainsFunc(white, matches) {
			return 2
		}
		if slices.ContainsFunc(black, matches) {
			return 0
		}
		return 1
	}
	sort.Slice(candidates, func(i, j int) bool {
		left, right := candidates[i].message, candidates[j].message
		if l, r := rank(left.Sender), rank(right.Sender); l != r {
			return l < r
		}
		if !left.DisplayAt.Equal(right.DisplayAt) {
			return left.DisplayAt.Before(right.DisplayAt)
		}
		return left.ID < right.ID
	})
	var groups []Group
	for _, c := range candidates {
		groups = append(groups, Group{Slots: c.slots})
	}
	return groups, nil
}

func (m *Message) canRotate() bool {
	if m.Ambiguous || m.ReceivedAt == nil || m.PartsReceived != m.PartsExpected {
		return false
	}
	for i, p := range m.Parts {
		decoded := decodePDU(p.PDU)
		if !decoded.decoded || decoded.timestamp == nil || p.Number != i+1 {
			return false
		}
	}
	return true
}

func normalizeSender(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func normalizePDU(pdu string) (string, error) {
	pdu = strings.ToUpper(strings.TrimSpace(pdu))
	if len(pdu) < 4 || len(pdu) > 1024 {
		return "", errors.New("invalid SMS PDU length")
	}
	if _, err := hex.DecodeString(pdu); err != nil {
		return "", errors.New("invalid SMS PDU encoding")
	}
	return pdu, nil
}

func digest(s string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(s)))
}

func validID(s string) bool {
	if len(s) != 64 || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func (m *Message) filename() string {
	return m.DisplayAt.UTC().Format(timePrefix) + "_" + m.ID + ".yaml"
}

func readMessage(path string) (*Message, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 ||
		(runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
		return nil, errors.New("invalid archive file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("cannot read archive file")
	}
	var m Message
	if err := yaml.Unmarshal(data, &m); err != nil || m.Version != schemaVersion ||
		!validID(m.ID) || (m.SIMID != "" && !validID(m.SIMID)) ||
		m.ArchivedAt.IsZero() || m.DisplayAt.IsZero() || len(m.Parts) == 0 ||
		filepath.Base(path) != m.filename() {
		return nil, errors.New("invalid archive contents")
	}
	for _, part := range m.Parts {
		pdu, err := normalizePDU(part.PDU)
		if err != nil || pdu != part.PDU || len(part.Sources) == 0 || part.Number != decodePDU(part.PDU).sequence {
			return nil, errors.New("invalid archived PDU")
		}
		for _, source := range part.Sources {
			if (source != "SM" && source != "ME") || (source == "SM" && m.SIMID == "") {
				return nil, errors.New("invalid archived PDU source")
			}
		}
	}
	return &m, nil
}

func (a *Archive) write(m *Message) error {
	data, err := yaml.Marshal(m)
	if err != nil {
		return errors.New("cannot encode archive record")
	}
	file, err := os.CreateTemp(a.dir, ".message-*")
	if err != nil {
		return errors.New("cannot create archive record")
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return errors.New("cannot persist archive record")
	}
	path := filepath.Join(a.dir, m.filename())
	if err := os.Rename(file.Name(), path); err != nil {
		return errors.New("cannot replace archive record")
	}
	if err := syncDir(a.dir); err != nil {
		return errors.New("cannot sync archive directory")
	}
	readback, err := os.ReadFile(path)
	if err != nil || string(readback) != string(data) {
		return errors.New("archive readback did not match")
	}
	return nil
}

func syncDir(path string) error {
	if runtime.GOOS == "windows" {
		return nil // Production targets Linux. Windows is for unit tests.
	}
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

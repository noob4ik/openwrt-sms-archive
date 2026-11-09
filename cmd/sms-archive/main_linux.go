package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"smsarchive/internal/archive"
)

type status struct {
	State       string     `json:"state"`
	Error       string     `json:"error"`
	ActiveSIM   string     `json:"active_sim"`
	LastAttempt time.Time  `json:"last_attempt"`
	LastSuccess *time.Time `json:"last_success"`
	SMUsed      int        `json:"sm_used"`
	SMTotal     int        `json:"sm_total"`
	MEUsed      int        `json:"me_used"`
	METotal     int        `json:"me_total"`
	KeepSlots   int        `json:"keep_slots"`
	Archived    int        `json:"archived"`
	Imported    int        `json:"imported"`
	Updated     int        `json:"updated"`
	Removed     int        `json:"removed"`
}

type settings struct {
	KeepSlots    int
	White, Black []string
	Valid        bool
}

type service struct {
	root, runtime string
	status        status
}

type rotationModem interface {
	identity() (string, error)
	selectStore(string) error
	readSIM(int) (archive.Slot, error)
	deleteSIM(int) error
	storage() (string, string, occupancy, error)
}

type completion struct {
	Version     int       `json:"version"`
	ID          string    `json:"id"`
	CompletedAt time.Time `json:"completed_at"`
}

type simProfile struct {
	ID string `json:"id"`
}

type webMessage struct {
	ID            string     `json:"id"`
	SIMID         string     `json:"sim_id"`
	Sender        string     `json:"sender"`
	ReceivedAt    *time.Time `json:"received_at"`
	ArchivedAt    time.Time  `json:"archived_at"`
	DisplayAt     time.Time  `json:"display_at"`
	Text          string     `json:"text"`
	PartsReceived int        `json:"parts_received"`
	PartsExpected int        `json:"parts_expected"`
	Sources       []string   `json:"sources"`
}

func main() {
	once := flag.Bool("once", false, "run one archive cycle")
	noDelete := flag.Bool("no-delete", false, "skip all SIM deletion in this invocation")
	flag.Parse()
	s := service{root: "/root/sms-archive", runtime: "/var/run/sms-archive"}
	for _, path := range []string{s.root, filepath.Join(s.root, "sims"), s.runtime} {
		if err := os.MkdirAll(path, 0700); err != nil {
			log.Fatal("cannot create private archive or runtime directory")
		}
	}
	lock, err := os.OpenFile(filepath.Join(s.runtime, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		log.Fatal("cannot open modem lock")
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		log.Fatal("archive service is already running")
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	for {
		err := s.run(*noDelete)
		if err != nil {
			s.status.Error = err.Error()
			if s.status.State != "disconnected" {
				s.status.State = "error"
			}
			log.Print(s.status.Error)
		}
		if err := writeJSON(filepath.Join(s.runtime, "status.json"), s.status, false); err != nil {
			log.Print("cannot publish archive status")
		}
		if *once {
			if err != nil {
				os.Exit(1)
			}
			return
		}
		select {
		case <-stop:
			return
		case <-time.After(30 * time.Second):
		}
	}
}

func (s *service) run(noDelete bool) error {
	now := time.Now().UTC()
	s.status.LastAttempt = now
	s.status.State, s.status.Error, s.status.ActiveSIM = "importing", "", ""
	s.status.Imported, s.status.Updated, s.status.Removed = 0, 0, 0
	a, err := archive.Load(s.root)
	if err != nil {
		return errors.New("cannot load SMS archive")
	}
	if err := s.publish(a); err != nil {
		return err
	}
	m, err := openModem("/dev/ttyACM2")
	if err != nil {
		s.status.State = "disconnected"
		return err
	}
	defer m.file.Close()
	model, err := m.command("AT+CGMM")
	if err != nil || !strings.Contains(strings.Join(model, " "), "L860-GL-16") {
		return errors.New("cannot verify Fibocom modem model")
	}
	id, err := m.identity()
	if err != nil {
		return err
	}
	s.status.ActiveSIM = id
	marker := filepath.Join(s.root, "sims", id+".json")
	initialized := completed(marker, id)
	cfg := readSettings()
	if initialized && !noDelete {
		if err := m.selectIncomingSIM(); err != nil {
			return err
		}
	}
	if _, err := m.command("AT+CMGF=0"); err != nil {
		return err
	}
	sm, smCount, err := m.scan("SM")
	if err != nil {
		return err
	}
	me, meCount, err := m.scan("ME")
	if err != nil {
		return err
	}
	if err := checkSIM(m, id); err != nil {
		return err
	}
	s.status.SMUsed, s.status.SMTotal = smCount.Used, smCount.Total
	s.status.MEUsed, s.status.METotal = meCount.Used, meCount.Total
	s.status.KeepSlots = min(cfg.KeepSlots, smCount.Total)
	slots := append(append([]archive.Slot{}, sm...), me...)
	stats, err := a.Import(id, slots, now)
	if err != nil {
		return errors.New("cannot persist and verify SMS archive")
	}
	s.status.Imported, s.status.Updated = stats.Added, stats.Updated
	for _, slot := range slots {
		if err := a.Verify(id, slot); err != nil {
			return errors.New("cannot verify imported SMS on disk")
		}
	}
	if !initialized {
		if err := verifySnapshot(m, id, sm, me); err != nil {
			return err
		}
		if err := writeJSON(marker, completion{1, id, now}, true); err != nil {
			return errors.New("cannot persist initial import completion")
		}
	}
	if err := s.publish(a); err != nil {
		return err
	}
	if initialized && !noDelete && !cfg.Valid {
		return errors.New("invalid SIM retention configuration; deletion skipped")
	}
	if initialized && !noDelete && smCount.Used > s.status.KeepSlots {
		if err := s.rotate(m, a, id, sm, cfg); err != nil {
			return err
		}
	}
	done := time.Now().UTC()
	s.status.LastSuccess = &done
	if initialized && !noDelete {
		s.status.State = "ready"
	}
	return nil
}

func checkSIM(m interface{ identity() (string, error) }, expected string) error {
	id, err := m.identity()
	if err != nil || id != expected {
		return errors.New("SIM identity changed or could not be verified")
	}
	return nil
}

func verifySnapshot(m *modem, id string, sm, me []archive.Slot) error {
	for _, entry := range []struct {
		store string
		slots []archive.Slot
	}{{"SM", sm}, {"ME", me}} {
		current, _, err := m.scan(entry.store)
		if err != nil || len(current) != len(entry.slots) {
			return errors.New("initial SMS snapshot changed; retrying without deletion")
		}
		old := make(map[int]string, len(entry.slots))
		for _, slot := range entry.slots {
			old[slot.Index] = slot.PDU
		}
		for _, slot := range current {
			if old[slot.Index] != slot.PDU {
				return errors.New("initial SMS snapshot changed; retrying without deletion")
			}
		}
	}
	return checkSIM(m, id)
}

func (s *service) rotate(m rotationModem, a *archive.Archive, id string, slots []archive.Slot, cfg settings) error {
	groups, err := a.VerifiedGroups(id, slots, cfg.White, cfg.Black)
	if err != nil {
		return errors.New("cannot identify verified rotation candidates")
	}
	if err := m.selectStore("SM"); err != nil {
		return err
	}
	verify := func(slot archive.Slot) error {
		if err := checkSIM(m, id); err != nil {
			return err
		}
		current, err := m.readSIM(slot.Index)
		if err != nil || current.PDU != slot.PDU || a.Verify(id, current) != nil {
			return errors.New("SIM slot does not match verified archive; deletion stopped")
		}
		return checkSIM(m, id)
	}
	for _, group := range groups {
		if s.status.SMUsed <= s.status.KeepSlots {
			break
		}
		for _, slot := range group.Slots {
			if err := verify(slot); err != nil {
				return err
			}
		}
		for _, slot := range group.Slots {
			if err := verify(slot); err != nil {
				return err
			}
			if err := m.deleteSIM(slot.Index); err != nil {
				return err
			}
			s.status.Removed++
			s.status.SMUsed--
		}
	}
	selected, _, count, err := m.storage()
	if err != nil || selected != "SM" {
		return errors.New("cannot verify occupancy after SIM rotation")
	}
	s.status.SMUsed, s.status.SMTotal = count.Used, count.Total
	if count.Used > s.status.KeepSlots {
		return errors.New("SIM above target; remaining records are incomplete, ambiguous, or new")
	}
	return nil
}

func completed(path, id string) bool {
	data, err := os.ReadFile(path)
	var marker completion
	return err == nil && json.Unmarshal(data, &marker) == nil &&
		marker.Version == 1 && marker.ID == id && !marker.CompletedAt.IsZero()
}

func (s *service) publish(a *archive.Archive) error {
	entries, err := os.ReadDir(filepath.Join(s.root, "sims"))
	if err != nil {
		return errors.New("cannot list known SIM cards")
	}
	known := make(map[string]bool)
	for _, entry := range entries {
		id, ok := strings.CutSuffix(entry.Name(), ".json")
		if ok && completed(filepath.Join(s.root, "sims", entry.Name()), id) {
			known[id] = true
		}
	}
	tz, _ := exec.Command("uci", "-q", "get", "system.@system[0].zonename").Output()
	messages := make([]webMessage, 0, len(a.Messages))
	for _, m := range a.Messages {
		if m.SIMID != "" {
			known[m.SIMID] = true
		}
		messages = append(messages, webMessage{
			ID: m.ID, SIMID: m.SIMID, Sender: m.Sender,
			ReceivedAt: m.ReceivedAt, ArchivedAt: m.ArchivedAt, DisplayAt: m.DisplayAt,
			Text: m.Text, PartsReceived: m.PartsReceived, PartsExpected: m.PartsExpected,
			Sources: m.Sources,
		})
	}
	sims := make([]simProfile, 0, len(known))
	for id := range known {
		sims = append(sims, simProfile{ID: id})
	}
	sort.Slice(sims, func(i, j int) bool { return sims[i].ID < sims[j].ID })
	sort.Slice(messages, func(i, j int) bool {
		if messages[i].DisplayAt.Equal(messages[j].DisplayAt) {
			return messages[i].ID > messages[j].ID
		}
		return messages[i].DisplayAt.After(messages[j].DisplayAt)
	})
	index := struct {
		Messages []webMessage `json:"messages"`
		SIMs     []simProfile `json:"sims"`
		Timezone string       `json:"timezone"`
	}{messages, sims, strings.TrimSpace(string(tz))}
	if err := writeJSON(filepath.Join(s.runtime, "index.json"), index, false); err != nil {
		return errors.New("cannot publish archive index")
	}
	s.status.Archived = len(a.Messages)
	return nil
}

func readSettings() settings {
	cfg := settings{KeepSlots: 10}
	data, err := exec.Command("ubus", "call", "uci", "get", `{"config":"sms_archive","section":"settings"}`).Output()
	if err != nil {
		return cfg
	}
	var reply struct {
		Values struct {
			Keep  *string  `json:"sim_keep_slots"`
			White []string `json:"whitelist"`
			Black []string `json:"blacklist"`
		} `json:"values"`
	}
	if json.Unmarshal(data, &reply) != nil {
		return cfg
	}
	cfg.White, cfg.Black, cfg.Valid = reply.Values.White, reply.Values.Black, true
	if reply.Values.Keep != nil {
		value, err := strconv.Atoi(*reply.Values.Keep)
		cfg.Valid = err == nil && value >= 0
		if cfg.Valid {
			cfg.KeepSlots = value
		}
	}
	return cfg
}

func writeJSON(path string, value any, durable bool) error {
	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, ".sms-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := json.NewEncoder(file).Encode(value); err != nil {
		return err
	}
	if durable {
		if err := file.Sync(); err != nil {
			return err
		}
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	if !durable {
		return nil
	}
	parent, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer parent.Close()
	if err := parent.Sync(); err != nil {
		return fmt.Errorf("directory sync: %w", err)
	}
	return nil
}

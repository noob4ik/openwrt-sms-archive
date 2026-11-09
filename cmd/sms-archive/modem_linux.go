package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"smsarchive/internal/archive"
)

type modem struct {
	file   *os.File
	reader *bufio.Reader
}

func openModem(path string) (*modem, error) {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NOCTTY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, errors.New("AT port unavailable")
	}
	file := os.NewFile(uintptr(fd), path)
	mode := syscall.Termios{
		Cflag:  syscall.B115200 | syscall.CS8 | syscall.CREAD | syscall.CLOCAL,
		Ispeed: syscall.B115200, Ospeed: syscall.B115200,
	}
	mode.Cc[syscall.VMIN] = 1
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TCSETS, uintptr(unsafe.Pointer(&mode))); errno != 0 {
		file.Close()
		return nil, errors.New("cannot configure AT port")
	}
	if err := file.SetDeadline(time.Now().Add(4 * time.Second)); err != nil {
		file.Close()
		return nil, errors.New("AT port does not support deadlines")
	}
	return &modem{file, bufio.NewReaderSize(file, 4096)}, nil
}

func (m *modem) command(cmd string) ([]string, error) {
	if err := m.file.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return nil, errors.New("cannot set AT deadline")
	}
	if _, err := m.file.WriteString(cmd + "\r"); err != nil {
		return nil, errors.New("AT write failed")
	}
	var lines []string
	for i := 0; i < 512; i++ {
		raw, err := m.reader.ReadSlice('\n')
		if err != nil {
			return nil, errors.New("AT reply timeout or read error")
		}
		line := strings.TrimSpace(string(raw))
		switch {
		case line == "OK":
			return lines, nil
		case line == "ERROR", strings.HasPrefix(line, "+CMS ERROR:"), strings.HasPrefix(line, "+CME ERROR:"):
			return nil, errors.New("modem rejected AT command")
		case line == "", line == cmd, strings.HasPrefix(line, "+CMTI:"):
		default:
			lines = append(lines, line)
		}
	}
	return nil, errors.New("AT reply too long")
}

var iccid = regexp.MustCompile(`^\+CCID:\s*"?([0-9]{18,22})[Ff]*"?$`)

func (m *modem) identity() (string, error) {
	lines, err := m.command("AT+CCID")
	if err == nil {
		for _, line := range lines {
			if match := iccid.FindStringSubmatch(line); match != nil {
				sum := sha256.Sum256([]byte(match[1]))
				return hex.EncodeToString(sum[:]), nil
			}
		}
	}
	return "", errors.New("cannot identify SIM")
}

type occupancy struct{ Used, Total int }

var cpms = regexp.MustCompile(`^\+CPMS:\s*"([A-Z]+)",\s*(\d+),\s*(\d+),\s*"([A-Z]+)",\s*(\d+),\s*(\d+),\s*"([A-Z]+)",\s*(\d+),\s*(\d+)$`)

func (m *modem) storage() (string, string, occupancy, error) {
	lines, err := m.command("AT+CPMS?")
	if err == nil {
		for _, line := range lines {
			if fields := cpms.FindStringSubmatch(line); fields != nil {
				used, e1 := strconv.Atoi(fields[2])
				total, e2 := strconv.Atoi(fields[3])
				if e1 == nil && e2 == nil && used <= total {
					return fields[1], fields[7], occupancy{used, total}, nil
				}
			}
		}
	}
	return "", "", occupancy{}, errors.New("cannot read SMS storage counters")
}

func (m *modem) selectStore(store string) error {
	_, err := m.command("AT+CPMS=\"" + store + "\"")
	return err
}

func (m *modem) selectIncomingSIM() error {
	if _, err := m.command(`AT+CPMS="SM","SM","SM"`); err != nil {
		return err
	}
	_, incoming, _, err := m.storage()
	if err != nil || incoming != "SM" {
		return errors.New("could not verify SIM as incoming SMS storage")
	}
	return nil
}

func (m *modem) scan(store string) ([]archive.Slot, occupancy, error) {
	if err := m.selectStore(store); err != nil {
		return nil, occupancy{}, err
	}
	selected, _, before, err := m.storage()
	if err != nil || selected != store {
		return nil, before, errors.New("cannot verify selected SMS storage")
	}
	lines, err := m.command("AT+CMGL=4")
	if err != nil {
		return nil, before, err
	}
	slots, err := parseList(store, lines)
	if err != nil {
		return nil, before, err
	}
	selected, _, after, err := m.storage()
	if err != nil || selected != store || before != after || len(slots) != after.Used {
		return nil, before, errors.New("SMS storage changed during scan")
	}
	return slots, after, nil
}

func parseList(store string, lines []string) ([]archive.Slot, error) {
	if len(lines)%2 != 0 {
		return nil, errors.New("incomplete SMS listing")
	}
	slots := make([]archive.Slot, 0, len(lines)/2)
	seen := make(map[int]bool)
	for i := 0; i < len(lines); i += 2 {
		header, ok := strings.CutPrefix(lines[i], "+CMGL:")
		fields := strings.Split(header, ",")
		index, err := strconv.Atoi(strings.TrimSpace(fields[0]))
		if !ok || err != nil || index < 1 || seen[index] {
			return nil, errors.New("invalid SMS listing header")
		}
		pdu, err := parsePDU(fields, lines[i+1])
		if err != nil {
			return nil, err
		}
		seen[index] = true
		slots = append(slots, archive.Slot{Store: store, Index: index, PDU: pdu})
	}
	return slots, nil
}

func parsePDU(fields []string, raw string) (string, error) {
	length, err := strconv.Atoi(strings.TrimSpace(fields[len(fields)-1]))
	decoded, decodeErr := hex.DecodeString(raw)
	if err != nil || decodeErr != nil || len(decoded) == 0 || length <= 0 || len(decoded)-1-int(decoded[0]) != length {
		return "", errors.New("invalid or truncated SMS PDU")
	}
	return strings.ToUpper(raw), nil
}

func (m *modem) readSIM(index int) (archive.Slot, error) {
	selected, _, _, err := m.storage()
	if err != nil || selected != "SM" {
		return archive.Slot{}, errors.New("SIM storage selection changed")
	}
	lines, err := m.command(fmt.Sprintf("AT+CMGR=%d", index))
	if err != nil || len(lines) != 2 || !strings.HasPrefix(lines[0], "+CMGR:") {
		return archive.Slot{}, errors.New("cannot verify current SMS slot")
	}
	pdu, err := parsePDU(strings.Split(lines[0], ","), lines[1])
	return archive.Slot{Store: "SM", Index: index, PDU: pdu}, err
}

func (m *modem) deleteSIM(index int) error {
	_, err := m.command(fmt.Sprintf("AT+CMGD=%d", index))
	return err
}

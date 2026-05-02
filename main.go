package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
)

// ---- Pager ----
// The pager is the only layer that touches the file.
// Everything above it asks for pages by number.
type Pager struct {
	file     *os.File
	pageSize uint16
}

func OpenPager(path string) (*Pager, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	var buf [2]byte
	if _, err := f.ReadAt(buf[:], 16); err != nil {
		f.Close()
		return nil, fmt.Errorf("reading page size: %w", err)
	}
	pageSize := binary.BigEndian.Uint16(buf[:])

	return &Pager{file: f, pageSize: pageSize}, nil
}

func (p *Pager) Close() error {
	return p.file.Close()
}

// GetPage returns the raw bytes of a 1-indexed page.
func (p *Pager) GetPage(pageNum uint32) ([]byte, error) {
	buf := make([]byte, p.pageSize)
	offset := int64(pageNum-1) * int64(p.pageSize)
	if _, err := p.file.ReadAt(buf, offset); err != nil {
		return nil, fmt.Errorf("reading page %d: %w", pageNum, err)
	}
	return buf, nil
}

// ---- File header ----
type SQLiteHeader struct {
	HeaderString         [16]byte
	PageSize             uint16
	WriteVersion         uint8
	ReadVersion          uint8
	Reserved             uint8
	MaxEmbeddedPayload   uint8
	MinEmbeddedPayload   uint8
	LeafPayload          uint8
	FileChangeCounter    uint32
	DatabaseSize         uint32
	FreelistTrunkPage    uint32
	TotalFreelistPages   uint32
	SchemaCookie         uint32
	SchemaFormat         uint32
	DefaultPageCacheSize uint32
	LargestRootBTreePage uint32
	TextEncoding         uint32
	UserVersion          uint32
	IncrementalVacuum    uint32
	ApplicationID        uint32
	ReservedExpansion    [20]byte
	VersionValidFor      uint32
	SQLiteVersion        uint32
}

func parseFileHeader(pager *Pager) (*SQLiteHeader, error) {
	page1, err := pager.GetPage(1)
	if err != nil {
		return nil, err
	}
	h := &SQLiteHeader{}
	if err := binary.Read(bytes.NewReader(page1[:100]), binary.BigEndian, h); err != nil {
		return nil, err
	}
	return h, nil
}

// ---- B-tree page header ----
type PageHeader struct {
	PageType            uint8
	FirstFreeBlock      uint16
	CellCount           uint16
	CellContentStart    uint16
	FragmentedFreeBytes uint8
}

func parsePageHeader(data []byte) (*PageHeader, error) {
	h := &PageHeader{}
	if err := binary.Read(bytes.NewReader(data), binary.BigEndian, h); err != nil {
		return nil, err
	}
	return h, nil
}

func readVarint(r io.ByteReader) (uint64, int, error) {
	var result uint64
	for i := 0; i < 9; i++ {
		b, err := r.ReadByte()
		if err != nil {
			return 0, 0, err
		}
		result = (result << 7) | uint64(b&0x7F)
		if b&0x80 == 0 {
			return result, i + 1, nil
		}
	}
	return result, 9, nil
}

func parseRecord(payload []byte) ([]string, error) {
	reader := bytes.NewReader(payload)
	headerSize, n, err := readVarint(reader)
	if err != nil {
		return nil, err
	}

	headerBytesRead := n
	var serialTypes []uint64
	for headerBytesRead < int(headerSize) {
		st, n, err := readVarint(reader)
		if err != nil {
			return nil, err
		}
		serialTypes = append(serialTypes, st)
		headerBytesRead += n
	}

	var values []string
	for _, st := range serialTypes {
		switch {
		case st == 0:
			values = append(values, "")
		case st == 8:
			values = append(values, "0")
		case st == 9:
			values = append(values, "1")
		case st >= 1 && st <= 6:
			sizes := []int{0, 1, 2, 3, 4, 6, 8}
			buf := make([]byte, sizes[st])
			if _, err := reader.Read(buf); err != nil {
				return nil, err
			}
			var val int64
			for _, b := range buf {
				val = (val << 8) | int64(b)
			}
			values = append(values, fmt.Sprintf("%d", val))
		case st == 7:
			buf := make([]byte, 8)
			if _, err := reader.Read(buf); err != nil {
				return nil, err
			}
			values = append(values, "float-placeholder")
		case st >= 13 && st%2 == 1:
			size := (st - 13) / 2
			buf := make([]byte, size)
			if _, err := reader.Read(buf); err != nil {
				return nil, err
			}
			values = append(values, string(buf))
		case st >= 12 && st%2 == 0:
			size := (st - 12) / 2
			buf := make([]byte, size)
			if _, err := reader.Read(buf); err != nil {
				return nil, err
			}
			values = append(values, string(buf))
		default:
			values = append(values, "")
		}
	}
	return values, nil
}

func parseCell(pageBytes []byte, cellOffset uint16) ([]string, error) {
	reader := bytes.NewReader(pageBytes[cellOffset:])
	payloadSize, _, err := readVarint(reader)
	if err != nil {
		return nil, err
	}
	if _, _, err = readVarint(reader); err != nil { // rowid
		return nil, err
	}
	payload := make([]byte, payloadSize)
	if _, err = io.ReadFull(reader, payload); err != nil {
		return nil, err
	}
	return parseRecord(payload)
}

// ---- Schema scanning ----
// Page 1 is the sqlite_schema table. Its B-tree page header starts at byte 100
// (after the 100-byte database file header).
const (
	schemaPage           = 1
	schemaHeaderOffset   = 100
	schemaBTreeHeaderLen = 8
)

func scanSchema(pager *Pager, fn func(columns []string) error) error {
	page1, err := pager.GetPage(schemaPage)
	if err != nil {
		return err
	}
	ph, err := parsePageHeader(page1[schemaHeaderOffset : schemaHeaderOffset+schemaBTreeHeaderLen])
	if err != nil {
		return err
	}
	pointerBase := schemaHeaderOffset + schemaBTreeHeaderLen
	for i := 0; i < int(ph.CellCount); i++ {
		off := pointerBase + i*2
		cellOffset := binary.BigEndian.Uint16(page1[off : off+2])
		cols, err := parseCell(page1, cellOffset)
		if err != nil {
			return err
		}
		if err := fn(cols); err != nil {
			return err
		}
	}
	return nil
}

// ---- Dot-commands ----
func dotDBInfo(pager *Pager) error {
	h, err := parseFileHeader(pager)
	if err != nil {
		return err
	}

	var numTables, numIndexes, numTriggers, numViews, schemaSize int
	if err := scanSchema(pager, func(cols []string) error {
		if len(cols) > 4 {
			schemaSize += len(cols[4])
		}
		if len(cols) > 0 {
			switch cols[0] {
			case "table":
				numTables++
			case "index":
				numIndexes++
			case "view":
				numViews++
			case "trigger":
				numTriggers++
			}
		}
		return nil
	}); err != nil {
		return err
	}

	textEncoding := func(enc uint32) string {
		switch enc {
		case 1:
			return "utf8"
		case 2:
			return "utf16le"
		case 3:
			return "utf16be"
		default:
			return "unknown"
		}
	}

	rows := [][2]string{
		{"database page size", fmt.Sprintf("%v", h.PageSize)},
		{"write format", fmt.Sprintf("%v", h.WriteVersion)},
		{"read format", fmt.Sprintf("%v", h.ReadVersion)},
		{"reserved bytes", fmt.Sprintf("%v", h.Reserved)},
		{"file change counter", fmt.Sprintf("%v", h.FileChangeCounter)},
		{"database page count", fmt.Sprintf("%v", h.DatabaseSize)},
		{"freelist page count", fmt.Sprintf("%v", h.TotalFreelistPages)},
		{"schema cookie", fmt.Sprintf("%v", h.SchemaCookie)},
		{"schema format", fmt.Sprintf("%v", h.SchemaFormat)},
		{"default cache size", fmt.Sprintf("%v", h.DefaultPageCacheSize)},
		{"autovacuum top root", fmt.Sprintf("%v", h.LargestRootBTreePage)},
		{"incremental vacuum", fmt.Sprintf("%v", h.IncrementalVacuum)},
		{"text encoding", fmt.Sprintf("%v (%s)", h.TextEncoding, textEncoding(h.TextEncoding))},
		{"user version", fmt.Sprintf("%v", h.UserVersion)},
		{"application id", fmt.Sprintf("%v", h.ApplicationID)},
		{"software version", fmt.Sprintf("%v", h.SQLiteVersion)},
		{"number of tables", fmt.Sprintf("%v", numTables)},
		{"number of indexes", fmt.Sprintf("%v", numIndexes)},
		{"number of triggers", fmt.Sprintf("%v", numTriggers)},
		{"number of views", fmt.Sprintf("%v", numViews)},
		{"schema size", fmt.Sprintf("%v", schemaSize)},
		{"data version", "1"},
	}

	var maxLen int
	for _, r := range rows {
		if len(r[0]) > maxLen {
			maxLen = len(r[0])
		}
	}
	for _, r := range rows {
		fmt.Printf("%-*s %s\n", maxLen+1, r[0]+":", r[1])
	}
	return nil
}

func dotTables(pager *Pager) error {
	var names []string
	err := scanSchema(pager, func(cols []string) error {
		if len(cols) > 2 && cols[0] == "table" && cols[2] != "sqlite_sequence" {
			names = append(names, cols[2])
		}
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Println(strings.Join(names, " "))
	return nil
}

func runDotCommand(pager *Pager, input string) error {
	fields := strings.Fields(input)
	if len(fields) == 0 {
		return nil
	}
	switch fields[0] {
	case ".dbinfo":
		return dotDBInfo(pager)
	case ".tables":
		return dotTables(pager)
	default:
		return fmt.Errorf("unknown dot-command: %s", fields[0])
	}
}

func execSQL(pager *Pager, query string) error {
	upper := strings.ToUpper(strings.TrimSpace(query))
	switch {
	case strings.HasPrefix(upper, "SELECT COUNT(*) FROM "):
		tableName := strings.TrimSpace(query[len("SELECT COUNT(*) FROM "):])
		return execSelectCount(pager, tableName)
	default:
		return fmt.Errorf("unsupported query: %s", query)
	}
}

func execSelectCount(pager *Pager, tableName string) error {
	var rootPage uint32
	err := scanSchema(pager, func(cols []string) error {
		if rootPage == 0 && len(cols) > 3 && cols[2] == tableName {
			fmt.Sscanf(cols[3], "%d", &rootPage)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if rootPage == 0 {
		return fmt.Errorf("table not found: %s", tableName)
	}

	pageBytes, err := pager.GetPage(rootPage)
	if err != nil {
		return err
	}
	ph, err := parsePageHeader(pageBytes[:schemaBTreeHeaderLen])
	if err != nil {
		return err
	}
	fmt.Println(ph.CellCount)
	return nil
}

func main() {
	if len(os.Args) < 3 {
		log.Fatal("usage: sqlite3 <database> <command|query>")
	}
	dbPath := os.Args[1]
	input := os.Args[2]

	pager, err := OpenPager(dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer pager.Close()

	if strings.HasPrefix(input, ".") {
		err = runDotCommand(pager, input)
	} else {
		err = execSQL(pager, input)
	}
	if err != nil {
		log.Fatal(err)
	}
}

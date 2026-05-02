package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"strings"

	"github.com/farreljordan/sqlite-go/sqlparser"
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
	r := bytes.NewReader(payload)

	headerSize, _, err := readVarint(r)
	if err != nil {
		return nil, err
	}

	headerBytes := make([]byte, headerSize-1)
	if _, err := r.Read(headerBytes); err != nil {
		return nil, err
	}

	hr := bytes.NewReader(headerBytes)

	var serialTypes []uint64
	for hr.Len() > 0 {
		st, _, err := readVarint(hr)
		if err != nil {
			return nil, err
		}
		serialTypes = append(serialTypes, st)
	}

	body := payload[headerSize:]
	pos := 0

	var cols []string

	for _, st := range serialTypes {
		var val string

		switch {
		case st == 0:
			val = "NULL"

		case st == 1:
			val = fmt.Sprintf("%d", int8(body[pos]))
			pos += 1

		case st == 2:
			val = fmt.Sprintf("%d", int16(binary.BigEndian.Uint16(body[pos:])))
			pos += 2

		case st == 3:
			v := int32(body[pos])<<16 | int32(body[pos+1])<<8 | int32(body[pos+2])
			val = fmt.Sprintf("%d", v)
			pos += 3

		case st == 4:
			val = fmt.Sprintf("%d", int32(binary.BigEndian.Uint32(body[pos:])))
			pos += 4

		case st == 5:
			// 6-byte big-endian signed integer
			v := int64(body[pos])<<40 | int64(body[pos+1])<<32 | int64(body[pos+2])<<24 |
				int64(body[pos+3])<<16 | int64(body[pos+4])<<8 | int64(body[pos+5])
			val = fmt.Sprintf("%d", v)
			pos += 6

		case st == 6:
			val = fmt.Sprintf("%d", int64(binary.BigEndian.Uint64(body[pos:])))
			pos += 8

		case st == 7:
			// 8-byte IEEE 754 float
			bits := binary.BigEndian.Uint64(body[pos:])
			f := math.Float64frombits(bits)
			val = fmt.Sprintf("%g", f)
			pos += 8

		case st == 8:
			// integer constant 0, zero bytes
			val = "0"

		case st == 9:
			// integer constant 1, zero bytes
			val = "1"

		case st >= 13 && st%2 == 1:
			size := int((st - 13) / 2)
			val = string(body[pos : pos+size])
			pos += size

		case st >= 12 && st%2 == 0:
			size := int((st - 12) / 2)
			val = fmt.Sprintf("%x", body[pos:pos+size])
			pos += size

		default:
			return nil, fmt.Errorf("unsupported serial type: %d", st)
		}

		cols = append(cols, val)
	}

	return cols, nil
}

func parseCell(page []byte, offset uint16) ([]string, int64, error) {
	i := int(offset)

	r := bytes.NewReader(page[i:])

	payloadSize, n1, err := readVarint(r)
	if err != nil {
		return nil, 0, err
	}
	i += n1

	rowid, n2, err := readVarint(r)
	if err != nil {
		return nil, 0, err
	}
	i += n2

	payload := page[i : i+int(payloadSize)]

	cols, err := parseRecord(payload)
	if err != nil {
		return nil, 0, err
	}

	return cols, int64(rowid), nil
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
		cols, _, err := parseCell(page1, cellOffset)
		if err != nil {
			return err
		}
		if err := fn(cols); err != nil {
			return err
		}
	}
	return nil
}

func scanTable(
	pager *Pager,
	pageNum uint32,
	processRow func(cols []string, rowid int64) error,
) error {
	pageBytes, err := pager.GetPage(pageNum)
	if err != nil {
		return err
	}

	// determine header offset
	headerOffset := 0
	if pageNum == 1 {
		headerOffset = 100
	}

	ph, err := parsePageHeader(pageBytes[headerOffset : headerOffset+8])
	if err != nil {
		return err
	}

	switch ph.PageType {

	case 0x0D: // leaf table page
		pointerBase := headerOffset + 8 // 8-byte header for leaf pages
		for i := 0; i < int(ph.CellCount); i++ {
			ptr := pointerBase + i*2
			cellOffset := binary.BigEndian.Uint16(pageBytes[ptr : ptr+2])

			cols, rowid, err := parseCell(pageBytes, cellOffset)
			if err != nil {
				return err
			}

			if err := processRow(cols, rowid); err != nil {
				return err
			}
		}

	case 0x05: // interior table page — header is 12 bytes (includes 4-byte right-most pointer)
		// Right-most child pointer lives at bytes 8-11 of the page header.
		rightMost := binary.BigEndian.Uint32(pageBytes[headerOffset+8 : headerOffset+12])
		// Cell pointer array starts after the 12-byte interior header.
		pointerBase := headerOffset + 12
		for i := 0; i < int(ph.CellCount); i++ {
			ptr := pointerBase + i*2
			cellOffset := binary.BigEndian.Uint16(pageBytes[ptr : ptr+2])

			// Each interior cell: 4-byte left child page number, then varint key.
			childPage := binary.BigEndian.Uint32(pageBytes[cellOffset : cellOffset+4])

			if err := scanTable(pager, childPage, processRow); err != nil {
				return err
			}
		}

		return scanTable(pager, rightMost, processRow)

	default:
		return fmt.Errorf("unsupported page type: %d", ph.PageType)
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
	stmt, err := sqlparser.Parse(query)
	if err != nil {
		return fmt.Errorf("parse error: %w", err)
	}

	switch s := stmt.(type) {
	case *sqlparser.Select:
		return execSelect(pager, s)
	default:
		return fmt.Errorf("unsupported statement")
	}
}

// TODO: still not using index, in companies.db there is idx_companies_country
func execSelect(pager *Pager, s *sqlparser.Select) error {
	if len(s.SelectExprs) == 1 && sqlparser.IsCountStar(s.SelectExprs[0]) {
		return execSelectCount(pager, s.From.Name)
	}

	var colNames []string

	for _, expr := range s.SelectExprs {
		col, ok := expr.(*sqlparser.ColExpr)
		if !ok {
			return fmt.Errorf("unsupported select expression")
		}
		colNames = append(colNames, col.Name)
	}

	return execSelectCols(pager, s.From.Name, colNames, s.Where)
}

func execSelectCols(pager *Pager, tableName string, colNames []string, where *sqlparser.WhereClause) error {
	var rootPage uint32
	var createSQL string

	if err := scanSchema(pager, func(cols []string) error {
		if len(cols) > 4 && cols[0] == "table" && cols[2] == tableName {
			fmt.Sscanf(cols[3], "%d", &rootPage)
			createSQL = cols[4]
		}
		return nil
	}); err != nil {
		return err
	}

	if rootPage == 0 {
		return fmt.Errorf("table not found: %s", tableName)
	}

	type colRef struct {
		idx     int
		isRowID bool
	}

	colRefs := make([]colRef, len(colNames))
	for i, colName := range colNames {
		idx, err := columnIndex(createSQL, colName)
		if err != nil {
			if strings.Contains(err.Error(), "rowid alias") {
				colRefs[i] = colRef{isRowID: true}
				continue
			}
			return err
		}
		colRefs[i] = colRef{idx: idx}
	}

	var whereRef *struct {
		idx     int
		isRowID bool
		value   string
	}

	if where != nil {
		idx, err := columnIndex(createSQL, where.Column)
		if err != nil {
			if strings.Contains(err.Error(), "rowid alias") {
				whereRef = &struct {
					idx     int
					isRowID bool
					value   string
				}{
					isRowID: true,
					value:   where.Value,
				}
			} else {
				return err
			}
		} else {
			whereRef = &struct {
				idx     int
				isRowID bool
				value   string
			}{
				idx:   idx,
				value: where.Value,
			}
		}
	}

	return scanTable(pager, rootPage, func(cols []string, rowid int64) error {
		if whereRef != nil {
			var v string

			if whereRef.isRowID {
				v = fmt.Sprintf("%d", rowid)
			} else if whereRef.idx < len(cols) {
				v = cols[whereRef.idx]
			} else {
				v = "NULL"
			}

			if v != whereRef.value {
				return nil // skip row
			}
		}

		// SELECT output
		for j, ref := range colRefs {
			if ref.isRowID {
				fmt.Print(rowid)
			} else if ref.idx < len(cols) {
				fmt.Print(cols[ref.idx])
			} else {
				fmt.Print("NULL")
			}

			if j < len(colRefs)-1 {
				fmt.Print("|")
			}
		}
		fmt.Println()

		return nil
	})
}

func splitTopLevel(s string, sep rune) []string {
	var parts []string
	depth, start := 0, 0
	for i, ch := range s {
		switch ch {
		case '(':
			depth++
		case ')':
			depth--
		case sep:
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, s[start:])
}

func extractIdentifier(s string) string {
	s = strings.TrimSpace(s)
	if len(s) == 0 {
		return ""
	}
	switch s[0] {
	case '"', '`', '\'':
		q := s[0]
		if end := strings.IndexByte(s[1:], q); end != -1 {
			return s[1 : end+1]
		}
	case '[':
		if end := strings.IndexByte(s, ']'); end != -1 {
			return s[1:end]
		}
	}
	if f := strings.Fields(s); len(f) > 0 {
		return f[0]
	}
	return ""
}

func columnIndex(createSQL, colName string) (int, error) {
	start := strings.Index(createSQL, "(")
	end := strings.LastIndex(createSQL, ")")
	if start == -1 || end == -1 {
		return -1, fmt.Errorf("cannot parse CREATE TABLE: %s", createSQL)
	}

	defs := splitTopLevel(createSQL[start+1:end], ',')

	idx := 0
	for _, def := range defs {
		def = strings.TrimSpace(def)
		if def == "" {
			continue
		}

		upper := strings.ToUpper(def)

		if strings.HasPrefix(upper, "PRIMARY KEY") ||
			strings.HasPrefix(upper, "UNIQUE") ||
			strings.HasPrefix(upper, "CHECK") ||
			strings.HasPrefix(upper, "FOREIGN KEY") {
			continue
		}

		name := extractIdentifier(def)

		// INTEGER PRIMARY KEY = rowid alias
		if strings.Contains(upper, "INTEGER") && strings.Contains(upper, "PRIMARY KEY") {
			if strings.EqualFold(name, colName) {
				return -1, fmt.Errorf("column %q is the rowid alias; not stored in record", colName)
			}
			idx++ // IMPORTANT: still occupies position
			continue
		}

		if strings.EqualFold(name, colName) {
			return idx, nil
		}

		idx++
	}

	return -1, fmt.Errorf("column %q not found in: %s", colName, createSQL)
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

	var count int64
	if err := scanTable(pager, rootPage, func(_ []string, _ int64) error {
		count++
		return nil
	}); err != nil {
		return err
	}
	fmt.Println(count)
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

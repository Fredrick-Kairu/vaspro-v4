package app

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"

	"github.com/xuri/excelize/v2"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/uploads"
)

// errUnreadable: the file can't be read as its type (VAL-4022).
var errUnreadable = errors.New("the file can't be read")

// rowReader yields a file's rows, header first; io.EOF after the last.
type rowReader interface {
	Next() ([]string, error)
}

// openRows reads r as contentType's rows, streaming. A CSV is read as
// it arrives, with its separator (comma, semicolon or tab) taken from
// the header line. An XLSX is a zip, which can only be read whole: it's
// held in memory, within the upload cap (20 MB), and its first sheet's
// rows are streamed from there.
func openRows(contentType string, r io.Reader) (rowReader, error) {
	switch uploads.ContentType(contentType) {
	case uploads.CSV:
		br := bufio.NewReaderSize(r, 64<<10)
		head, _ := br.Peek(64 << 10)
		if i := bytes.IndexByte(head, '\n'); i >= 0 {
			head = head[:i]
		}
		cr := csv.NewReader(br)
		cr.Comma = separator(head)
		cr.FieldsPerRecord = -1
		cr.LazyQuotes = true
		return csvRows{cr}, nil
	case uploads.XLSX:
		data, err := io.ReadAll(r)
		if err != nil {
			return nil, err
		}
		f, err := excelize.OpenReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("%w: not a workbook", errUnreadable)
		}
		sheets := f.GetSheetList()
		if len(sheets) == 0 {
			return nil, fmt.Errorf("%w: no sheet", errUnreadable)
		}
		rows, err := f.Rows(sheets[0])
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errUnreadable, err)
		}
		return &xlsxRows{f: f, rows: rows}, nil
	}
	return nil, fmt.Errorf("%w: unknown type", errUnreadable)
}

// separator picks whichever of comma, semicolon or tab the header line
// has most of; a comma when it has none.
func separator(header []byte) rune {
	best, n := ',', bytes.Count(header, []byte(","))
	for _, c := range []rune{';', '\t'} {
		if m := bytes.Count(header, []byte(string(c))); m > n {
			best, n = c, m
		}
	}
	return best
}

type csvRows struct{ r *csv.Reader }

func (c csvRows) Next() ([]string, error) {
	rec, err := c.r.Read()
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: %v", errUnreadable, err)
	}
	return rec, err
}

type xlsxRows struct {
	f    *excelize.File
	rows *excelize.Rows
	done bool
}

func (x *xlsxRows) Next() ([]string, error) {
	if x.done || !x.rows.Next() {
		if !x.done {
			x.done = true
			_ = x.rows.Close()
			_ = x.f.Close()
		}
		if err := x.rows.Error(); err != nil {
			return nil, fmt.Errorf("%w: %v", errUnreadable, err)
		}
		return nil, io.EOF
	}
	cols, err := x.rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errUnreadable, err)
	}
	return cols, nil
}

// validUTF8 reports whether every cell is UTF-8 text.
func validUTF8(rec []string) bool {
	for _, c := range rec {
		if !utf8.ValidString(c) {
			return false
		}
	}
	return true
}

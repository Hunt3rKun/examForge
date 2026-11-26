package render

import (
	"errors"
	"fmt"

	"examforge/internal/ir"
)

// gridLayout is a normalized table: every origin cell placed on a rectangular
// grid, with spans defaulted to 1.
type gridLayout struct {
	Columns int
	// Rows lists each row's origin cells in column order. Positions covered by
	// a span are omitted; free holes are padded with explicit empty cells.
	Rows [][]ir.TableCell
}

// buildTableGrid places header and body cells on one rectangular grid. A cell
// starts at the first free slot of its row and its whole span must be free.
// The column count is the widest row after spans. Overlapping spans and spans
// beyond the table are errors: nothing is squeezed or truncated.
func buildTableGrid(t ir.Table) (*gridLayout, error) {
	headers, err := tableHeaderRows(t.Headers)
	if err != nil {
		return nil, err
	}
	rows := make([][]ir.TableCell, 0, len(headers)+len(t.Rows))
	rows = append(rows, headers...)
	rows = append(rows, t.Rows...)
	if len(rows) > ir.MaxTableRows {
		return nil, fmt.Errorf("table has %d rows; limit is %d", len(rows), ir.MaxTableRows)
	}

	type pos struct{ row, col int }
	occupied := make([]bool, len(rows)*ir.MaxTableColumns)
	origin := make(map[pos]ir.TableCell)
	columns := 0
	for r, row := range rows {
		col := 0
		for _, cell := range row {
			if cell.ColSpan < 0 || cell.RowSpan < 0 {
				return nil, fmt.Errorf("cell %q has a negative colspan or rowspan", cell.Text)
			}
			cs, rs := cell.ColSpan, cell.RowSpan
			if cs == 0 {
				cs = 1
			}
			if rs == 0 {
				rs = 1
			}
			cell.ColSpan, cell.RowSpan = cs, rs
			if cs > ir.MaxTableColumns || rs > ir.MaxTableRows {
				return nil, fmt.Errorf("cell %q span %dx%d exceeds limits %dx%d", cell.Text, cs, rs, ir.MaxTableColumns, ir.MaxTableRows)
			}
			if r+rs > len(rows) {
				return nil, fmt.Errorf("cell %q spans %d row(s) but only %d row(s) remain", cell.Text, rs, len(rows)-r)
			}
			for col < ir.MaxTableColumns && occupied[r*ir.MaxTableColumns+col] {
				col++
			}
			if col+cs > ir.MaxTableColumns {
				return nil, fmt.Errorf("cell %q at row %d exceeds %d columns", cell.Text, r, ir.MaxTableColumns)
			}
			for dr := 0; dr < rs; dr++ {
				for dc := 0; dc < cs; dc++ {
					at := (r+dr)*ir.MaxTableColumns + col + dc
					if occupied[at] {
						return nil, fmt.Errorf("cell %q at row %d col %d overlaps a cell spanning into it", cell.Text, r, col)
					}
					occupied[at] = true
				}
			}
			origin[pos{r, col}] = cell
			col += cs
			if col > columns {
				columns = col
			}
		}
	}
	if columns == 0 {
		return nil, errors.New("table has no columns")
	}

	out := &gridLayout{Columns: columns, Rows: make([][]ir.TableCell, len(rows))}
	for r := range rows {
		for col := 0; col < columns; col++ {
			at := pos{r, col}
			if cell, ok := origin[at]; ok {
				out.Rows[r] = append(out.Rows[r], cell)
			} else if !occupied[r*ir.MaxTableColumns+col] {
				// A ragged hole becomes an explicit empty cell. A slot covered
				// by a span is omitted: Typst's table skips it.
				out.Rows[r] = append(out.Rows[r], ir.TableCell{ColSpan: 1, RowSpan: 1})
			}
		}
	}
	return out, nil
}

// tableHeaderRows returns the normalized header rows. Decode-time
// TableHeaders.UnmarshalJSON already accepted both exchange shapes.
func tableHeaderRows(headers ir.TableHeaders) ([][]ir.TableCell, error) {
	return headers, nil
}

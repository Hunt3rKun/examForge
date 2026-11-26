package render

import (
	"encoding/json"
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

	type pos struct{ row, col int }
	occupied := make(map[pos]bool)
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
			if r+rs > len(rows) {
				return nil, fmt.Errorf("cell %q spans %d row(s) but only %d row(s) remain", cell.Text, rs, len(rows)-r)
			}
			for occupied[pos{r, col}] {
				col++
			}
			for dr := 0; dr < rs; dr++ {
				for dc := 0; dc < cs; dc++ {
					at := pos{r + dr, col + dc}
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
			} else if !occupied[at] {
				// A ragged hole becomes an explicit empty cell. A slot covered
				// by a span is omitted: Typst's table skips it.
				out.Rows[r] = append(out.Rows[r], ir.TableCell{ColSpan: 1, RowSpan: 1})
			}
		}
	}
	return out, nil
}

// tableHeaderRows normalizes the two schema shapes of table headers, one flat
// row of cells or several rows, into a list of rows. It accepts both the
// JSON-decoded and the typed Go shape.
func tableHeaderRows(headers any) ([][]ir.TableCell, error) {
	switch h := headers.(type) {
	case nil:
		return nil, nil
	case []ir.TableCell:
		return [][]ir.TableCell{h}, nil
	case [][]ir.TableCell:
		return h, nil
	case []any:
		if len(h) == 0 {
			return nil, nil
		}
		if _, ok := toCell(h[0]); ok {
			row, err := headerRow(h)
			if err != nil {
				return nil, err
			}
			return [][]ir.TableCell{row}, nil
		}
		rows := make([][]ir.TableCell, 0, len(h))
		for i, item := range h {
			if typed, ok := item.([]ir.TableCell); ok {
				rows = append(rows, typed)
				continue
			}
			items, ok := item.([]any)
			if !ok {
				return nil, fmt.Errorf("headers[%d] is neither a cell nor a row of cells", i)
			}
			row, err := headerRow(items)
			if err != nil {
				return nil, fmt.Errorf("headers[%d]: %w", i, err)
			}
			rows = append(rows, row)
		}
		return rows, nil
	}
	return nil, errors.New("headers must be a row of cells or a list of rows")
}

func headerRow(items []any) ([]ir.TableCell, error) {
	row := make([]ir.TableCell, 0, len(items))
	for i, item := range items {
		cell, ok := toCell(item)
		if !ok {
			return nil, fmt.Errorf("item %d is not a table cell", i)
		}
		row = append(row, cell)
	}
	return row, nil
}

// toCell converts one decoded cell into the typed DTO.
func toCell(v any) (ir.TableCell, bool) {
	switch c := v.(type) {
	case ir.TableCell:
		return c, true
	case map[string]any:
		data, err := json.Marshal(c)
		if err != nil {
			return ir.TableCell{}, false
		}
		var cell ir.TableCell
		if err := json.Unmarshal(data, &cell); err != nil {
			return ir.TableCell{}, false
		}
		return cell, true
	}
	return ir.TableCell{}, false
}

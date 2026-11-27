// Package testpdf builds a minimal, valid multi-page PDF programmatically for
// tests. The generator is deterministic (no timestamps or random IDs), so the
// same inputs always produce the same bytes and digest.
package testpdf

import (
	"bytes"
	"fmt"
)

// Build returns a PDF with pageCount pages of the given size in points. Page
// n paints a solid rectangle of color [r,g,b] (0..1) covering the whole page,
// so a rasterized page has a predictable color for pixel assertions.
func Build(pageCount, widthPt, heightPt int, colors [][3]float64) []byte {
	if pageCount < 1 {
		panic("testpdf: need at least one page")
	}
	for len(colors) < pageCount {
		colors = append(colors, [3]float64{1, 1, 1})
	}
	var buf bytes.Buffer
	offsets := []int{0}
	write := func(s string) {
		offsets = append(offsets, buf.Len())
		buf.WriteString(s)
	}
	buf.WriteString("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")

	// Object numbering: 1 catalog, 2 pages tree, then per page: page object,
	// content stream. Kids are 3, 5, 7, ...
	kids := ""
	for i := 0; i < pageCount; i++ {
		kids += fmt.Sprintf("%d 0 R ", 3+2*i)
	}
	write("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")
	write(fmt.Sprintf("2 0 obj\n<< /Type /Pages /Kids [%s] /Count %d >>\nendobj\n", kids, pageCount))
	for i := 0; i < pageCount; i++ {
		pageObj := 3 + 2*i
		contentObj := pageObj + 1
		c := colors[i]
		stream := fmt.Sprintf("%f %f %f rg 0 0 %d %d re f\n", c[0], c[1], c[2], widthPt, heightPt)
		write(fmt.Sprintf("%d 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %d %d] /Contents %d 0 R >>\nendobj\n",
			pageObj, widthPt, heightPt, contentObj))
		write(fmt.Sprintf("%d 0 obj\n<< /Length %d >>\nstream\n%sendstream\nendobj\n", contentObj, len(stream), stream))
	}
	xref := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n", len(offsets))
	buf.WriteString("0000000000 65535 f \n")
	for _, off := range offsets[1:] {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets), xref)
	return buf.Bytes()
}

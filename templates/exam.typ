// ExamForge exam template (MVP slice 1).
//
// This template is declarative: it consumes payload.json written by the Go
// precompiler (internal/render) and only lays it out. It never parses Markdown,
// $LaTeX$ or {{blank:N}}, never reads the raw Exam IR or Layout Spec JSON,
// never resolves asset IDs and holds no regex logic. Every layout number comes
// precomputed in payload.layout.
#import "@preview/mitex:0.2.7": mi, mitex

#let payload = json("payload.json")
#let exam = payload.exam
#let page-layout = payload.layout

// ── Inline: Token type -> style ─────────────────────────────────────────────
#let answer-fill = rgb("#b03a2e")
#let analysis-fill = rgb("#1f5f9e")

#let answer-blank(n) = {
  // The blank's number stays queryable: metadata keeps the index semantic.
  metadata((kind: "blank", index: n))
  underline(offset: 3pt, stroke: 0.5pt, h(page-layout.blank_width_mm * 1mm))
}

#let render-inline(tokens) = {
  for token in tokens {
    if token.type == "text" { token.text }
    else if token.type == "math_inline" { mi(token.text) }
    else if token.type == "bold" { strong(token.text) }
    else if token.type == "italic" { emph(token.text) }
    else if token.type == "underline" { underline(token.text) }
    else if token.type == "blank" { answer-blank(token.index) }
  }
}

// ── Page ────────────────────────────────────────────────────────────────────
#let running-line(tokens) = align(
  center,
  text(size: 9pt, render-inline(tokens)),
)

// The binding line lives in the left margin reserve and never joins the flow.
// It is drawn in the page background, so it repeats on every page and keeps
// its position with any paper size and column count.
#let vertical-text(body) = rotate(
  -90deg,
  reflow: true,
  box(fill: white, inset: (x: 3pt), text(size: 9pt, body)),
)

#let binding-line() = {
  let reserve = page-layout.binding_reserve_mm * 1mm
  place(
    left + top,
    dx: page-layout.margin.left_mm * 1mm - reserve,
    block(width: reserve, height: 100%, {
      place(center + horizon, line(angle: 90deg, length: 100%, stroke: 0.6pt + gray.darken(40%)))
      place(top + center, dy: 8%, vertical-text[密封线内不要答题])
      place(top + center, dy: 34%, vertical-text[学校：＿＿＿＿＿＿　姓名：＿＿＿＿＿＿])
      place(top + center, dy: 56%, vertical-text[班级：＿＿＿＿＿＿　考号：＿＿＿＿＿＿])
    }),
  )
}

#set page(
  paper: page-layout.typst_paper,
  margin: (
    left: page-layout.margin.left_mm * 1mm,
    right: page-layout.margin.right_mm * 1mm,
    top: page-layout.margin.top_mm * 1mm,
    bottom: page-layout.margin.bottom_mm * 1mm,
  ),
  columns: page-layout.columns,
  header: if page-layout.at("header", default: none) != none { running-line(page-layout.header.tokens) } else { none },
  footer: if page-layout.at("footer", default: none) != none { running-line(page-layout.footer.tokens) } else { none },
  background: if page-layout.show_binding_line { binding-line() } else { none },
)

#set text(font: ("Noto Sans CJK SC",), lang: "zh", size: 10.5pt)
#set par(justify: true, leading: 0.75em)
#set columns(gutter: page-layout.gutter_mm * 1mm)
#set table(stroke: 0.5pt + gray.darken(40%), inset: 4pt)
#set grid(inset: 0pt, stroke: none)

// ── Blocks ──────────────────────────────────────────────────────────────────
#let picture(block) = image(block.src, width: block.width_mm * 1mm)

#let render-table(t) = table(
  columns: t.columns,
  align: center + horizon,
  ..t.rows.flatten().map(cell => table.cell(
    colspan: cell.colspan,
    rowspan: cell.rowspan,
    render-inline(cell.tokens),
  )),
)

#let render-block(block) = {
  if block.kind == "paragraph" { par(render-inline(block.tokens)) }
  else if block.kind == "math_block" { align(center, mitex(block.latex)) }
  else if block.kind == "image" {
    let align-to = block.at("align", default: none)
    if align-to == "right" { align(right, picture(block)) }
    else if align-to == "center" { align(center, picture(block)) }
    else { picture(block) }
  }
  else if block.kind == "float_group" {
    // Local side-by-side: the image only narrows the paragraphs grouped with it.
    grid(
      columns: (1fr, block.width_mm * 1mm),
      column-gutter: page-layout.float_gap_mm * 1mm,
      align: (left + top, right + top),
      // Text beside the image is a column of its own, without stem indents
      // and without justification stretching: an array here would print as
      // code, so the rendered blocks are joined into one content value.
      { set par(hanging-indent: 0pt, justify: false); block.blocks.map(render-block).join() },
      picture(block),
    )
  }
  else if block.kind == "table" { render-table(block.table) }
  else {
    // Unknown block: a visible placeholder, reported by a warning as well.
    text(fill: gray.darken(40%), size: 9pt)[［未支持的内容块：#block.source］]
  }
}

// ── Questions ───────────────────────────────────────────────────────────────
#let alnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// number-label and number-indent are shared by the measure and render passes,
// so the measured width is the width the options really get.
#let number-label(q) = if q.display_number == "" { none } else { strong([#q.display_number.]) }
// Absolute on purpose: widths must stay single-unit lengths to become numbers.
#let number-gap = 5pt
#let number-indent(q) = if number-label(q) == none { 0pt } else { measure(number-label(q)).width + number-gap }

// A label keeps its own punctuation; a bare "A" becomes "A.".
#let label-punctuation(label) = {
  let flat = label.tokens.map(t => t.text).join()
  if flat == "" or not alnum.contains(flat.clusters().last()) { "" } else { "." }
}

#let option-cell(opt) = [
  #strong[#render-inline(opt.label.tokens)#label-punctuation(opt.label)]#h(0.35em)
  // Option content stays inline: a paragraph is a run of text inside the cell.
  #for block in opt.content {
    if block.kind == "paragraph" { render-inline(block.tokens) }
    else { render-block(block) }
  }
]

#let option-grid(q, indent) = {
  let options = q.at("options", default: ())
  // One uniform grid: rows fill left to right and a short row stays empty.
  pad(
    left: indent,
    grid(
      columns: (1fr,) * q.option_columns,
      column-gutter: page-layout.option_gutter_mm * 1mm,
      row-gutter: 0.4em,
      align: left + top,
      ..options.map(option-cell),
    ),
  )
}

// The first stem paragraph continues on the number's line; the rest are blocks.
// The hanging indent is scoped to the stem only: continuation lines align
// under the stem text, beside the number.
#let stem(q, label, indent) = {
  let first = if q.stem.len() > 0 { q.stem.first() } else { none }
  let lead = if label != none { [#label#h(number-gap)] }
  {
    set par(hanging-indent: indent)
    if first != none and first.kind == "paragraph" {
      [#lead#render-inline(first.tokens)]
      for block in q.stem.slice(1) { render-block(block) }
    }
    else {
      lead
      for block in q.stem { render-block(block) }
    }
  }
}

#let labeled-part(title, fill, head, blocks, indent) = block(
  above: 0.4em,
  below: 0.2em,
  pad(left: indent)[
    #text(weight: "bold", fill: fill)[#title]
    #head
    #for block in blocks { render-block(block) }
  ],
)

#let answer-part(answer, indent) = labeled-part(
  [【参考答案】],
  answer-fill,
  if answer.at("short_values", default: none) == none { none } else {
    answer.short_values.map(item => render-inline(item.tokens)).join("、")
  },
  answer.at("detail", default: ()),
  indent,
)

#let analysis-part(blocks, indent) = labeled-part([【解析】], analysis-fill, none, blocks, indent)

#let render-question(q, depth) = context {
  let indent = number-indent(q)
  pad(left: depth * page-layout.child_indent_mm * 1mm, [
    #stem(q, number-label(q), indent)
    #if q.at("options", default: ()).len() > 0 [
      #v(0.4em)
      #option-grid(q, indent)
    ]
    // Answer space is the precomputed reserve_space_mm: zero means no space.
    #let reserve = q.at("reserve_space_mm", default: 0)
    #if reserve > 0 { v(reserve * 1mm) }
    #if q.at("answer", default: none) != none { answer-part(q.answer, indent) }
    #if q.at("analysis", default: none) != none { analysis-part(q.analysis, indent) }
    #for child in q.at("children", default: ()) { render-question(child, depth + 1) }
    #v(0.8em)
  ])
}

// ── Measure pass ─────────────────────────────────────────────────────────────
// Each option group whose grid is "auto" is measured once, as finally laid out
// (text, MiTeX and images). The widths go back to Go, which applies the uniform
// grid ladder. The pass output is a probe document, not an exam.
#let measure-question(q, depth) = context {
  if q.option_requested == "auto" and q.at("options", default: ()).len() > 0 {
    // Same widths the render pass pads with: the column minus both indents.
    let indent = number-indent(q) + depth * page-layout.child_indent_mm * 1mm
    let avail = page-layout.column_width_mm * 1mm - indent
    let options = q.at("options", default: ())
    let widths = options.map(opt => measure(option-cell(opt), width: avail).width)
    // Lengths become plain numbers of points: JSON cannot carry Typst lengths.
    metadata((kind: "option_width", group: q.index, wmax: calc.max(..widths) / 1pt, avail: avail / 1pt))
  }
  for child in q.at("children", default: ()) { measure-question(child, depth + 1) }
}

// ── Document ────────────────────────────────────────────────────────────────
#let title-block() = block(above: 0pt, below: 4mm)[
  #align(center)[
    #text(size: 18pt, weight: "bold")[#exam.title]
    #if exam.at("subtitle", default: none) != none [
      #v(0.4em)
      #linebreak()
      #text(size: 12pt)[#exam.subtitle]
    ]
  ]
]

#if payload.pass == "measure" {
  for q in exam.questions { measure-question(q, 0) }
} else {
  if page-layout.columns == 1 { title-block() }
  // A two-column title spans the page: a page-wide float above the columns.
  else { place(top + center, float: true, scope: "parent", title-block()) }
  for q in exam.questions { render-question(q, 0) }
}

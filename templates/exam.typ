// Stage 0 minimal smoke template only. Inline Markdown / $LaTeX$ / {{blank:N}}
// parsing belongs in Go internal/ir/inline (MVP slice 1); no complex regex parsing here.
#let data = json(sys.inputs.data)
#let layout = json(sys.inputs.layout)
#set page(
  paper: layout.paper_size,
  columns: layout.columns,
  header: if "header_text" in layout { layout.header_text } else { none },
  footer: if "footer_text" in layout { layout.footer_text } else { none },
  margin: (x: 18mm, y: 20mm),
)
#set text(font: "Noto Sans CJK SC", lang: "zh")
#set par(justify: true)

#let render-block(block) = {
  if block.type == "text" { [#block.markdown] }
  else if block.type == "math_block" { $#block.latex$ }
  else if block.type == "image_ref" {
    let asset = data.assets.at(block.asset_id)
    image("../" + asset.uri, width: 32mm)
  }
  else if block.type == "table" {
    let cells = block.table.headers.flatten() + block.table.rows.flatten()
    table(
      columns: 3,
      ..cells.map(cell => table.cell(
        colspan: if "colspan" in cell { cell.colspan } else { 1 },
        rowspan: if "rowspan" in cell { cell.rowspan } else { 1 },
      )[#cell.text]),
    )
  }
  else { text(fill: gray)[[unsupported block: #block.type]] }
}

#let render-question(q, layout) = [
  #strong[#q.display_number.]
  #for block in q.stem { render-block(block) }
  #if "options" in q [
    #for option in q.options [
      #h(8pt)
      #strong[#option.label.]
      #for block in option.content { render-block(block) }
    ]
  ]
  #if "children" in q [
    #for child in q.children { render-question(child, layout) }
  ]
  #if layout.show_answer and "answer" in q [
    #text(fill: blue)[答案：#if "short_values" in q.answer [#q.answer.short_values.join("、")]]
    #if "detail" in q.answer [#for block in q.answer.detail { render-block(block) }]
  ] else if "answer_space_mm" in q and q.answer_space_mm > 0 [
    #v(q.answer_space_mm * 1mm)
  ]
  #if layout.show_analysis and "analysis" in q [
    #text(fill: green)[解析：#for block in q.analysis { render-block(block) }]
  ]
  #v(6pt)
]

#align(center)[
  #text(size: 18pt, weight: "bold")[#data.metadata.title]
  #if "subtitle" in data.metadata [#linebreak(); #data.metadata.subtitle]
]
#v(8pt)
#for question in data.questions { render-question(question, layout) }

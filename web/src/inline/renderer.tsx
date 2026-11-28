import katex from "katex";
import { Fragment } from "react";
import { parseInline } from "./parser";

export function MathPreview({latex, displayMode = false}: {latex: string; displayMode?: boolean}): JSX.Element {
  try {
    const html = katex.renderToString(latex, {displayMode, throwOnError: false, trust: false, maxExpand: 1000, maxSize: 10, strict: "ignore"});
    // Only KaTeX output is HTML. Errors and all other content use text nodes.
    if (!html.includes('class="katex-error"')) return <span dangerouslySetInnerHTML={{__html: html}} />;
  } catch { /* fall back to literal text */ }
  return <span title="Formula could not be rendered">{latex}</span>;
}

export function InlinePreview({source}: {source: string}): JSX.Element {
  const {tokens, diagnostics} = parseInline(source);
  return <span>{tokens.map((token, i) => {
    switch (token.type) {
      case "text": return <Fragment key={i}>{token.text}</Fragment>;
      case "math_inline": return <MathPreview key={i} latex={token.text} />;
      case "bold": return <strong key={i}>{token.text}</strong>;
      case "italic": return <em key={i}>{token.text}</em>;
      case "underline": return <u key={i}>{token.text}</u>;
      case "blank": return <span key={i} aria-label={`Blank ${token.index}`}>____</span>;
    }
  })}{diagnostics.length > 0 && <span role="note" title={diagnostics.map(d => d.message).join("; ")}> Invalid inline markup</span>}</span>;
}

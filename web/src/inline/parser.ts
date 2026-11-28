export type InlineToken = {type: "text" | "math_inline" | "bold" | "italic" | "underline"; text: string} | {type: "blank"; index: number | bigint};
export type InlineDiagnostic = {code: string; message: string; start_byte: number; end_byte: number};
const escape = (char: string | undefined) => char === "\\" || char === "$" || char === "*" || char === "_";
const prefix = "{{blank:";

// Work on UTF-8 bytes to match the server's diagnostic offsets and blank limit.
export function parseInline(source: string | Uint8Array): {tokens: InlineToken[]; diagnostics: InlineDiagnostic[]} {
  const bytes = typeof source === "string" ? new TextEncoder().encode(source) : source;
  const diagnostics: InlineDiagnostic[] = [];
  const tokens: InlineToken[] = [];
  const decode = new TextDecoder("utf-8", {ignoreBOM: true});
  const fatal = new TextDecoder("utf-8", {fatal: true, ignoreBOM: true});
  const chars = Array.from(bytes, b => String.fromCharCode(b)).join("");
  function diag(code: string, start_byte: number, end_byte: number, message: string) { diagnostics.push({code, message, start_byte, end_byte}); }
  let text: number[] = [];
  function emit(type: "text" | "bold" | "italic" | "underline") { if (text.length) tokens.push({type, text: decode.decode(Uint8Array.from(text))}); text = []; }
  function append(start: number, end: number) { for (let i = start; i < end; i++) text.push(bytes[i]); }
  for (let pos = 0, invalid = -1; pos <= bytes.length;) {
    const b = bytes[pos];
    const width = b < 128 ? 1 : b >= 194 && b <= 223 ? 2 : b >= 224 && b <= 239 ? 3 : b >= 240 && b <= 244 ? 4 : 0;
    let valid = false;
    if (width && pos + width <= bytes.length) { try { fatal.decode(bytes.subarray(pos, pos + width)); valid = true; } catch { /* invalid byte */ } }
    if (valid || pos === bytes.length) {
      if (invalid !== -1) diag("invalid_utf8", invalid, pos, "Invalid UTF-8 bytes replaced with U+FFFD");
      if (pos === bytes.length) break;
      invalid = -1; pos += width;
    } else { if (invalid === -1) invalid = pos; pos++; }
  }
  const mathClose = (start: number) => { for (let p = start; p < chars.length; p++) { if (chars[p] === "\\" && p + 1 < chars.length) p++; else if (chars[p] === "$") return p; } return -1; };
  const math = (i: number, end: number, style: "text" | "bold" | "italic" | "underline") => {
    if (end === i + 1) { diag("empty_span", i, end + 1, "Empty math span"); append(i, end + 1); }
    else { emit(style); tokens.push({type: "math_inline", text: decode.decode(bytes.subarray(i + 1, end))}); }
  };
  for (let i = 0; i < chars.length;) {
    const c = chars[i]; const next = chars[i + 1];
    if (c === "\\" && escape(next)) { append(i + 1, i + 2); i += 2; }
    else if (chars.startsWith(prefix, i)) {
      let j = i + prefix.length;
      while (j < Math.min(i + 64, chars.length) && !["}", "\n", "\r"].includes(chars[j])) j++;
      const closed = chars.startsWith("}}", j); const end = j + (closed ? 2 : 0);
      const digits = chars.slice(i + prefix.length, j);
      const n = closed && /^[0-9]+$/.test(digits) ? BigInt(digits) : 0n;
      if (n > 0n && n <= 9223372036854775807n) { emit("text"); tokens.push({type: "blank", index: n <= BigInt(Number.MAX_SAFE_INTEGER) ? Number(n) : n}); }
      else { diag("invalid_blank", i, end, "Expected {{blank:N}} with a positive integer"); append(i, end); }
      i = end;
    } else if (c === "$") {
      const end = mathClose(i + 1);
      if (end < 0) { diag("unclosed_math", i, i + 1, "Unclosed math span"); append(i, ++i); }
      else { math(i, end, "text"); i = end + 1; }
    } else if (c === "*" || (c === "_" && next === "_")) {
      const width = c === next ? 2 : 1;
      const style = c === "_" ? "underline" : width === 2 ? "bold" : "italic";
      let p = i + width;
      let closer = -1;
      for (; p < chars.length; p++) {
        if (chars[p] === "\\" && escape(chars[p + 1])) p++;
        else if (chars[p] === "$") { p = mathClose(p + 1); if (p < 0) break; }
        else if (chars[p] === c) {
          if (width === 2) { if (chars[p + 1] === c) { closer = p; break; } }
          else if (chars[p + 1] === c) p++;
          else { closer = p; break; }
        }
      }
      if (closer < 0) { diag(`unclosed_${style}`, i, i + width, `Unclosed ${style} span`); append(i, i + width); i += width; }
      else if (closer === i + width) { diag("empty_span", i, closer + width, `Empty ${style} span`); append(i, closer + width); i = closer + width; }
      else {
        emit("text");
        for (let j = i + width; j < closer;) {
          if (chars[j] === "\\" && j + 1 < closer && escape(chars[j + 1])) { append(j + 1, j + 2); j += 2; }
          else if (chars[j] === "$") { const end = mathClose(j + 1); math(j, end, style); j = end + 1; }
          else append(j, ++j);
        }
        emit(style); i = closer + width;
      }
    } else append(i, ++i);
  }
  emit("text");
  return {tokens, diagnostics: diagnostics.sort((a, b) => a.start_byte - b.start_byte)};
}

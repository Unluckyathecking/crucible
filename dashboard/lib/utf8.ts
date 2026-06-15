// truncateUtf8Buffer returns a UTF-8 string from buf truncated to at most
// maxBytes at a complete codepoint boundary. It walks backward past UTF-8
// continuation bytes to find the preceding start byte, then checks whether
// the full sequence fits in [0, maxBytes). If it fits, all bytes of the
// sequence are included; if not, the start byte is excluded so the output
// never contains a partial sequence.
//
// Only valid UTF-8 lead byte ranges are accepted: 0xC2-0xDF (2-byte),
// 0xE0-0xEF (3-byte), 0xF0-0xF4 (4-byte). Bytes 0xC0-0xC1 are overlong
// encodings and 0xF5-0xFF are out-of-range; both are excluded by dropping
// the lead byte rather than treating them as valid sequence starters.
export function truncateUtf8Buffer(buf: Buffer, maxBytes: number): string {
  if (maxBytes <= 0) return "";
  if (buf.length <= maxBytes) return buf.toString("utf8");
  let end = maxBytes;
  // Walk back past continuation bytes (high two bits = 0b10 = 0x80–0xBF).
  while (end > 0 && (buf[end - 1] & 0xc0) === 0x80) end--;
  // If we stopped at a multi-byte lead byte, verify its full sequence fits.
  if (end > 0 && (buf[end - 1] & 0x80) !== 0) {
    const lead = buf[end - 1];
    if (lead >= 0xc2 && lead <= 0xdf) {
      // 2-byte sequence
      end = (end - 1 + 2 <= maxBytes) ? end - 1 + 2 : end - 1;
    } else if (lead >= 0xe0 && lead <= 0xef) {
      // 3-byte sequence
      end = (end - 1 + 3 <= maxBytes) ? end - 1 + 3 : end - 1;
    } else if (lead >= 0xf0 && lead <= 0xf4) {
      // 4-byte sequence
      end = (end - 1 + 4 <= maxBytes) ? end - 1 + 4 : end - 1;
    } else {
      // Invalid lead byte (overlong 0xC0-0xC1 or out-of-range 0xF5-0xFF): exclude it.
      // The outer loop stopped AT this byte (it is not a continuation byte), so bytes
      // BEFORE it have not been inspected. After dropping the invalid lead via end--,
      // walk back past any preceding continuation bytes that are now orphaned.
      end--;
      while (end > 0 && (buf[end - 1] & 0xc0) === 0x80) end--;
    }
  }
  return buf.toString("utf8", 0, end);
}

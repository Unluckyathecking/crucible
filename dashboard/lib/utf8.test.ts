import { describe, it, expect } from "vitest";
import { truncateUtf8Buffer } from "./utf8";

function buf(s: string): Buffer { return Buffer.from(s, "utf8"); }
function bufHex(hex: string): Buffer { return Buffer.from(hex, "hex"); }

describe("truncateUtf8Buffer", () => {
  it("returns empty string when maxBytes <= 0", () => {
    expect(truncateUtf8Buffer(buf("hello"), 0)).toBe("");
    expect(truncateUtf8Buffer(buf("hello"), -1)).toBe("");
  });

  it("returns full string when buf fits within maxBytes", () => {
    expect(truncateUtf8Buffer(buf("hello"), 10)).toBe("hello");
    expect(truncateUtf8Buffer(buf("hello"), 5)).toBe("hello");
  });

  it("truncates ASCII at exact byte boundary", () => {
    expect(truncateUtf8Buffer(buf("hello world"), 5)).toBe("hello");
  });

  it("does not split a 2-byte UTF-8 sequence across the boundary", () => {
    // "é" = 0xC3 0xA9 (2 bytes). Limit at 1 must exclude it entirely.
    const b = Buffer.concat([buf("a"), buf("é")]);
    expect(truncateUtf8Buffer(b, 1)).toBe("a");
    // Limit at 3 must include "é" (offset 1-2 fit within 3).
    expect(truncateUtf8Buffer(b, 3)).toBe("aé");
  });

  it("does not split a 3-byte UTF-8 sequence across the boundary", () => {
    // "€" = 0xE2 0x82 0xAC (3 bytes).
    const b = Buffer.concat([buf("ab"), buf("€")]);
    expect(truncateUtf8Buffer(b, 2)).toBe("ab");
    expect(truncateUtf8Buffer(b, 4)).toBe("ab");
    expect(truncateUtf8Buffer(b, 5)).toBe("ab€");
  });

  it("does not split a 4-byte UTF-8 sequence across the boundary", () => {
    // "😀" = 0xF0 0x9F 0x98 0x80 (4 bytes).
    const b = Buffer.concat([buf("a"), buf("😀")]);
    expect(truncateUtf8Buffer(b, 1)).toBe("a");
    expect(truncateUtf8Buffer(b, 3)).toBe("a");
    expect(truncateUtf8Buffer(b, 5)).toBe("a😀");
  });

  it("excludes overlong lead bytes (0xC0-0xC1)", () => {
    // 0xC0 followed by a continuation byte — overlong encoding.
    const b = bufHex("41C080"); // 'A' + overlong 0xC0 + continuation 0x80
    // maxBytes=2 stops mid-sequence; lead 0xC0 is invalid, excluded.
    const result = truncateUtf8Buffer(b, 2);
    expect(result).toBe("A");
  });

  it("walks back past orphaned continuation bytes after excluding an invalid lead byte", () => {
    // Buffer: 'a' (0x61), lone continuation 0x80, invalid overlong lead 0xC0, continuation 0x80.
    // At maxBytes=3 the walk-back stops at 0xC0 (not a continuation byte), detects it as
    // an invalid lead, and does end--. The 0x80 at position 1 is now orphaned at the end
    // of the slice; the second walk-back must exclude it so only 'a' is returned.
    const b = bufHex("6180C080");
    expect(truncateUtf8Buffer(b, 3)).toBe("a");
  });

  it("excludes out-of-range lead bytes (0xF5-0xFF)", () => {
    const b = bufHex("41F5808080"); // 'A' + invalid lead 0xF5 + continuations
    const result = truncateUtf8Buffer(b, 2);
    expect(result).toBe("A");
  });

  it("handles a lone continuation byte at truncation point", () => {
    // A lone continuation byte (0x80) at exactly maxBytes is walked back by the
    // while loop so the invalid byte is excluded from the truncated output.
    const b = bufHex("618062"); // 'a' + lone 0x80 + 'b'
    expect(truncateUtf8Buffer(b, 2)).toBe("a");
  });

  it("handles empty buffer", () => {
    expect(truncateUtf8Buffer(Buffer.alloc(0), 10)).toBe("");
  });

  it("handles buffer of exactly maxBytes with no multi-byte sequence", () => {
    expect(truncateUtf8Buffer(buf("hello"), 5)).toBe("hello");
  });
});

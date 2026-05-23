import { describe, it, expect } from 'vitest';
import { generateKey, hashKey } from './keys';

describe('keys.ts', () => {
  describe('generateKey', () => {
    it('should generate a key with the correct prefix format', () => {
      const { full, prefix } = generateKey('test_');

      expect(full.startsWith('test_live_')).toBe(true);
      expect(prefix).toBe(full.slice(0, 24));
    });

    it('should return a prefix of exactly 24 characters', () => {
      const { prefix } = generateKey('test_');
      expect(prefix.length).toBe(24);
    });

    it('should generate unique keys', () => {
      const key1 = generateKey('test_');
      const key2 = generateKey('test_');

      expect(key1.full).not.toBe(key2.full);
      expect(key1.prefix).not.toBe(key2.prefix);
    });

    it('should use the base32 alphabet (no padding)', () => {
      const { full } = generateKey('test_');
      const suffix = full.split('live_')[1];

      // Check that it only contains standard base32 characters
      expect(/^[A-Z2-7]+$/.test(suffix)).toBe(true);
    });
  });

  describe('hashKey', () => {
    it('should produce a deterministic hash', () => {
      const salt = 'salt123';
      const key = 'key123';

      const hash1 = hashKey(salt, key);
      const hash2 = hashKey(salt, key);

      expect(hash1).toEqual(hash2);
      expect(Buffer.isBuffer(hash1)).toBe(true);
    });

    it('should match the expected SHA-256 output byte-for-byte', () => {
      const salt = 'salt123';
      const key = 'key123';

      const hash = hashKey(salt, key);
      const expectedHex = '9ec4ffe0a7b0f743f094e60076c66e9560408c30ad11ac88a05bc98cedfa0f62';

      expect(hash.toString('hex')).toBe(expectedHex);
    });

    it('should correctly hash with empty salt', () => {
      const salt = '';
      const key = 'key123';

      const hash = hashKey(salt, key);
      const expectedHex = '8fefe692f690a3173176ecdff4318225afaeb97fdd6f60c866ed823d59221665'; // sha256("key123")

      expect(hash.toString('hex')).toBe(expectedHex);
    });
  });
});

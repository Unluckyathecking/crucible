import { describe, it, expect } from 'vitest';
import { generateKey, hashKey } from './keys';
import testData from '../testdata/keys.json';

describe('keys.ts', () => {
  describe('generateKey', () => {
    it('should generate a key with the correct prefix format using regex', () => {
      const { full, prefix } = generateKey('test_');

      // Match exactly the allowed prefix + 'live_' followed by base32
      expect(/^test_live_[A-Z2-7]+$/.test(full)).toBe(true);
      expect(prefix).toBe(full.slice(0, 24));
    });

    it('should return a prefix of exactly 24 characters', () => {
      const { prefix } = generateKey('test_');
      expect(prefix.length).toBe(24);
    });

    it('should generate unique keys and maintain collision resistance across 1000 iterations', () => {
      const generatedFullKeys = new Set<string>();
      const generatedPrefixes = new Set<string>();
      const ITERATIONS = 1000;

      for (let i = 0; i < ITERATIONS; i++) {
        const key = generateKey('test_');
        generatedFullKeys.add(key.full);
        generatedPrefixes.add(key.prefix);
      }

      expect(generatedFullKeys.size).toBe(ITERATIONS);
      expect(generatedPrefixes.size).toBe(ITERATIONS);
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

    it('should match the expected SHA-256 output byte-for-byte with the Go gateway implementation using testdata', () => {
      // The expected bytes are stable and identical to Go because the salt and keys are fixed in the test JSON structure.
      for (const tc of testData) {
        const hash = hashKey(tc.salt, tc.key);
        expect(hash.toString('hex')).toBe(tc.expectedHash);
      }
    });
  });
});

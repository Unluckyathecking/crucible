import { generateKey, hashKey } from './keys';

describe('keys', () => {
  describe('generateKey', () => {
    it('generates a key with the correct prefix and length', () => {
      const productPrefix = 'myprod_';
      const result = generateKey(productPrefix);

      expect(result).toHaveProperty('full');
      expect(result).toHaveProperty('prefix');

      // Check prefix
      expect(result.full.startsWith(`${productPrefix}live_`)).toBe(true);

      // Check prefix matches first 24 chars of full
      expect(result.prefix).toBe(result.full.slice(0, 24));

      // Check total length
      // prefix: myprod_live_ (12 chars)
      // suffix: 39 chars base32
      // total: 12 + 39 = 51 chars
      expect(result.full.length).toBe(productPrefix.length + 5 + 39);
    });

    it('generates unique keys', () => {
      const key1 = generateKey('test_');
      const key2 = generateKey('test_');
      expect(key1.full).not.toBe(key2.full);
    });

    it('generates keys with valid base32 characters', () => {
      const productPrefix = 'b32_';
      const result = generateKey(productPrefix);

      const expectedPrefix = `${productPrefix}live_`;
      const suffix = result.full.slice(expectedPrefix.length);

      // Standard base32 alphabet (no padding): A-Z and 2-7
      expect(suffix).toMatch(/^[A-Z2-7]+$/);
    });
  });

  describe('hashKey', () => {
    it('hashes correctly', () => {
      const salt = 'salt123';
      const key = 'testkey';
      const hash = hashKey(salt, key);

      expect(Buffer.isBuffer(hash)).toBe(true);
      expect(hash.length).toBe(32); // sha256 produces 32 bytes
    });
  });
});

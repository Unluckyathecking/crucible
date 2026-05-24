import { describe, it, expect, vi, beforeEach } from 'vitest';
import { POST } from '../route';
import { auth } from '@/auth';
import { ensureCustomer, insertApiKey } from '@/lib/db';
import { generateKey, hashKey } from '@/lib/keys';

vi.mock('@/auth', () => ({
  auth: vi.fn(),
}));

vi.mock('@/lib/db', () => ({
  ensureCustomer: vi.fn(),
  insertApiKey: vi.fn(),
}));

vi.mock('@/lib/keys', () => ({
  generateKey: vi.fn(),
  hashKey: vi.fn(),
}));

describe('POST /api/keys', () => {
  const mockSession = {
    user: { email: 'test@example.com' },
  };

  beforeEach(() => {
    vi.resetAllMocks();
    const VALID_SALT_LENGTH = 32;
    process.env.API_KEY_HASH_SALT = '0'.repeat(VALID_SALT_LENGTH);
    process.env.API_KEY_PREFIX = 'test_';

    (auth as any).mockResolvedValue(mockSession);
    (ensureCustomer as any).mockResolvedValue({ id: 'cust_123', email: 'test@example.com', plan_id: 'free' });
    (generateKey as any).mockReturnValue({ full: 'test_live_abc', prefix: 'test_live_abc' });
    (hashKey as any).mockReturnValue(Buffer.from('hash'));
  });

  it('should return 401 if unauthorized', async () => {
    (auth as any).mockResolvedValue(null);
    const req = new Request('http://localhost/api/keys', { method: 'POST', body: JSON.stringify({ name: 'test' }), headers: { 'content-type': 'application/json' } });
    const res = await POST(req);
    expect(res.status).toBe(401);
  });

  it('should return 500 if API_KEY_HASH_SALT is missing or short', async () => {
    process.env.API_KEY_HASH_SALT = 'short';
    const req = new Request('http://localhost/api/keys', { method: 'POST', body: JSON.stringify({ name: 'test' }), headers: { 'content-type': 'application/json' } });
    const res = await POST(req);
    expect(res.status).toBe(500);
    expect(await res.text()).toContain('API_KEY_HASH_SALT not configured');
  });

  it('should generate an API key successfully', async () => {
    const req = new Request('http://localhost/api/keys', { method: 'POST', body: JSON.stringify({ name: 'test key' }), headers: { 'content-type': 'application/json' } });
    const res = await POST(req);
    expect(res.status).toBe(200);
    expect(await res.text()).toContain('test_live_abc');
    expect(insertApiKey).toHaveBeenCalledTimes(1);
    expect(insertApiKey).toHaveBeenCalledWith('cust_123', 'test_live_abc', Buffer.from('hash'), 'test key');
  });

  it('should retry on Postgres unique_violation (23505) and succeed', async () => {
    (insertApiKey as any)
      .mockRejectedValueOnce({ code: '23505' })
      .mockResolvedValueOnce('key_123');

    (generateKey as any)
      .mockReturnValueOnce({ full: 'test_live_fail', prefix: 'test_live_fail' })
      .mockReturnValueOnce({ full: 'test_live_success', prefix: 'test_live_success' });

    const req = new Request('http://localhost/api/keys', { method: 'POST', body: JSON.stringify({ name: 'test key' }), headers: { 'content-type': 'application/json' } });
    const res = await POST(req);

    expect(res.status).toBe(200);
    expect(await res.text()).toContain('test_live_success');
    expect(insertApiKey).toHaveBeenCalledTimes(2);
    expect(generateKey).toHaveBeenCalledTimes(2);
  });

  it('should fail after 3 unsuccessful attempts', async () => {
    (insertApiKey as any).mockRejectedValue({ code: '23505' });

    const req = new Request('http://localhost/api/keys', { method: 'POST', body: JSON.stringify({ name: 'test key' }), headers: { 'content-type': 'application/json' } });
    const res = await POST(req);

    expect(res.status).toBe(500);
    expect(await res.text()).toContain('Failed to generate a unique key after 3 attempts');
    expect(insertApiKey).toHaveBeenCalledTimes(3);
    expect(generateKey).toHaveBeenCalledTimes(3);
  });

  it('should throw immediately on non-23505 errors', async () => {
    const customError = new Error('Database down');
    (customError as any).code = '50000';
    (insertApiKey as any).mockRejectedValue(customError);

    const req = new Request('http://localhost/api/keys', { method: 'POST', body: JSON.stringify({ name: 'test key' }), headers: { 'content-type': 'application/json' } });

    await expect(POST(req)).rejects.toThrow('Database down');
    expect(insertApiKey).toHaveBeenCalledTimes(1);
  });

  it('should accept name from FormData', async () => {
    const formData = new FormData();
    formData.append('name', 'form data key');
    const req = new Request('http://localhost/api/keys', { method: 'POST', body: formData });
    const res = await POST(req);
    expect(res.status).toBe(200);
    expect(insertApiKey).toHaveBeenCalledWith('cust_123', 'test_live_abc', Buffer.from('hash'), 'form data key');
  });

  it('should handle undefined name in FormData', async () => {
    const formData = new FormData();
    const req = new Request('http://localhost/api/keys', { method: 'POST', body: formData });
    const res = await POST(req);
    expect(res.status).toBe(200);
    expect(insertApiKey).toHaveBeenCalledWith('cust_123', 'test_live_abc', Buffer.from('hash'), '');
  });

  it('should handle undefined name in JSON body', async () => {
    const req = new Request('http://localhost/api/keys', { method: 'POST', body: JSON.stringify({}), headers: { 'content-type': 'application/json' } });
    const res = await POST(req);
    expect(res.status).toBe(200);
    expect(insertApiKey).toHaveBeenCalledWith('cust_123', 'test_live_abc', Buffer.from('hash'), '');
  });

  it('should reject names longer than 64 characters', async () => {
    const longName = 'a'.repeat(65);
    const req = new Request('http://localhost/api/keys', { method: 'POST', body: JSON.stringify({ name: longName }), headers: { 'content-type': 'application/json' } });
    const res = await POST(req);
    expect(res.status).toBe(400);
    expect(await res.text()).toContain('Name must be 64 characters or fewer');
  });
});
